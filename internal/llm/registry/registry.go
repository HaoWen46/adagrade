// Package registry resolves provider names to live LLM clients from the
// llm_providers table — providers are managed in the app UI, credentials stored
// encrypted (DECISIONS D11 v1 + D16). A small TTL cache plus explicit invalidation
// keeps resolution cheap; rate-limiter token buckets survive reloads so edits don't
// reset throttling state. It lives outside internal/llm because it imports the
// adapter packages, which import llm (cycle otherwise).
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/anthropiccompat"
	"github.com/HaoWen46/adagrade/internal/llm/openaicompat"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

const cacheTTL = 30 * time.Second

type entry struct {
	provider llm.Provider
	limiter  *rate.Limiter
	rps      float32
	burst    int32
	loadedAt time.Time
}

// DBSource implements llm.ProviderSource over the llm_providers table.
type DBSource struct {
	st  *store.Store
	key [32]byte

	mu    sync.Mutex
	cache map[string]*entry

	// testFetchHook, when set (tests only), runs synchronously just before the DB
	// fetch for the given provider name — used to simulate a stalled refresh and
	// prove it doesn't block concurrent lookups of other, already-cached providers.
	testFetchHook func(name string)
}

func NewDBSource(st *store.Store, key [32]byte) *DBSource {
	return &DBSource{st: st, key: key, cache: map[string]*entry{}}
}

// Invalidate forces reloads; provider handlers call it after any change. Limiter
// token buckets are kept so an edit doesn't reset in-flight throttling.
func (s *DBSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.cache {
		e.loadedAt = time.Time{}
	}
}

// Provider implements llm.ProviderSource. Only the cache check and the final
// install run under the lock; the DB fetch and BuildClient (decrypt + construct a
// wire client) happen unlocked, so one stalled/slow refresh never blocks every
// other worker's lookup of an unrelated (or already-cached) provider — including
// concurrent refreshes of the SAME name, which race benignly: both fetch and
// build outside the lock, and whichever re-acquires the lock last simply
// overwrites the other's (equivalent, freshly-loaded) entry.
func (s *DBSource) Provider(ctx context.Context, name string) (llm.Provider, *rate.Limiter, error) {
	s.mu.Lock()
	if e, ok := s.cache[name]; ok && time.Since(e.loadedAt) < cacheTTL {
		s.mu.Unlock()
		return e.provider, e.limiter, nil
	}
	s.mu.Unlock()

	if s.testFetchHook != nil {
		s.testFetchHook(name)
	}

	row, err := s.st.Q.GetProviderByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		s.mu.Lock()
		delete(s.cache, name)
		s.mu.Unlock()
		return nil, nil, &llm.ProviderUnavailableError{Name: name, Reason: "not configured — add it under Providers"}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("provider lookup: %w", err)
	}
	if !row.Enabled {
		s.mu.Lock()
		delete(s.cache, name)
		s.mu.Unlock()
		return nil, nil, &llm.ProviderUnavailableError{Name: name, Reason: "disabled — enable it under Providers"}
	}

	client, err := BuildClient(row, s.key)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.cache[name]
	if e == nil || e.rps != row.RequestsPerSecond || e.burst != row.Burst {
		e = &entry{limiter: rate.NewLimiter(rate.Limit(row.RequestsPerSecond), int(row.Burst))}
	}
	e.provider = client
	e.rps = row.RequestsPerSecond
	e.burst = row.Burst
	e.loadedAt = time.Now()
	s.cache[name] = e
	return e.provider, e.limiter, nil
}

// BuildClient decrypts the stored key and constructs the concrete client for a
// provider row (also used by the "Test" endpoint, bypassing the cache).
func BuildClient(row db.LlmProvider, key [32]byte) (llm.VerifiableProvider, error) {
	apiKey, err := secrets.Open(key, row.ApiKeyCiphertext)
	if err != nil {
		return nil, fmt.Errorf("provider %q: cannot decrypt stored API key (master key changed?): %w", row.Name, err)
	}
	switch row.Kind {
	case "anthropic-compat":
		return anthropiccompat.New(row.Name, row.BaseUrl, string(apiKey)), nil
	case "openai-compat":
		return openaicompat.New(row.Name, row.BaseUrl, string(apiKey)), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown kind %q", row.Name, row.Kind)
	}
}

// envSeedModels suggests vision-capable models for auto-imported env providers
// (openrouter list curated from the live catalog — see docs/MODELS.md).
var envSeedModels = map[string][]string{
	"qwen": {"qwen3-vl-plus"},
	"openrouter": {
		"qwen/qwen3.5-flash-02-23",
		"openai/gpt-5-nano",
		"google/gemini-3.1-flash-lite",
		"google/gemma-4-26b-a4b-it:free",
		"anthropic/claude-sonnet-5",
	},
}

// ImportEnvProviders inserts any env-detected provider whose NAME is not in the
// table yet ("paste the key in .env, restart" just works). It never updates an
// existing row — once a provider exists, the app UI owns it (D11 v1).
func ImportEnvProviders(ctx context.Context, st *store.Store, key [32]byte, envProviders []config.Provider, log *slog.Logger) error {
	if len(envProviders) == 0 {
		return nil
	}
	existing, err := st.Q.ListProviders(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, p := range existing {
		have[p.Name] = true
	}
	for _, p := range envProviders {
		if have[p.Name] {
			continue
		}
		sealed, err := secrets.Seal(key, []byte(p.APIKey))
		if err != nil {
			return err
		}
		models := envSeedModels[p.Name]
		if models == nil {
			models = []string{}
		}
		if _, err := st.Q.CreateProvider(ctx, db.CreateProviderParams{
			Name: p.Name, Kind: string(p.Kind), BaseUrl: p.BaseURL,
			ApiKeyCiphertext: sealed, ApiKeyHint: KeyHint(p.APIKey),
			Models: models, RequestsPerSecond: 1, Burst: 2,
		}); err != nil {
			return fmt.Errorf("import env provider %q: %w", p.Name, err)
		}
		log.Info("imported provider from environment into the app database", "provider", p.Name)
	}
	return nil
}

// KeyHint renders the displayable tail of a secret ("…ab12").
func KeyHint(apiKey string) string {
	if len(apiKey) <= 4 {
		return "…"
	}
	return "…" + apiKey[len(apiKey)-4:]
}

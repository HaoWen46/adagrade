package registry_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/llm/registry"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestImportEnvProviders_InsertsMissingNamesOnly(t *testing.T) {
	ctx := context.Background()
	st := storetest.Fresh(t)
	key, err := secrets.LoadOrCreateKey(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	env := []config.Provider{
		{Name: "qwen", Kind: config.ProviderKindAnthropicCompat, BaseURL: "https://q.example", APIKey: "sk-q-1"},
	}
	if err := registry.ImportEnvProviders(ctx, st, key, env, log); err != nil {
		t.Fatal(err)
	}

	// Second boot: qwen exists (must NOT be touched), openrouter is new (imported).
	env = []config.Provider{
		{Name: "qwen", Kind: config.ProviderKindAnthropicCompat, BaseURL: "https://q.example", APIKey: "sk-q-CHANGED"},
		{Name: "openrouter", Kind: config.ProviderKindOpenAICompat, BaseURL: "https://openrouter.ai/api/v1", APIKey: "sk-or-1"},
	}
	if err := registry.ImportEnvProviders(ctx, st, key, env, log); err != nil {
		t.Fatal(err)
	}

	rows, err := st.Q.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("providers: got %d want 2", len(rows))
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
	}

	or := rows[byName["openrouter"]]
	if or.Kind != "openai-compat" || len(or.Models) == 0 || or.ApiKeyHint != "…or-1" {
		t.Errorf("openrouter row: kind=%s models=%d hint=%s", or.Kind, len(or.Models), or.ApiKeyHint)
	}
	plain, err := secrets.Open(key, or.ApiKeyCiphertext)
	if err != nil || string(plain) != "sk-or-1" {
		t.Errorf("openrouter key roundtrip: %q err=%v", plain, err)
	}

	// qwen kept its ORIGINAL key — env changes never override app-owned rows.
	qw := rows[byName["qwen"]]
	plain, err = secrets.Open(key, qw.ApiKeyCiphertext)
	if err != nil || string(plain) != "sk-q-1" {
		t.Errorf("qwen key must be untouched: %q err=%v", plain, err)
	}
}

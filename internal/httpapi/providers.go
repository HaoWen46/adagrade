package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/llm/registry"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Providers are managed entirely in the app (D11 v1): rows carry the base URL,
// model suggestions, and rate limits; the API key is stored encrypted and only its
// tail is ever sent back to a browser.

var providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

type providerJSON struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Kind              string     `json:"kind"`
	BaseURL           string     `json:"base_url"`
	APIKeyHint        string     `json:"api_key_hint"`
	Models            []string   `json:"models"`
	RequestsPerSecond float32    `json:"requests_per_second"`
	Burst             int32      `json:"burst"`
	Enabled           bool       `json:"enabled"`
	LastVerifiedAt    *time.Time `json:"last_verified_at,omitempty"`
}

func toProviderJSON(p db.LlmProvider) providerJSON {
	return providerJSON{
		ID: p.ID, Name: p.Name, Kind: p.Kind, BaseURL: p.BaseUrl,
		APIKeyHint: p.ApiKeyHint, Models: p.Models,
		RequestsPerSecond: p.RequestsPerSecond, Burst: p.Burst,
		Enabled: p.Enabled, LastVerifiedAt: tsPtr(p.LastVerifiedAt),
	}
}

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Q.ListProviders(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]providerJSON, 0, len(rows))
	for _, p := range rows {
		out = append(out, toProviderJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func validBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name              string   `json:"name"`
		Kind              string   `json:"kind"`
		BaseURL           string   `json:"base_url"`
		APIKey            string   `json:"api_key"`
		Models            []string `json:"models"`
		RequestsPerSecond float32  `json:"requests_per_second"`
		Burst             int32    `json:"burst"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if body.Kind == "" {
		body.Kind = "anthropic-compat"
	}
	switch {
	case !providerNameRe.MatchString(body.Name):
		apiError(w, http.StatusBadRequest, "name must be a short lowercase slug (letters, digits, dashes)")
		return
	case body.Kind != "anthropic-compat" && body.Kind != "openai-compat":
		apiError(w, http.StatusBadRequest, "kind must be anthropic-compat or openai-compat")
		return
	case !validBaseURL(body.BaseURL):
		apiError(w, http.StatusBadRequest, "base_url must be a valid http(s) URL")
		return
	case strings.TrimSpace(body.APIKey) == "":
		apiError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	if body.RequestsPerSecond <= 0 {
		body.RequestsPerSecond = 1
	}
	if body.Burst < 1 {
		body.Burst = 2
	}
	if body.Models == nil {
		body.Models = []string{}
	}

	sealed, err := secrets.Seal(s.secretKey, []byte(strings.TrimSpace(body.APIKey)))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "encrypting the key failed")
		return
	}
	p, err := s.store.Q.CreateProvider(r.Context(), db.CreateProviderParams{
		Name: body.Name, Kind: body.Kind,
		BaseUrl:          strings.TrimRight(body.BaseURL, "/"),
		ApiKeyCiphertext: sealed, ApiKeyHint: registry.KeyHint(strings.TrimSpace(body.APIKey)),
		Models:            body.Models,
		RequestsPerSecond: body.RequestsPerSecond, Burst: body.Burst,
	})
	if err != nil {
		apiError(w, http.StatusConflict, "a provider with that name already exists")
		return
	}
	s.providers.Invalidate()
	s.seedAfterProviderChange(r.Context())
	s.audit(r, "provider.create", "provider", strconv.FormatInt(p.ID, 10), map[string]any{"name": p.Name})
	writeJSON(w, http.StatusCreated, toProviderJSON(p))
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	existing, err := s.store.Q.GetProvider(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such provider")
		return
	}
	var body struct {
		BaseURL           *string  `json:"base_url"`
		APIKey            *string  `json:"api_key"` // omit/empty = keep current
		Models            []string `json:"models"`
		RequestsPerSecond *float32 `json:"requests_per_second"`
		Burst             *int32   `json:"burst"`
		Enabled           *bool    `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := db.UpdateProviderParams{
		ID:                id,
		BaseUrl:           existing.BaseUrl,
		ApiKeyCiphertext:  existing.ApiKeyCiphertext,
		ApiKeyHint:        existing.ApiKeyHint,
		Models:            existing.Models,
		RequestsPerSecond: existing.RequestsPerSecond,
		Burst:             existing.Burst,
		Enabled:           existing.Enabled,
	}
	if body.BaseURL != nil {
		if !validBaseURL(*body.BaseURL) {
			apiError(w, http.StatusBadRequest, "base_url must be a valid http(s) URL")
			return
		}
		params.BaseUrl = strings.TrimRight(*body.BaseURL, "/")
	}
	if body.APIKey != nil && strings.TrimSpace(*body.APIKey) != "" {
		key := strings.TrimSpace(*body.APIKey)
		sealed, err := secrets.Seal(s.secretKey, []byte(key))
		if err != nil {
			apiError(w, http.StatusInternalServerError, "encrypting the key failed")
			return
		}
		params.ApiKeyCiphertext = sealed
		params.ApiKeyHint = registry.KeyHint(key)
	}
	if body.Models != nil {
		params.Models = body.Models
	}
	if body.RequestsPerSecond != nil {
		if *body.RequestsPerSecond <= 0 {
			apiError(w, http.StatusBadRequest, "requests_per_second must be positive")
			return
		}
		params.RequestsPerSecond = *body.RequestsPerSecond
	}
	if body.Burst != nil {
		if *body.Burst < 1 {
			apiError(w, http.StatusBadRequest, "burst must be at least 1")
			return
		}
		params.Burst = *body.Burst
	}
	if body.Enabled != nil {
		params.Enabled = *body.Enabled
	}

	p, err := s.store.Q.UpdateProvider(r.Context(), params)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "update failed")
		return
	}
	s.providers.Invalidate()
	s.seedAfterProviderChange(r.Context())
	s.audit(r, "provider.update", "provider", strconv.FormatInt(id, 10), map[string]any{"enabled": p.Enabled})
	writeJSON(w, http.StatusOK, toProviderJSON(p))
}

// seedAfterProviderChange re-runs the idempotent startup seeding after a provider is
// created or updated, so enabling the first vision-capable provider immediately yields
// the default grading method instead of waiting for a restart. Best-effort: seeding
// failures never fail the provider request itself.
func (s *Server) seedAfterProviderChange(ctx context.Context) {
	if err := grading.EnsureSeeds(ctx, s.store, s.log); err != nil {
		s.log.Warn("seed after provider change failed", "err", err)
	}
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	p, err := s.store.Q.GetProvider(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such provider")
		return
	}
	refs, err := s.store.Q.CountMethodVersionsUsingProvider(r.Context(), p.Name)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "reference check failed")
		return
	}
	if refs > 0 {
		apiError(w, http.StatusConflict, "grading methods reference this provider — disable it instead of deleting (records keep their history)")
		return
	}
	if err := s.store.Q.DeleteProvider(r.Context(), id); err != nil {
		apiError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	s.providers.Invalidate()
	s.audit(r, "provider.delete", "provider", strconv.FormatInt(id, 10), map[string]any{"name": p.Name})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestProvider verifies base URL + key with a live call: model listing when
// the endpoint supports it (also refreshing suggestions), else a 1-token ping
// against the given model.
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	row, err := s.store.Q.GetProvider(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such provider")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}

	client, err := registry.BuildClient(row, s.secretKey)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	models, listErr := client.ListModels(ctx)
	if listErr == nil {
		// Persist discovered models as picker suggestions — but aggregators like
		// OpenRouter return hundreds; beyond the cap, keep the row's curated list
		// and only return the catalog for this one response.
		const persistCap = 50
		if len(models) > 0 && len(models) <= persistCap {
			merged := mergeModels(row.Models, models)
			if err := s.store.Q.SetProviderModels(ctx, db.SetProviderModelsParams{ID: id, Models: merged}); err == nil {
				s.providers.Invalidate()
			}
		}
		_ = s.store.Q.MarkProviderVerified(ctx, id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
		return
	}

	// No model listing on this endpoint — fall back to a minimal real call.
	model := strings.TrimSpace(body.Model)
	if model == "" && len(row.Models) > 0 {
		model = row.Models[0]
	}
	if model == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": "this endpoint does not list models; enter a model id to test with (e.g. qwen3-vl-plus)",
		})
		return
	}
	if err := client.Ping(ctx, model); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = s.store.Q.MarkProviderVerified(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": []string{}, "tested_model": model})
}

func mergeModels(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, m := range list {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// --- model pricing (trust spec §2, D35) -----------------------------------------
//
// Pricing is operator-entered $/Mtok data, edited on the Providers page — never
// seeded from MODELS.md. Prices travel as decimal strings end-to-end; the store
// layer does the string<->NUMERIC conversion (never float64).

type pricingJSON struct {
	Model            string `json:"model"`
	InputUSDPerMtok  string `json:"input_usd_per_mtok"`
	OutputUSDPerMtok string `json:"output_usd_per_mtok"`
}

func toPricingJSON(p db.ModelPricing) pricingJSON {
	return pricingJSON{
		Model:            p.Model,
		InputUSDPerMtok:  store.NumStr(p.InputUsdPerMtok),
		OutputUSDPerMtok: store.NumStr(p.OutputUsdPerMtok),
	}
}

// handleListPricing returns every pricing row for one provider.
func (s *Server) handleListPricing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	if _, err := s.store.Q.GetProvider(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such provider")
		return
	}
	rows, err := s.store.ListModelPricing(r.Context(), id)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]pricingJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, toPricingJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"pricing": out})
}

// handlePutPricing upserts one (provider, model) pricing row. Editing pricing only
// affects cost_usd computed for FUTURE grading_records — no historical backfill
// (trust spec §2, flagged decision): a run in flight keeps whatever price was in
// effect when each of its records was inserted.
func (s *Server) handlePutPricing(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		apiError(w, http.StatusBadRequest, "invalid provider id")
		return
	}
	if _, err := s.store.Q.GetProvider(r.Context(), id); err != nil {
		apiError(w, http.StatusNotFound, "no such provider")
		return
	}
	var body struct {
		Model            string `json:"model"`
		InputUSDPerMtok  string `json:"input_usd_per_mtok"`
		OutputUSDPerMtok string `json:"output_usd_per_mtok"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Model = strings.TrimSpace(body.Model)
	if body.Model == "" {
		apiError(w, http.StatusBadRequest, "model is required")
		return
	}
	row, err := s.store.UpsertModelPricing(r.Context(), id, body.Model, body.InputUSDPerMtok, body.OutputUSDPerMtok)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid pricing: "+err.Error())
		return
	}
	s.audit(r, "provider.pricing.set", "provider", strconv.FormatInt(id, 10), map[string]any{
		"model": body.Model, "input_usd_per_mtok": body.InputUSDPerMtok, "output_usd_per_mtok": body.OutputUSDPerMtok,
	})
	writeJSON(w, http.StatusOK, toPricingJSON(row))
}

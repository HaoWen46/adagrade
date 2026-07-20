package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// fakeCompatAPI is a minimal Anthropic-compatible endpoint: model listing on/off.
func fakeCompatAPI(t *testing.T, withModels bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-live-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		if !withModels {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"vision-a"},{"id":"vision-b"}]}`))
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-live-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProviders_CRUDAndSecrets(t *testing.T) {
	ts, _, st := harness(t)
	lect := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, ts, st, "ta@ntu.edu.tw", "ta")

	// TA cannot create providers.
	resp := postJSON(t, ta, ts.URL+"/api/providers", map[string]any{
		"name": "qwen", "base_url": "https://example.com/anthropic", "api_key": "sk-secret-12345",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta create provider: got %d want 403", resp.StatusCode)
	}

	// Lecturer creates one; response carries only the key hint.
	created := postExpect(t, lect, ts.URL+"/api/providers", map[string]any{
		"name": "qwen", "base_url": "https://example.com/anthropic/", "api_key": "sk-secret-12345",
		"models": []string{"qwen3-vl-plus"},
	}, http.StatusCreated)
	pid := int64(created["id"].(float64))
	if created["api_key_hint"] != "…2345" {
		t.Errorf("key hint: %v", created["api_key_hint"])
	}
	if created["base_url"] != "https://example.com/anthropic" {
		t.Errorf("base_url not normalized: %v", created["base_url"])
	}
	raw, _ := json.Marshal(created)
	if strings.Contains(string(raw), "sk-secret-12345") {
		t.Fatal("plaintext key leaked into the response")
	}

	// The key is not plaintext in the database either.
	var ct []byte
	if err := st.Pool.QueryRow(t.Context(), "SELECT api_key_ciphertext FROM llm_providers WHERE id = $1", pid).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "sk-secret-12345") {
		t.Fatal("plaintext key stored in the database")
	}

	// Invalid inputs rejected.
	for _, bad := range []map[string]any{
		{"name": "Bad Name!", "base_url": "https://x.com", "api_key": "k"},
		{"name": "ok", "base_url": "not-a-url", "api_key": "k"},
		{"name": "ok2", "base_url": "https://x.com", "api_key": "  "},
	} {
		r := postJSON(t, lect, ts.URL+"/api/providers", bad)
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("bad provider %v: got %d want 400", bad, r.StatusCode)
		}
	}

	// TA can list (method editor needs it) — still no plaintext.
	listed := getJSON[map[string][]map[string]any](t, ta, ts.URL+"/api/providers", http.StatusOK)
	if len(listed["providers"]) != 1 || listed["providers"][0]["api_key_hint"] != "…2345" {
		t.Errorf("list: %v", listed)
	}

	// Update: disable + rotate key.
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/providers/%d", ts.URL, pid),
		strings.NewReader(`{"enabled": false, "api_key": "sk-rotated-9999"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	uresp, err := lect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]any
	_ = json.NewDecoder(uresp.Body).Decode(&updated)
	uresp.Body.Close()
	if updated["enabled"] != false || updated["api_key_hint"] != "…9999" {
		t.Errorf("update: %v", updated)
	}

	// Delete is blocked once a method references the provider.
	seedTemplateAndMethod(t, ts, st, lect, "qwen")
	dreq, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/providers/%d", ts.URL, pid), strings.NewReader("{}"))
	dreq.Header.Set("X-ADA-CSRF", "1")
	dreq.Header.Set("Content-Type", "application/json")
	dresp, _ := lect.Do(dreq)
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusConflict {
		t.Errorf("delete referenced provider: got %d want 409", dresp.StatusCode)
	}
}

func TestProviders_CreateSeedsDefaultMethod(t *testing.T) {
	ts, _, st := harness(t)
	lect := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	before := getJSON[map[string][]map[string]any](t, lect, ts.URL+"/api/methods", http.StatusOK)
	if len(before["methods"]) != 0 {
		t.Fatalf("fresh DB should have no methods: %v", before["methods"])
	}

	// Creating an enabled provider with a seed-recognized name yields the default method.
	postExpect(t, lect, ts.URL+"/api/providers", map[string]any{
		"name": "qwen", "base_url": "https://example.com/anthropic", "api_key": "sk-secret-12345",
	}, http.StatusCreated)
	after := getJSON[map[string][]map[string]any](t, lect, ts.URL+"/api/methods", http.StatusOK)
	if len(after["methods"]) != 1 || after["methods"][0]["name"] != "Default — qwen3-vl-plus" {
		t.Fatalf("methods after qwen create: %v", after["methods"])
	}

	// A second provider does not add more methods.
	postExpect(t, lect, ts.URL+"/api/providers", map[string]any{
		"name": "anthropic", "base_url": "https://example.com/anthropic", "api_key": "sk-secret-67890",
	}, http.StatusCreated)
	again := getJSON[map[string][]map[string]any](t, lect, ts.URL+"/api/methods", http.StatusOK)
	if len(again["methods"]) != 1 {
		t.Fatalf("second provider should not add methods: %v", again["methods"])
	}
}

func TestProviders_EnableSeedsDefaultMethod(t *testing.T) {
	ts, _, st := harness(t)
	lect := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	// A disabled qwen provider inserted outside the API: no seed has happened yet.
	p, err := st.Q.CreateProvider(t.Context(), db.CreateProviderParams{
		Name: "qwen", Kind: "anthropic-compat", BaseUrl: "https://example.com/anthropic",
		ApiKeyCiphertext: []byte("sealed"), ApiKeyHint: "…2345",
		Models: []string{}, RequestsPerSecond: 1, Burst: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, st, "UPDATE llm_providers SET enabled = FALSE WHERE id = $1", p.ID)
	before := getJSON[map[string][]map[string]any](t, lect, ts.URL+"/api/methods", http.StatusOK)
	if len(before["methods"]) != 0 {
		t.Fatalf("no methods expected before enabling: %v", before["methods"])
	}

	// Enabling it through the API seeds the default method.
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/providers/%d", ts.URL, p.ID),
		strings.NewReader(`{"enabled": true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := lect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable provider: got %d want 200", resp.StatusCode)
	}
	after := getJSON[map[string][]map[string]any](t, lect, ts.URL+"/api/methods", http.StatusOK)
	if len(after["methods"]) != 1 || after["methods"][0]["name"] != "Default — qwen3-vl-plus" {
		t.Fatalf("methods after enable: %v", after["methods"])
	}
}

// seedTemplateAndMethod creates the default template + a method on the named provider.
func seedTemplateAndMethod(t *testing.T, ts *httptest.Server, st storeSeeder, c *http.Client, provider string) int64 {
	t.Helper()
	if err := grading.EnsureSeeds(t.Context(), st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, ts.URL+"/api/methods", map[string]any{
		"name": "On " + provider,
		"config": map[string]any{
			"provider": provider, "model": "some-model",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	return int64(m["id"].(float64))
}

func TestProviders_TestEndpoint(t *testing.T) {
	env := harnessEnv(t)
	lect := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")

	// Endpoint WITH model listing: ok + models merged into suggestions.
	api := fakeCompatAPI(t, true)
	created := postExpect(t, lect, env.ts.URL+"/api/providers", map[string]any{
		"name": "listing", "base_url": api.URL, "api_key": "sk-live-test",
	}, http.StatusCreated)
	pid := int64(created["id"].(float64))

	res := postExpect(t, lect, fmt.Sprintf("%s/api/providers/%d/test", env.ts.URL, pid), map[string]any{}, http.StatusOK)
	if res["ok"] != true {
		t.Fatalf("test: %v", res)
	}
	listed := getJSON[map[string][]map[string]any](t, lect, env.ts.URL+"/api/providers", http.StatusOK)
	var models []any
	for _, p := range listed["providers"] {
		if p["name"] == "listing" {
			models = p["models"].([]any)
			if p["last_verified_at"] == nil {
				t.Error("last_verified_at not set after successful test")
			}
		}
	}
	if len(models) != 2 {
		t.Errorf("models not merged: %v", models)
	}

	// Wrong key: ok=false with the server's message surfaced.
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/providers/%d", env.ts.URL, pid), strings.NewReader(`{"api_key":"sk-wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	r2, _ := lect.Do(req)
	r2.Body.Close()
	res = postExpect(t, lect, fmt.Sprintf("%s/api/providers/%d/test", env.ts.URL, pid), map[string]any{"model": "vision-a"}, http.StatusOK)
	if res["ok"] != false || res["error"] == nil {
		t.Fatalf("bad-key test should be ok=false with error: %v", res)
	}

	// Endpoint WITHOUT model listing: needs a model, then pings.
	api2 := fakeCompatAPI(t, false)
	created2 := postExpect(t, lect, env.ts.URL+"/api/providers", map[string]any{
		"name": "pinger", "base_url": api2.URL, "api_key": "sk-live-test",
	}, http.StatusCreated)
	pid2 := int64(created2["id"].(float64))

	res = postExpect(t, lect, fmt.Sprintf("%s/api/providers/%d/test", env.ts.URL, pid2), map[string]any{}, http.StatusOK)
	if res["ok"] != false || !strings.Contains(res["error"].(string), "model") {
		t.Fatalf("no-model test should ask for a model id: %v", res)
	}
	res = postExpect(t, lect, fmt.Sprintf("%s/api/providers/%d/test", env.ts.URL, pid2), map[string]any{"model": "any-model"}, http.StatusOK)
	if res["ok"] != true || res["tested_model"] != "any-model" {
		t.Fatalf("ping test: %v", res)
	}
}

// fakeOpenAIAPI is a minimal OpenAI-compatible endpoint (what OpenRouter speaks).
func fakeOpenAIAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-or-live" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"google/gemini-2.5-flash"},{"id":"qwen/qwen3-vl-plus"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProviders_OpenAICompatKind(t *testing.T) {
	env := harnessEnv(t)
	lect := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")

	// Unknown kinds are still rejected.
	resp := postJSON(t, lect, env.ts.URL+"/api/providers", map[string]any{
		"name": "weird", "kind": "grpc", "base_url": "https://x.com", "api_key": "k",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind: got %d want 400", resp.StatusCode)
	}

	api := fakeOpenAIAPI(t)
	created := postExpect(t, lect, env.ts.URL+"/api/providers", map[string]any{
		"name": "openrouter", "kind": "openai-compat", "base_url": api.URL, "api_key": "sk-or-live",
	}, http.StatusCreated)
	if created["kind"] != "openai-compat" {
		t.Fatalf("kind: %v", created["kind"])
	}
	pid := int64(created["id"].(float64))

	res := postExpect(t, lect, fmt.Sprintf("%s/api/providers/%d/test", env.ts.URL, pid), map[string]any{}, http.StatusOK)
	if res["ok"] != true || len(res["models"].([]any)) != 2 {
		t.Fatalf("openai-compat test: %v", res)
	}
}

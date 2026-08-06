package offline

import (
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm/anthropiccompat"
	"github.com/HaoWen46/adagrade/internal/llm/openaicompat"
)

// env turns a map into the getenv function BuildProvider takes, so no test
// mutates the process environment (and none can leak a key into another test).
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// The secret every message in this file must never contain. Provider errors are
// printed to a terminal and pasted into bug reports.
const fixtureKey = "sk-secret-value-do-not-print"

func assertNoKeyLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), fixtureKey) {
		t.Errorf("error message contains the API KEY itself: %v", err)
	}
}

// --- route 1: --base-url ---------------------------------------------------

// TestBuildProvider_BaseURLDefaultsToAnthropicCompat — --provider-kind is
// optional, and the default matches config's own default for a declared
// provider.
func TestBuildProvider_BaseURLDefaultsToAnthropicCompat(t *testing.T) {
	o := Options{BaseURL: "https://api.example.test/anthropic", APIKeyEnv: "ADA_KEY", Model: "vision-1"}
	p, model, err := BuildProvider(o, env(map[string]string{"ADA_KEY": fixtureKey}))
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if _, ok := p.(*anthropiccompat.Client); !ok {
		t.Errorf("provider = %T, want *anthropiccompat.Client", p)
	}
	if model != "vision-1" {
		t.Errorf("model = %q, want %q", model, "vision-1")
	}
	if p.Name() == "" {
		t.Error("provider name is empty: it appears in every error the run reports")
	}
}

// TestBuildProvider_BaseURLHonoursTheProviderKind — the dialect flag is the
// only way a --base-url run can reach the OpenAI wire shape.
func TestBuildProvider_BaseURLHonoursTheProviderKind(t *testing.T) {
	o := Options{
		BaseURL:      "https://gateway.example.test/v1",
		ProviderKind: ProviderKindOpenAICompat,
		APIKeyEnv:    "ADA_KEY",
		Model:        "vision-1",
	}
	p, _, err := BuildProvider(o, env(map[string]string{"ADA_KEY": fixtureKey}))
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if _, ok := p.(*openaicompat.Client); !ok {
		t.Errorf("provider = %T, want *openaicompat.Client", p)
	}
}

// TestBuildProvider_MissingKeyNamesTheEnvVar — the operator's fix is "export
// THAT variable", so the message has to say which one. The key itself is never
// an argument and never in a message.
func TestBuildProvider_MissingKeyNamesTheEnvVar(t *testing.T) {
	o := Options{BaseURL: "https://api.example.test", APIKeyEnv: "ADA_KEY", Model: "vision-1"}
	_, _, err := BuildProvider(o, env(nil))
	assertErrorType[*ProviderError](t, err, "ADA_KEY")
	if code := ExitCode(err); code != ExitProvider {
		t.Errorf("ExitCode = %d, want %d", code, ExitProvider)
	}
}

// TestBuildProvider_BaseURLMustBeAnAbsoluteHTTPURL — a typo'd URL otherwise
// fails once per page, deep inside the transcription stage, as an HTTP error.
func TestBuildProvider_BaseURLMustBeAnAbsoluteHTTPURL(t *testing.T) {
	for _, bad := range []string{"api.example.test", "ftp://api.example.test", "https://"} {
		o := Options{BaseURL: bad, APIKeyEnv: "ADA_KEY", Model: "vision-1"}
		_, _, err := BuildProvider(o, env(map[string]string{"ADA_KEY": fixtureKey}))
		assertErrorType[*ProviderError](t, err, "--base-url")
		assertNoKeyLeak(t, err)
	}
}

// --- route 2: --provider ---------------------------------------------------

// providerTableEnv declares two providers of different kinds through the
// documented ADAMARKER_PROVIDERS variables.
func providerTableEnv() map[string]string {
	return map[string]string{
		"ADAMARKER_PROVIDERS":               "alpha,beta",
		"ADAMARKER_PROVIDER_ALPHA_KIND":     "anthropic-compat",
		"ADAMARKER_PROVIDER_ALPHA_BASE_URL": "https://alpha.example.test",
		"ADAMARKER_PROVIDER_ALPHA_API_KEY":  fixtureKey,
		"ADAMARKER_PROVIDER_BETA_KIND":      "openai-compat",
		"ADAMARKER_PROVIDER_BETA_BASE_URL":  "https://beta.example.test/v1",
		"ADAMARKER_PROVIDER_BETA_API_KEY":   fixtureKey,
	}
}

// TestBuildProvider_TableLookupBuildsTheRowsKind — the table row, not a flag,
// decides the dialect on this route.
func TestBuildProvider_TableLookupBuildsTheRowsKind(t *testing.T) {
	tests := map[string]func(any) bool{
		"alpha": func(p any) bool { _, ok := p.(*anthropiccompat.Client); return ok },
		"beta":  func(p any) bool { _, ok := p.(*openaicompat.Client); return ok },
	}
	for name, isRightKind := range tests {
		p, model, err := BuildProvider(Options{Provider: name, Model: "vision-1"}, env(providerTableEnv()))
		if err != nil {
			t.Fatalf("BuildProvider(%s): %v", name, err)
		}
		if !isRightKind(p) {
			t.Errorf("provider %s = %T, wrong client kind", name, p)
		}
		if p.Name() != name {
			t.Errorf("provider name = %q, want %q", p.Name(), name)
		}
		if model != "vision-1" {
			t.Errorf("model = %q, want %q", model, "vision-1")
		}
	}
}

// TestBuildProvider_AutoDetectedVendorKeyIsInTheTable — pasting a vendor key in
// .env is the documented "just works" path, and it must work here too.
func TestBuildProvider_AutoDetectedVendorKeyIsInTheTable(t *testing.T) {
	p, _, err := BuildProvider(
		Options{Provider: "openrouter", Model: "vision-1"},
		env(map[string]string{"OPENROUTER_API_KEY": fixtureKey}),
	)
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if _, ok := p.(*openaicompat.Client); !ok {
		t.Errorf("provider = %T, want *openaicompat.Client", p)
	}
}

// TestBuildProvider_UnknownNameListsTheConfiguredOnes — "provider not found" is
// unfixable advice when the operator cannot see what IS configured.
func TestBuildProvider_UnknownNameListsTheConfiguredOnes(t *testing.T) {
	_, _, err := BuildProvider(Options{Provider: "gamma", Model: "vision-1"}, env(providerTableEnv()))
	assertErrorType[*ProviderError](t, err, "gamma", "alpha", "beta")
	assertNoKeyLeak(t, err)
}

// TestBuildProvider_EmptyTableSaysWhichEnvVarsDefineOne — with nothing
// configured there is no list to print, so the message has to name the
// variables that would create one.
func TestBuildProvider_EmptyTableSaysWhichEnvVarsDefineOne(t *testing.T) {
	_, _, err := BuildProvider(Options{Provider: "alpha", Model: "vision-1"}, env(nil))
	assertErrorType[*ProviderError](t, err, "ADAMARKER_PROVIDERS", "OPENROUTER_API_KEY", "--base-url")
}

// TestBuildProvider_BrokenTableIsAProviderError — a half-declared provider is a
// configuration problem (exit 8), not an unclassified crash.
func TestBuildProvider_BrokenTableIsAProviderError(t *testing.T) {
	_, _, err := BuildProvider(Options{Provider: "alpha", Model: "vision-1"}, env(map[string]string{
		"ADAMARKER_PROVIDERS":               "alpha",
		"ADAMARKER_PROVIDER_ALPHA_BASE_URL": "https://alpha.example.test",
		// no _API_KEY
	}))
	assertErrorType[*ProviderError](t, err)
	if code := ExitCode(err); code != ExitProvider {
		t.Errorf("ExitCode = %d, want %d", code, ExitProvider)
	}
	assertNoKeyLeak(t, err)
}

// TestBuildProvider_NoRouteAtAll — ParseArgs already refuses this combination
// when the API stage runs; the stage still refuses to invent a provider.
func TestBuildProvider_NoRouteAtAll(t *testing.T) {
	_, _, err := BuildProvider(Options{Model: "vision-1"}, env(providerTableEnv()))
	assertErrorType[*ProviderError](t, err, "--provider", "--base-url")
}

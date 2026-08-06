package offline

import (
	"net/url"
	"sort"
	"strings"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/anthropiccompat"
	"github.com/HaoWen46/adagrade/internal/llm/openaicompat"
)

// providerEnvHint names the variables that define a provider table. It is
// printed whenever a lookup fails, because "no such provider" without it leaves
// the operator with nothing to change.
const providerEnvHint = `configure one with ADAMARKER_PROVIDERS=name plus ADAMARKER_PROVIDER_<NAME>_{KIND,BASE_URL,API_KEY}, ` +
	`or paste a vendor key (DEEPSEEK_API_KEY, QWEN_API_KEY, OPENROUTER_API_KEY), ` +
	`or bypass the table entirely with --base-url URL --api-key-env NAME`

// BuildProvider constructs the LLM client the transcription stage will call,
// and returns it with the model id to call it with.
//
// There is no database in this mode, so the server's app-managed provider
// registry (D11 v1) is unreachable. The two routes here are the offline
// substitutes, and ParseArgs has already guaranteed they are mutually
// exclusive:
//
//   - --base-url (with --api-key-env): construct a client directly. --provider-kind
//     picks the dialect and defaults to anthropic-compat, matching config's own
//     default for a declared provider.
//   - --provider NAME: look NAME up in the environment provider table
//     (config.LoadProviders), and construct whatever kind that row declares.
//
// The API KEY is never an argument on either route — only the NAME of the
// variable holding it — so a key cannot end up in a shell history, a process
// listing, or a screenshot of the command line.
//
// Every failure is a *ProviderError (exit 8): all of them are configuration
// problems, and all of them are fixed before the run rather than during it.
// No network call is made here; a wrong URL or a revoked key surfaces on the
// first transcription call.
func BuildProvider(o Options, getenv func(string) string) (llm.Provider, string, error) {
	switch {
	case o.BaseURL != "":
		p, err := buildFromBaseURL(o, getenv)
		return p, o.Model, err
	case o.Provider != "":
		p, err := buildFromTable(o, getenv)
		return p, o.Model, err
	}
	return nil, "", newProviderError(nil,
		"no provider to transcribe with: pass --provider NAME or --base-url URL (with --api-key-env NAME), or --stop-after match|mask to skip the API stage")
}

// buildFromBaseURL is the direct route: the operator has a URL and a key, and
// wants neither a table nor a database in the way.
func buildFromBaseURL(o Options, getenv func(string) string) (llm.Provider, error) {
	if err := checkBaseURL(o.BaseURL); err != nil {
		return nil, err
	}
	// APIKeyEnv is guaranteed non-empty alongside --base-url by ParseArgs.
	key := strings.TrimSpace(getenv(o.APIKeyEnv))
	if key == "" {
		return nil, newProviderError(nil,
			"environment variable %s is empty or unset, but --api-key-env names it as the API key for --base-url %s: export it (never pass the key itself as an argument)",
			o.APIKeyEnv, o.BaseURL)
	}
	kind := o.ProviderKind
	if kind == "" {
		kind = ProviderKindAnthropicCompat
	}
	return newClient(providerDisplayName(o.BaseURL), kind, o.BaseURL, key)
}

// checkBaseURL rejects a URL that cannot address an API before the run spends a
// page on finding out. A relative or non-HTTP URL is a typo every time.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return newProviderError(err, "--base-url %s is not a valid URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return newProviderError(nil, "--base-url %s must be an absolute http:// or https:// URL", raw)
	}
	if u.Host == "" {
		return newProviderError(nil, "--base-url %s has no host: pass the API's base address, e.g. https://api.example.com/v1", raw)
	}
	return nil
}

// providerDisplayName labels the direct-route client for messages. The host is
// what an operator recognizes; it is not a secret (the key is, and is not here).
func providerDisplayName(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "base-url"
}

// buildFromTable is the named route: resolve NAME through the same env-only
// provider table the server seeds itself from, so a name that works in .env
// works here.
func buildFromTable(o Options, getenv func(string) string) (llm.Provider, error) {
	table, err := config.LoadProviders(getenv)
	if err != nil {
		// config's own message names the offending variable; it carries no key.
		return nil, newProviderError(err, "the provider table in the environment is not usable")
	}
	for _, row := range table {
		if row.Name == o.Provider {
			return newClient(row.Name, string(row.Kind), row.BaseURL, row.APIKey)
		}
	}
	if len(table) == 0 {
		return nil, newProviderError(nil, "--provider %s: no providers are configured in the environment; %s", o.Provider, providerEnvHint)
	}
	return nil, newProviderError(nil, "--provider %s is not configured; available: %s (%s)",
		o.Provider, strings.Join(providerNames(table), ", "), providerEnvHint)
}

// providerNames lists the table's names in sorted order, so the same table
// always prints the same message.
func providerNames(table []config.Provider) []string {
	names := make([]string, 0, len(table))
	for _, p := range table {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// newClient is the one place a kind becomes a client, mirroring
// registry.BuildClient's switch — the offline mode speaks the same two dialects
// as the server, and adding a third must mean editing both.
func newClient(name, kind, baseURL, apiKey string) (llm.Provider, error) {
	switch kind {
	case ProviderKindAnthropicCompat:
		return anthropiccompat.New(name, baseURL, apiKey), nil
	case ProviderKindOpenAICompat:
		return openaicompat.New(name, baseURL, apiKey), nil
	}
	return nil, newProviderError(nil, "provider %s declares kind %q, want %q or %q",
		name, kind, ProviderKindAnthropicCompat, ProviderKindOpenAICompat)
}

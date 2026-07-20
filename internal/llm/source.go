package llm

import (
	"context"
	"fmt"

	"golang.org/x/time/rate"
)

// ProviderSource resolves a provider name to a live client + its rate limiter.
// Production uses the DB-backed registry (providers managed in the app UI, D11 v1);
// tests use StaticSource.
type ProviderSource interface {
	Provider(ctx context.Context, name string) (Provider, *rate.Limiter, error)
}

// ErrProviderUnavailable prefixes resolution failures (unknown/disabled provider).
type ProviderUnavailableError struct {
	Name   string
	Reason string
}

func (e *ProviderUnavailableError) Error() string {
	return fmt.Sprintf("provider %q unavailable: %s", e.Name, e.Reason)
}

// VerifiableProvider is a Provider whose credentials/endpoint can be checked live —
// what the Providers page "Test" button drives. Both wire adapters implement it.
type VerifiableProvider interface {
	Provider
	ListModels(ctx context.Context) ([]string, error)
	Ping(ctx context.Context, model string) error
}

// StaticSource is a fixed name→Provider map for tests, with a generous shared
// limiter per provider.
type StaticSource map[string]Provider

func (s StaticSource) Provider(_ context.Context, name string) (Provider, *rate.Limiter, error) {
	p, ok := s[name]
	if !ok {
		return nil, nil, &ProviderUnavailableError{Name: name, Reason: "not configured"}
	}
	return p, rate.NewLimiter(rate.Inf, 1), nil
}

package auth

import "context"

// BootstrapStore is the narrow slice of the user store the bootstrap check needs.
type BootstrapStore interface {
	CountActiveAdmins(ctx context.Context) (int64, error)
	UpsertActiveAdmin(ctx context.Context, email string) error
}

// EnsureBootstrapAdmin seeds `email` as an active admin when the allowlist has no
// active admin at all — the fresh-deploy lockout fix (docs/DECISIONS.md D8). It is a
// no-op when email is empty or an active admin already exists, so a long-lived
// deployment never has its user management silently overridden by an env var.
func EnsureBootstrapAdmin(ctx context.Context, s BootstrapStore, email string) error {
	if email == "" {
		return nil
	}
	n, err := s.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.UpsertActiveAdmin(ctx, NormalizeEmail(email))
}

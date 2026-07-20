package auth

import (
	"context"
	"errors"
	"testing"
)

type fakeBootstrapStore struct {
	activeAdmins int64
	upserted     []string
	failCount    bool
}

func (f *fakeBootstrapStore) CountActiveAdmins(ctx context.Context) (int64, error) {
	if f.failCount {
		return 0, errors.New("db down")
	}
	return f.activeAdmins, nil
}

func (f *fakeBootstrapStore) UpsertActiveAdmin(ctx context.Context, email string) error {
	f.upserted = append(f.upserted, email)
	return nil
}

func TestEnsureBootstrapAdmin_SeedsWhenNoActiveAdmin(t *testing.T) {
	f := &fakeBootstrapStore{activeAdmins: 0}
	if err := EnsureBootstrapAdmin(context.Background(), f, "boss@ntu.edu.tw"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.upserted) != 1 || f.upserted[0] != "boss@ntu.edu.tw" {
		t.Errorf("upserted: got %v", f.upserted)
	}
}

func TestEnsureBootstrapAdmin_NoopWhenAdminExists(t *testing.T) {
	f := &fakeBootstrapStore{activeAdmins: 1}
	if err := EnsureBootstrapAdmin(context.Background(), f, "boss@ntu.edu.tw"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.upserted) != 0 {
		t.Errorf("should not upsert when an active admin exists, got %v", f.upserted)
	}
}

func TestEnsureBootstrapAdmin_NoopWhenEmailUnset(t *testing.T) {
	f := &fakeBootstrapStore{activeAdmins: 0}
	if err := EnsureBootstrapAdmin(context.Background(), f, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.upserted) != 0 {
		t.Errorf("empty email must not seed, got %v", f.upserted)
	}
}

func TestEnsureBootstrapAdmin_PropagatesStoreError(t *testing.T) {
	f := &fakeBootstrapStore{failCount: true}
	if err := EnsureBootstrapAdmin(context.Background(), f, "boss@ntu.edu.tw"); err == nil {
		t.Fatal("expected error")
	}
}

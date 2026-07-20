// Package storetest gives other packages' integration tests a migrated, clean
// database. Tests skip automatically when ADAMARKER_TEST_DATABASE_URL is unset, so
// plain `make test` never needs Postgres.
package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/HaoWen46/adagrade/internal/store"
)

// DSN returns the integration-test database URL or skips the test.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ADAMARKER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ADAMARKER_TEST_DATABASE_URL not set (use `make test-integration`)")
	}
	return dsn
}

// Fresh wipes the test database, runs all migrations, and returns a connected Store
// that closes with the test.
//
// Different test *packages* run in parallel and share the one test database, so Fresh
// holds a Postgres session-level advisory lock for the entire test — DB-backed tests
// serialize across packages instead of dropping the schema out from under each other.
func Fresh(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	dsn := DSN(t)

	s, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	t.Cleanup(s.Close)

	lock, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("storetest: acquire lock conn: %v", err)
	}
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext('adamarker-test-db'))"); err != nil {
		lock.Release()
		t.Fatalf("storetest: advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lock.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('adamarker-test-db'))")
		lock.Release()
	})

	if _, err := s.Pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("storetest: reset schema: %v", err)
	}
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	// River's own tables, so tests exercising the queue/runner Just Work.
	migrator, err := rivermigrate.New(riverpgxv5.New(s.Pool), nil)
	if err != nil {
		t.Fatalf("storetest: river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("storetest: river migrate: %v", err)
	}
	return s
}

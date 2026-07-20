package queue

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestAcquireWorkerLock_SecondFailsWhileHeld is the core of Finding 1
// (docs/DECISIONS.md D26): a single worker fleet per database. The advisory lock
// lives on a dedicated connection, so a second AcquireWorkerLock against the same
// database must fail while the first holds it, and succeed once the first
// releases. This is the guard that stops a stale ./bin/adamarker zombie from
// grading new jobs with old code against the shared dev DB.
func TestAcquireWorkerLock_SecondFailsWhileHeld(t *testing.T) {
	// storetest.Fresh both skips without a test DB and serializes DB-backed tests
	// across packages via its own advisory lock — so no other package's worker
	// lock can be held concurrently and flake this test.
	st := storetest.Fresh(t)
	dsn := storetest.DSN(t)
	ctx := context.Background()

	release1, err := AcquireWorkerLock(ctx, dsn)
	if err != nil {
		t.Fatalf("first AcquireWorkerLock: unexpected error: %v", err)
	}

	// Second acquisition must fail while the first holds the lock.
	release2, err := AcquireWorkerLock(ctx, dsn)
	if err == nil {
		release2()
		t.Fatal("second AcquireWorkerLock succeeded while first still held the lock; want error")
	}

	// After the first releases, a fresh acquisition must succeed.
	release1()
	release3, err := AcquireWorkerLock(ctx, dsn)
	if err != nil {
		t.Fatalf("AcquireWorkerLock after release: unexpected error: %v", err)
	}
	release3()

	// prevent the connected-but-lost warning if st goes unused.
	_ = st
}

// TestAcquireWorkerLock_ReleaseIsIdempotent proves release can be called more than
// once safely (main.go's deferred release plus any explicit call): a second
// release is a no-op and does not panic.
func TestAcquireWorkerLock_ReleaseIsIdempotent(t *testing.T) {
	_ = storetest.Fresh(t)
	dsn := storetest.DSN(t)
	ctx := context.Background()

	release, err := AcquireWorkerLock(ctx, dsn)
	if err != nil {
		t.Fatalf("AcquireWorkerLock: %v", err)
	}
	release()
	release() // must not panic

	// Lock must be free again after release.
	release2, err := AcquireWorkerLock(ctx, dsn)
	if err != nil {
		t.Fatalf("AcquireWorkerLock after idempotent release: %v", err)
	}
	release2()
}

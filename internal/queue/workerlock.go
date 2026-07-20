package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
)

// workerLockKey names the process-wide "one worker fleet per database" advisory
// lock (docs/DECISIONS.md D26). hashtext maps the label to the int4 key
// pg_try_advisory_lock's single-argument (bigint) form takes; the label is only
// ever passed to the DB, never logged, so it carries no PII.
const workerLockKey = "adamarker:workers"

// ErrWorkerLockHeld is returned by AcquireWorkerLock when another adamarker
// instance already holds the worker-fleet advisory lock against the same
// database. Callers decide whether that is fatal (the default) or a downgraded
// warning (ADAMARKER_ALLOW_MULTIPLE_WORKERS=1).
var ErrWorkerLockHeld = errors.New("queue: another adamarker instance is already running workers against this database")

// AcquireWorkerLock takes the single-worker-fleet advisory lock so exactly one
// adamarker process works jobs against a given database at a time (D26). This is
// the fix for the CRITICAL audit finding: two stale ./bin/adamarker zombies kept
// River workers alive against the shared dev DB for hours, grading new jobs with
// old code.
//
// The lock MUST live on a dedicated connection opened with pgx.Connect — NOT a
// pooled connection. pg_advisory_lock is session-scoped: a pool recycles
// connections and would silently drop the lock when the holding conn is returned
// and reused, defeating the guard. Holding one dedicated conn open for the
// process lifetime keeps the session — and thus the lock — alive.
//
// On success it returns a release func that closes the dedicated connection
// (which releases the session lock best-effort) and is safe to call more than
// once. On contention it returns ErrWorkerLockHeld. The caller (main.go) wires
// this between queue.New and qc.Start and treats ErrWorkerLockHeld as fatal by
// default.
func AcquireWorkerLock(ctx context.Context, dsn string) (release func(), err error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("queue: worker-lock connect: %w", err)
	}

	var got bool
	if err := conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock(hashtext($1))", workerLockKey,
	).Scan(&got); err != nil {
		// Close the just-opened conn before returning; a failed try leaves no lock.
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("queue: worker-lock try: %w", err)
	}
	if !got {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, ErrWorkerLockHeld
	}

	// release closes the dedicated conn (releasing the session lock). Idempotent
	// via sync.Once so a deferred release plus an explicit one is safe. Uses a
	// cancellation-free context so shutdown (which cancels the run ctx) can still
	// close the conn cleanly.
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = conn.Close(context.WithoutCancel(context.Background()))
		})
	}
	return release, nil
}

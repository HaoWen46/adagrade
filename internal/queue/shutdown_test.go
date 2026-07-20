package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

// --- isShutdownCancel / snoozeOnShutdown: pure logic, no DB (F17) ---

func TestSnoozeOnShutdown_OnlyWhenStopping(t *testing.T) {
	c := &Client{}

	// Not stopping: a context.Canceled is a real error, passed through untouched so
	// the normal retry/terminal taxonomy still applies (a per-job timeout mid-run
	// must NOT be silently snoozed).
	if got := c.snoozeOnShutdown(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("not stopping: want passthrough of context.Canceled, got %v", got)
	}

	// Stopping + context.Canceled ⇒ JobSnooze(0) so the attempt isn't consumed.
	c.stopping.Store(true)
	var snooze *rivertype.JobSnoozeError
	if got := c.snoozeOnShutdown(context.Canceled); !errors.As(got, &snooze) {
		t.Errorf("stopping + Canceled: want JobSnooze, got %v", got)
	}
	if got := c.snoozeOnShutdown(fmt.Errorf("wrap: %w", context.DeadlineExceeded)); !errors.As(got, &snooze) {
		t.Errorf("stopping + wrapped DeadlineExceeded: want JobSnooze, got %v", got)
	}

	// Stopping but a NON-cancellation error passes through unchanged (a real
	// grading/provider failure during shutdown is still a real failure).
	real := errors.New("provider 500")
	if got := c.snoozeOnShutdown(real); got != real {
		t.Errorf("stopping + real error: want passthrough, got %v", got)
	}
	// nil stays nil.
	if got := c.snoozeOnShutdown(nil); got != nil {
		t.Errorf("nil: want nil, got %v", got)
	}
}

// --- Shutdown drains an in-flight job (integration, needs the test DB) ---

// slowArgs is a test-only job whose worker sleeps, to prove Shutdown's soft drain
// waits for an in-flight job before returning.
type slowArgs struct {
	Sleep time.Duration `json:"sleep"`
}

func (slowArgs) Kind() string { return "test.slow" }

type slowWorker struct {
	river.WorkerDefaults[slowArgs]
	started chan struct{}
	done    *atomic.Bool
}

func (w *slowWorker) Work(ctx context.Context, job *river.Job[slowArgs]) error {
	select {
	case w.started <- struct{}{}:
	default:
	}
	// Sleep, but stop early if the context is cancelled (a hard stop) — cooperative,
	// like the real workers.
	select {
	case <-time.After(job.Args.Sleep):
		w.done.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// testDSN mirrors storetest.DSN without importing it (queue is a lower layer): skip
// unless the integration DB is configured.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ADAMARKER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ADAMARKER_TEST_DATABASE_URL not set")
	}
	return dsn
}

// freshQueuePool returns a migrated pool for an isolated client, holding the shared
// test-DB advisory lock for the test's duration.
func freshQueuePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext('adamarker-test-db'))"); err != nil {
		lock.Release()
		t.Fatalf("advisory lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lock.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext('adamarker-test-db'))")
		lock.Release()
	})
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	return pool
}

// newTestClient builds a real River client wrapping our Client shell with a single
// test worker on a "test" queue, so we exercise the actual Start/Shutdown path.
func newTestClient(t *testing.T, pool *pgxpool.Pool, w river.Worker[slowArgs]) *Client {
	t.Helper()
	c := &Client{}
	workers := river.NewWorkers()
	river.AddWorker(workers, w)
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:               map[string]river.QueueConfig{"test": {MaxWorkers: 2}},
		Workers:              workers,
		SoftStopTimeout:      escalationSoftStopTimeout,
		RescueStuckJobsAfter: rescueStuckJobsAfter,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.river = rc
	return c
}

// TestShutdown_DrainsInFlightJob: a job already running when Shutdown is called
// must be allowed to finish within the drain window — Shutdown returns nil only
// after the worker body completes and the job is 'completed', not cancelled.
func TestShutdown_DrainsInFlightJob(t *testing.T) {
	pool := freshQueuePool(t)
	started := make(chan struct{}, 1)
	var done atomic.Bool
	c := newTestClient(t, pool, &slowWorker{started: started, done: &done})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	res, err := c.river.Insert(context.Background(), slowArgs{Sleep: 1500 * time.Millisecond}, &river.InsertOpts{Queue: "test"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Wait until the worker has actually picked the job up, so Shutdown races a
	// genuinely in-flight job rather than an unstarted one.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job never started")
	}

	// A drain window comfortably longer than the job: a clean soft drain.
	if err := c.Shutdown(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !done.Load() {
		t.Error("Shutdown returned before the in-flight job finished (drain did not wait)")
	}

	job, err := c.river.JobGet(context.Background(), res.Job.ID)
	if err != nil {
		t.Fatalf("JobGet: %v", err)
	}
	if job.State != rivertype.JobStateCompleted {
		t.Errorf("drained job state = %q, want completed", job.State)
	}
}

// TestShutdown_EscalatesWhenDrainExpires: when a job outlives the drain window,
// Shutdown escalates to a hard cancel. Because the worker returns ctx.Err() while
// the client is stopping, snoozeOnShutdown turns it into a JobSnooze — the job ends
// up re-schedulable (scheduled/available) WITHOUT consuming its attempt, never
// discarded.
func TestShutdown_EscalatesWhenDrainExpires(t *testing.T) {
	pool := freshQueuePool(t)
	started := make(chan struct{}, 1)
	var done atomic.Bool
	c := newTestClient(t, pool, &slowWorker{started: started, done: &done})

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// A job far longer than the drain window forces escalation.
	res, err := c.river.Insert(context.Background(), slowArgs{Sleep: 30 * time.Second}, &river.InsertOpts{Queue: "test"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job never started")
	}

	// Tiny drain → escalate to StopAndCancel almost immediately.
	if err := c.Shutdown(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if done.Load() {
		t.Error("job should have been cancelled mid-sleep, not completed")
	}

	job, err := c.river.JobGet(context.Background(), res.Job.ID)
	if err != nil {
		t.Fatalf("JobGet: %v", err)
	}
	// The snooze re-queues the job without burning an attempt: it must NOT be
	// discarded/cancelled, and its recorded attempt count stays at 1 (snooze wrote
	// Attempt-1, i.e. it did not advance).
	switch job.State {
	case rivertype.JobStateScheduled, rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateRetryable:
		// acceptable: re-schedulable
	default:
		t.Errorf("escalated job state = %q, want a re-schedulable state (not discarded/cancelled)", job.State)
	}
	if job.Attempt > 1 {
		t.Errorf("snooze must not consume an attempt: got attempt=%d, want <=1", job.Attempt)
	}
}

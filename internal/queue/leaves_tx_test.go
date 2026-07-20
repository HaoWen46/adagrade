package queue

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
)

// TestEnqueueLeavesTx_Transactional pins the retry-failed atomicity seam
// (adversarial audit 2026-07-11): leaf jobs enqueued via EnqueueLeavesTx commit
// or roll back WITH the caller's transaction, exactly like the planning path —
// a failure after the item reset can never leave pending items with no jobs.
func TestEnqueueLeavesTx_Transactional(t *testing.T) {
	pool := freshQueuePool(t)
	ctx := context.Background()
	c, err := New(pool, Deps{Runner: &grading.Runner{}}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	countLeafJobs := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'run.leaf'").Scan(&n); err != nil {
			t.Fatalf("count river_job: %v", err)
		}
		return n
	}

	// Rolled-back tx: nothing committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := c.EnqueueLeavesTx(ctx, tx, "fake", []int64{101, 102}); err != nil {
		t.Fatalf("EnqueueLeavesTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := countLeafJobs(); n != 0 {
		t.Fatalf("rolled-back tx must insert no jobs, found %d", n)
	}

	// Committed tx: one job per item, on the llm queue with the leaf budget.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := c.EnqueueLeavesTx(ctx, tx, "fake", []int64{101, 102}); err != nil {
		t.Fatalf("EnqueueLeavesTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := countLeafJobs(); n != 2 {
		t.Fatalf("committed tx must insert 2 jobs, found %d", n)
	}
	var badOpts int64
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'run.leaf' AND (queue <> $1 OR max_attempts <> $2)",
		llmQueue, leafMaxAttempts,
	).Scan(&badOpts); err != nil {
		t.Fatalf("check job opts: %v", err)
	}
	if badOpts != 0 {
		t.Fatalf("%d leaf jobs carry the wrong queue/max_attempts", badOpts)
	}
}

// TestNew_WiresStoppingHook: the runner's F17 Stopping hook must observe the
// client's stopping flag so ExecuteLeaf can tell a shutdown drain (worker
// snoozes; item safely stays 'running') from a plain final-attempt timeout
// (job discarded; item must be failed terminally to stay recoverable).
func TestNew_WiresStoppingHook(t *testing.T) {
	pool := unstartedPool(t)
	runner := &grading.Runner{}
	c, err := New(pool, Deps{Runner: runner}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if runner.Stopping == nil {
		t.Fatal("runner.Stopping was not wired")
	}
	if runner.Stopping() {
		t.Error("Stopping() = true before any shutdown")
	}
	c.stopping.Store(true)
	if !runner.Stopping() {
		t.Error("Stopping() = false after the stopping flag was set")
	}
}

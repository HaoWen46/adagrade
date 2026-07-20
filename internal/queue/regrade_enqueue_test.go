package queue

import (
	"context"
	"log/slog"
	"strconv"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
)

// TestEnqueueRegradeAI_DedupsInFlightSubItem is the BUG 5 regression: EnqueueRegradeAI had
// no River UniqueOpts, so double-clicking "Grade pending (N)" enqueued the same sub-item
// twice and doubled LLM spend. After the fix a duplicate enqueue of a sub-item that is
// still in flight (queued/running/retryable) is a graceful no-op, while a genuinely NEW
// batch after the first job COMPLETED is still allowed to run.
func TestEnqueueRegradeAI_DedupsInFlightSubItem(t *testing.T) {
	pool := freshQueuePool(t)
	c, err := New(pool, Deps{Runner: &grading.Runner{}}, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	count := func(subItemID int64) int {
		var n int
		q := `SELECT count(*) FROM river_job WHERE kind = 'regrade.ai'`
		if subItemID == 0 {
			if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
				t.Fatalf("count jobs: %v", err)
			}
			return n
		}
		if err := pool.QueryRow(ctx, q+` AND args->>'sub_item_id' = $1`, strconv.FormatInt(subItemID, 10)).Scan(&n); err != nil {
			t.Fatalf("count jobs for %d: %v", subItemID, err)
		}
		return n
	}

	// Enqueue sub-item 42 twice while the first is still in-flight (never Start()ed, so
	// it stays 'available'): the duplicate is a no-op.
	if inserted, err := c.EnqueueRegradeAI(ctx, []int64{42}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	} else if inserted != 1 {
		t.Fatalf("first enqueue inserted %d jobs, want 1", inserted)
	}
	if inserted, err := c.EnqueueRegradeAI(ctx, []int64{42}); err != nil {
		t.Fatalf("duplicate enqueue: %v", err)
	} else if inserted != 0 {
		t.Fatalf("duplicate enqueue reported %d inserted jobs, want 0", inserted)
	}
	if got := count(42); got != 1 {
		t.Fatalf("double-enqueue of an in-flight sub-item created %d jobs, want 1", got)
	}

	// A DIFFERENT sub-item is unaffected — uniqueness is per-args, not global.
	if inserted, err := c.EnqueueRegradeAI(ctx, []int64{43}); err != nil {
		t.Fatalf("enqueue 43: %v", err)
	} else if inserted != 1 {
		t.Fatalf("enqueue 43 inserted %d jobs, want 1", inserted)
	}
	if got := count(0); got != 2 {
		t.Fatalf("distinct sub-item should enqueue separately, total jobs = %d, want 2", got)
	}

	// After the first job COMPLETES, a fresh batch for the same sub-item must run again
	// (re-run-after-completion semantics): mark 42 completed, then re-enqueue → allowed.
	if _, err := pool.Exec(ctx,
		`UPDATE river_job SET state = 'completed', finalized_at = now()
		 WHERE kind = 'regrade.ai' AND args->>'sub_item_id' = '42'`); err != nil {
		t.Fatalf("mark 42 completed: %v", err)
	}
	if inserted, err := c.EnqueueRegradeAI(ctx, []int64{42}); err != nil {
		t.Fatalf("re-enqueue after completion: %v", err)
	} else if inserted != 1 {
		t.Fatalf("re-enqueue after completion inserted %d jobs, want 1", inserted)
	}
	if got := count(42); got != 2 {
		t.Fatalf("re-enqueue after completion should be allowed, got %d rows for sub-item 42, want 2 (one completed + one new)", got)
	}
}

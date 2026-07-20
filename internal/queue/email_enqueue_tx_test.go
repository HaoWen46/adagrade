package queue

import (
	"context"
	"strconv"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/publish"
)

// TestEnqueueEmailSendsTx_CommitRollbackAndGenerationUniqueness pins the
// transactional outbox seam for publish delivery. The publish-item state and its
// River job must commit or roll back together. A repeated enqueue for the same
// item generation is a no-op while that job is in flight, but a newer generation
// is distinct work and must enqueue even while the old generation remains queued.
func TestEnqueueEmailSendsTx_CommitRollbackAndGenerationUniqueness(t *testing.T) {
	pool := freshQueuePool(t)
	ctx := context.Background()
	c, err := New(pool, Deps{Runner: &grading.Runner{}, Email: &recordingEmailSender{}}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	count := func(itemID int64, generation int32) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM river_job
			WHERE kind = 'email.send'
			  AND args->>'item_id' = $1
			  AND args->>'email_generation' = $2`,
			strconv.FormatInt(itemID, 10), strconv.FormatInt(int64(generation), 10),
		).Scan(&n); err != nil {
			t.Fatalf("count jobs for item %d generation %d: %v", itemID, generation, err)
		}
		return n
	}

	ref := publish.DeliveryRef{ItemID: 101, Generation: 1}

	// A caller failure after enqueue must not strand a job for rolled-back item
	// state: River's insert participates in the caller's transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rollback tx: %v", err)
	}
	if err := c.EnqueueEmailSendsTx(ctx, tx, []publish.DeliveryRef{ref}); err != nil {
		t.Fatalf("enqueue rollback tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := count(ref.ItemID, ref.Generation); got != 0 {
		t.Fatalf("rolled-back enqueue left %d job(s), want 0", got)
	}

	// Commit publishes exactly one job with the expected queue policy.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin commit tx: %v", err)
	}
	if err := c.EnqueueEmailSendsTx(ctx, tx, []publish.DeliveryRef{ref}); err != nil {
		t.Fatalf("enqueue commit tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := count(ref.ItemID, ref.Generation); got != 1 {
		t.Fatalf("committed enqueue created %d job(s), want 1", got)
	}
	var badOpts int64
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM river_job
		WHERE kind = 'email.send'
		  AND (queue <> $1 OR max_attempts <> $2)`,
		emailQueue, emailSendMaxAttempts,
	).Scan(&badOpts); err != nil {
		t.Fatalf("check email job options: %v", err)
	}
	if badOpts != 0 {
		t.Fatalf("%d email job(s) have the wrong queue/max attempts", badOpts)
	}

	// The same generation is already queued, so a duplicate transactional call
	// succeeds without creating a second in-flight job.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin duplicate tx: %v", err)
	}
	if err := c.EnqueueEmailSendsTx(ctx, tx, []publish.DeliveryRef{ref}); err != nil {
		t.Fatalf("enqueue duplicate tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit duplicate tx: %v", err)
	}
	if got := count(ref.ItemID, ref.Generation); got != 1 {
		t.Fatalf("same-generation duplicate created %d jobs, want 1 total", got)
	}

	// A resend advances the generation. It is different delivery work and must
	// not be suppressed by the still-in-flight generation-1 job.
	newer := publish.DeliveryRef{ItemID: ref.ItemID, Generation: ref.Generation + 1}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin newer-generation tx: %v", err)
	}
	if err := c.EnqueueEmailSendsTx(ctx, tx, []publish.DeliveryRef{newer}); err != nil {
		t.Fatalf("enqueue newer generation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit newer generation: %v", err)
	}
	if got := count(newer.ItemID, newer.Generation); got != 1 {
		t.Fatalf("new generation created %d jobs, want 1", got)
	}
}

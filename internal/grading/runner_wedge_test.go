package grading

import (
	"context"
	"errors"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// Run-state wedge regressions (adversarial audit 2026-07-11): redelivered
// already-succeeded leaves must still run the completion check, and a final
// attempt that ends in an interruption must not strand the item in 'running'
// where neither River (job discarded) nor retry-failed (matches 'failed' only)
// can ever reach it again.

// interruptingProvider cancels the leaf's OWN context mid-call and returns its
// error — the exact shape of River's per-job timeout firing during the vision
// call: every context downstream of the job ctx is dead by the time ExecuteLeaf
// sees the error.
type interruptingProvider struct{ cancel context.CancelFunc }

func (p *interruptingProvider) Name() string { return "fake" }

func (p *interruptingProvider) Grade(ctx context.Context, _ string, _ llm.Request) (llm.Result, error) {
	p.cancel()
	return llm.Result{}, ctx.Err()
}

// planItems plans the run and returns its item rows.
func planItems(t *testing.T, h *spotCheckHarness, runID int64) []db.ListRunItemsRow {
	t.Helper()
	ctx := context.Background()
	if err := h.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	items, err := h.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 100000})
	if err != nil {
		t.Fatalf("list run items: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no items planned")
	}
	return items
}

// TestExecuteLeaf_RedeliveredSucceededLeafCompletesRun: a worker that crashes
// after writing the item record but before the completion check leaves every
// item 'succeeded' with the run stuck 'running'. River redelivers the job; the
// succeeded short-circuit must re-run the completion check instead of returning
// early, or the run sits 'running' forever.
func TestExecuteLeaf_RedeliveredSucceededLeafCompletesRun(t *testing.T) {
	h := newSpotCheckHarness(t)
	ctx := context.Background()
	runID := h.buildRun(t, 2)
	items := planItems(t, h, runID)

	// First leaf completes through the normal path (run stays running — one
	// live leaf remains).
	if err := h.runner.ExecuteLeaf(ctx, items[0].ID, false); err != nil {
		t.Fatalf("execute leaf 1: %v", err)
	}

	// Second leaf: simulate the crash window by calling gradeLeaf directly —
	// record + item written, completion check never ran.
	item, err := h.st.Q.GetRunItem(ctx, items[1].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	run, err := h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if err := h.runner.gradeLeaf(ctx, item, run); err != nil {
		t.Fatalf("gradeLeaf: %v", err)
	}
	item, err = h.st.Q.GetRunItem(ctx, items[1].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != "succeeded" {
		t.Fatalf("setup: item state = %q, want succeeded", item.State)
	}
	run, err = h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("setup: run status = %q, want running (crashed before completion check)", run.Status)
	}

	// Redelivery of the already-succeeded leaf must flip the run to completed.
	if err := h.runner.ExecuteLeaf(ctx, items[1].ID, false); err != nil {
		t.Fatalf("redelivered ExecuteLeaf: %v", err)
	}
	run, err = h.st.Q.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "completed" {
		t.Errorf("run status after redelivery = %q, want completed", run.Status)
	}
}

// TestExecuteLeaf_FinalAttemptInterruptionMarksFailed: a per-job timeout on the
// FINAL attempt means River discards the job — an item left 'running' would be
// unrecoverable (retry-failed only resets state='failed'). ExecuteLeaf must
// write an honest terminal failure instead, on a context that survives the
// interruption that killed the job ctx, and let the run complete.
func TestExecuteLeaf_FinalAttemptInterruptionMarksFailed(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	items := planItems(t, h, runID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &Runner{
		Store:     h.st,
		Blobs:     h.blobs,
		Providers: llm.StaticSource{"fake": &interruptingProvider{cancel: cancel}},
		Log:       h.runner.Log,
	}

	err := runner.ExecuteLeaf(ctx, items[0].ID, true /* finalAttempt */)
	if err != nil {
		t.Fatalf("final-attempt interruption should be absorbed into the item row (job completes), got %v", err)
	}

	item, err := h.st.Q.GetRunItem(context.Background(), items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != "failed" {
		t.Errorf("item state = %q, want failed (recoverable via retry-failed)", item.State)
	}
	if !item.Error.Valid || item.Error.String != "interrupted on final attempt (timeout/shutdown)" {
		t.Errorf("item error = %+v, want the honest interruption message", item.Error)
	}

	// The run must complete (completed-with-failure), not sit 'running' forever.
	run, err := h.st.Q.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "completed" {
		t.Errorf("run status = %q, want completed", run.Status)
	}

	// And retry-failed's reset must pick the item back up.
	ids, err := h.st.Q.ResetFailedItems(context.Background(), runID)
	if err != nil {
		t.Fatalf("reset failed items: %v", err)
	}
	if len(ids) != 1 || ids[0] != items[0].ID {
		t.Errorf("ResetFailedItems = %v, want the interrupted item", ids)
	}
}

// TestExecuteLeaf_FinalAttemptShutdownInterruptionNotTerminal preserves F17: a
// graceful-shutdown cancellation on the final attempt is snoozed by the worker
// (the attempt is NOT consumed, the job re-runs on next start), so the runner
// must not burn the leaf into a terminal failure. The Stopping hook is how the
// runner knows the difference.
func TestExecuteLeaf_FinalAttemptShutdownInterruptionNotTerminal(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	items := planItems(t, h, runID)

	cancelProv := &fake.ScriptedProvider{NameStr: "fake", Steps: []fake.JSONStep{{Err: context.Canceled}}}
	runner := &Runner{
		Store:     h.st,
		Blobs:     h.blobs,
		Providers: llm.StaticSource{"fake": cancelProv},
		Stopping:  func() bool { return true },
		Log:       h.runner.Log,
	}

	err := runner.ExecuteLeaf(context.Background(), items[0].ID, true /* finalAttempt */)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown interruption must be RETURNED so the worker snoozes, got %v", err)
	}

	item, err := h.st.Q.GetRunItem(context.Background(), items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State == "failed" {
		t.Errorf("shutdown interruption must not mark the item failed, got state=%q", item.State)
	}
	if item.Error.Valid {
		t.Errorf("shutdown interruption must not write an error column, got %q", item.Error.String)
	}
	run, err := h.st.Q.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Errorf("run status = %q, want running (leaf reworks on next start)", run.Status)
	}
}

// TestExecuteLeaf_NonFinalInterruptionNotTerminal preserves F17 for non-final
// attempts regardless of shutdown state: the error is returned, River retries,
// no terminal state is written.
func TestExecuteLeaf_NonFinalInterruptionNotTerminal(t *testing.T) {
	h := newSpotCheckHarness(t)
	runID := h.buildRun(t, 1)
	items := planItems(t, h, runID)

	timeoutProv := &fake.ScriptedProvider{NameStr: "fake", Steps: []fake.JSONStep{{Err: context.DeadlineExceeded}}}
	runner := &Runner{
		Store:     h.st,
		Blobs:     h.blobs,
		Providers: llm.StaticSource{"fake": timeoutProv},
		Log:       h.runner.Log,
	}

	err := runner.ExecuteLeaf(context.Background(), items[0].ID, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("non-final interruption must be returned for River's retry, got %v", err)
	}

	item, err := h.st.Q.GetRunItem(context.Background(), items[0].ID)
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State == "failed" {
		t.Errorf("non-final interruption must not mark the item failed, got state=%q", item.State)
	}
	if item.Error.Valid {
		t.Errorf("non-final interruption must not write an error column, got %q", item.Error.String)
	}
}

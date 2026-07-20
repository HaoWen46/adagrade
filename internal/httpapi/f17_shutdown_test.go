package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// gradeableRun builds a run through the normal HTTP path to the point where its
// leaves are planned and gradeable (masks accepted, items created), returning the
// run id and the planned item rows. It drives Plan on the env's own runner (fake
// provider) only far enough to CREATE the items — it does NOT grade them, so a
// caller can execute a leaf with its own provider.
func gradeableRun(t *testing.T, env *testEnv, c *http.Client, aid, methodID int64) (int64, []db.ListRunItemsRow) {
	t.Helper()
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	if err := env.runner.Plan(context.Background(), runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	items, err := env.st.Q.ListRunItems(context.Background(), db.ListRunItemsParams{RunID: runID, ItemLimit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("gradeableRun: no items planned")
	}
	return runID, items
}

// TestExecuteLeaf_ShutdownCancellationNotTerminal is the F17 grading half: a
// SIGTERM that cancels a provider call mid-flight surfaces as context.Canceled
// while the queue client's stopping flag (the runner's Stopping hook) is set.
// Even on the FINAL attempt, ExecuteLeaf must NOT write a terminal failed item
// (no failed state, no error column, no model record) — the worker snoozes the
// job, so the attempt isn't consumed and the leaf reworks on the next start. It
// returns the error so River records a plain errored/snoozed attempt instead of
// burning the leaf into a permanent failure. (A final-attempt interruption
// WITHOUT the stopping flag — a plain job timeout — is the opposite case: River
// discards the job, so the runner must fail the item terminally; see
// internal/grading's TestExecuteLeaf_FinalAttemptInterruptionMarksFailed.)
func TestExecuteLeaf_ShutdownCancellationNotTerminal(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)
	runID, items := gradeableRun(t, env, c, aid, methodID)

	// A runner whose "fake" provider aborts every call with context.Canceled —
	// exactly what a mid-flight SIGTERM does to the vision HTTP call — with the
	// Stopping hook reading true, as queue.New wires it during Shutdown.
	cancelProv := &fake.ScriptedProvider{NameStr: "fake", Steps: []fake.JSONStep{{Err: context.Canceled}}}
	runner := &grading.Runner{
		Store:     env.st,
		Blobs:     env.blobs,
		Providers: llm.StaticSource{"fake": cancelProv},
		Stopping:  func() bool { return true },
	}

	itemID := items[0].ID
	err := runner.ExecuteLeaf(context.Background(), itemID, true /* finalAttempt */)
	if err == nil {
		t.Fatal("ExecuteLeaf should RETURN the cancellation (so River records a plain attempt), got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("returned error should wrap context.Canceled, got %v", err)
	}

	got, err := env.st.Q.GetRunItem(context.Background(), itemID)
	if err != nil {
		t.Fatalf("GetRunItem: %v", err)
	}
	if got.State == "failed" {
		t.Errorf("interruption must NOT mark the item failed (got state=%q)", got.State)
	}
	if got.Error.Valid {
		t.Errorf("interruption must NOT write a terminal error column, got %q", got.Error.String)
	}

	// And no model record was persisted for this leaf.
	if _, err := env.st.Q.GetRecordForLeaf(context.Background(), db.GetRecordForLeafParams{
		RunID:    int8Of(runID),
		AnswerID: got.AnswerID,
		ModelID:  pgtype.Text{String: got.ModelID, Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("no model record should exist after a cancelled leaf, got err=%v", err)
	}
}

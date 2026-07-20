package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// mustCompleteRun flips a freshly created run straight to 'completed' — the
// zero-leaf and scope guards only fire for a run that already cleared the
// existing "must be completed" check, so tests exercising them skip the
// pending/running states entirely.
func mustCompleteRun(t *testing.T, s *store.Store, runID int64) db.GradingRun {
	t.Helper()
	run, err := s.Q.SetRunStatus(context.Background(), db.SetRunStatusParams{ID: runID, Status: "completed"})
	if err != nil {
		t.Fatalf("SetRunStatus(completed): %v", err)
	}
	return run
}

// mustSucceededItem records one succeeded leaf (run_item + its model record) so a
// run has a real spot-checkable sample — the positive control for the zero-leaf
// guard (A3).
func mustSucceededItem(t *testing.T, s *store.Store, f fixture, run db.GradingRun) {
	t.Helper()
	ctx := context.Background()
	rec := mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "6", 100, 50, "")
	item, err := s.Q.CreateRunItem(ctx, db.CreateRunItemParams{
		RunID: run.ID, AnswerID: f.AnswerID, ModelID: "test-model", Provider: "test-provider",
		RubricVersionID: pgtype.Int8{Int64: f.RubricVersionID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateRunItem: %v", err)
	}
	if _, err := s.Q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
		ID: item.ID, State: "succeeded", RecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
	}); err != nil {
		t.Fatalf("MarkItemTerminal(succeeded): %v", err)
	}
}

// TestSetAssessmentFinalSource_RejectsZeroSucceededRun is A3's RED/GREEN pin:
// a completed run whose leaves are all failed/skipped (zero succeeded) can
// never produce a spot-check sample (createSpotCheckSample only pools
// state='succeeded' items) — pinning it as the final source would wedge
// publish behind an unreachable "review spot-check" call to action. The store
// must refuse with a typed, distinguishable error before the pin (and its
// RecomputeOfficials side effect) ever lands.
func TestSetAssessmentFinalSource_RejectsZeroSucceededRun(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f) // scope_kind="assessment"
	mustCompleteRun(t, s, run.ID)
	// No run_items at all: zero succeeded leaves.

	_, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", run.ID)
	if !errors.Is(err, store.ErrFinalRunNoSucceeded) {
		t.Fatalf("SetAssessmentFinalSource on a zero-succeeded run: got %v want ErrFinalRunNoSucceeded", err)
	}

	// The assessment must be left untouched — a rejected pin is a no-op.
	a, err := s.Q.GetAssessment(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("GetAssessment: %v", err)
	}
	if a.FinalSourceKind.Valid {
		t.Fatalf("rejected pin changed final_source_kind: %+v", a)
	}

	// The positive control: the same run, now with one succeeded leaf, pins
	// cleanly — proves the guard is checking leaves, not blanket-refusing.
	mustSucceededItem(t, s, f, run)
	if _, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", run.ID); err != nil {
		t.Fatalf("SetAssessmentFinalSource once the run has a succeeded leaf: %v", err)
	}
}

// TestSetAssessmentFinalSource_RejectsNonAssessmentScope is A4-minimal's
// RED/GREEN pin: RecomputeOfficials joins strictly on run_id = final_run_id,
// so pinning a problem- or answer-scoped run as the final source silently
// un-officializes every answer outside that scope. Full supplemental-run
// layering is out of scope (decision recorded in the task brief); the store
// instead refuses any non-assessment-scoped run outright.
func TestSetAssessmentFinalSource_RejectsNonAssessmentScope(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	run, err := s.Q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: f.AssessmentID, ScopeKind: "problem", ScopeID: f.ProblemID,
		MethodVersionID: f.MethodVersionID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("CreateRun (problem-scoped): %v", err)
	}
	mustCompleteRun(t, s, run.ID)
	mustSucceededItem(t, s, f, run) // has real grades — the scope guard must fire anyway

	_, _, err = s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", run.ID)
	if !errors.Is(err, store.ErrFinalRunNotAssessmentScope) {
		t.Fatalf("SetAssessmentFinalSource on a problem-scoped run: got %v want ErrFinalRunNotAssessmentScope", err)
	}

	a, err := s.Q.GetAssessment(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("GetAssessment: %v", err)
	}
	if a.FinalSourceKind.Valid {
		t.Fatalf("rejected pin changed final_source_kind: %+v", a)
	}

	// An answer-scoped run is rejected the same way.
	answerRun, err := s.Q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: f.AssessmentID, ScopeKind: "answer", ScopeID: f.AnswerID,
		MethodVersionID: f.MethodVersionID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("CreateRun (answer-scoped): %v", err)
	}
	mustCompleteRun(t, s, answerRun.ID)
	mustSucceededItem(t, s, f, answerRun)
	if _, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", answerRun.ID); !errors.Is(err, store.ErrFinalRunNotAssessmentScope) {
		t.Fatalf("SetAssessmentFinalSource on an answer-scoped run: got %v want ErrFinalRunNotAssessmentScope", err)
	}
}

// TestSetAssessmentFinalSource_RecoversNullSourceWhilePublished is A6's
// RED/GREEN pin for the published-guard recovery exception: 0035's
// fail-closed backfill (or any other legacy path) can leave a PUBLISHED
// assessment with final_source_kind NULL. An operator must be able to set a
// source in that state without first unpublishing, because nothing
// published can move underneath them — RecomputeOfficials (grading.sql,
// "Published answers are never touched") only ever writes answers with
// published_at IS NULL. What must still 409 is changing a source that is
// already set while published; that stays gated behind the explicit
// unpublish escape hatch.
func TestSetAssessmentFinalSource_RecoversNullSourceWhilePublished(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	run := mustRun(t, s, f)
	mustCompleteRun(t, s, run.ID)
	mustSucceededItem(t, s, f, run)

	// Publish with a human official grade — mirrors 0035's world exactly:
	// an answer can be published with no final source ever chosen (or one
	// the migration had to NULL out), and its existing official pointer
	// must survive untouched.
	humanRecordID := mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")

	admin, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "admin@example.test", Role: "admin", Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Note:         "publish with no final source chosen",
		CreatedBy:    admin.ID,
		Items: []store.CreatePublishItemInput{{
			StudentID:      f.StudentID,
			Snapshot:       []byte(`{"total":"9"}`),
			RecipientEmail: "student@example.test",
			RegradeToken:   "tok-recover-null-source",
		}},
	}); err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}

	a, err := s.Q.GetAssessment(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("GetAssessment: %v", err)
	}
	if a.FinalSourceKind.Valid {
		t.Fatalf("test setup invariant broken: assessment already has a final source, got %+v", a.FinalSourceKind)
	}

	// The recovery exception: setting a source while published is allowed
	// when the current source is NULL.
	if _, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", run.ID); err != nil {
		t.Fatalf("SetAssessmentFinalSource(method) on a published assessment with NULL source: %v", err)
	}

	// The published answer's official pointer must be untouched — proof the
	// exception cannot rewrite a published grade.
	var officialRecordID int64
	if err := s.Pool.QueryRow(ctx,
		`SELECT official_record_id FROM answers WHERE id = $1`, f.AnswerID,
	).Scan(&officialRecordID); err != nil {
		t.Fatalf("query official_record_id: %v", err)
	}
	if officialRecordID != humanRecordID {
		t.Fatalf("published answer's official_record_id changed: got %d want %d (immutable)", officialRecordID, humanRecordID)
	}

	// Changing an ALREADY-SET source while still published must still 409 —
	// the exception is one-shot recovery from NULL, not a general bypass.
	run2, err := s.Q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: f.AssessmentID, ScopeKind: "assessment", ScopeID: f.AssessmentID,
		MethodVersionID: f.MethodVersionID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("CreateRun (second): %v", err)
	}
	mustCompleteRun(t, s, run2.ID)
	mustSucceededItem(t, s, f, run2)

	if _, _, err := s.SetAssessmentFinalSource(ctx, f.AssessmentID, "method", run2.ID); !errors.Is(err, store.ErrFinalSourcePublished) {
		t.Fatalf("changing an already-set source while published: got %v want ErrFinalSourcePublished", err)
	}
}

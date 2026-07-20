package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestSpotCheck_GateLifecycle(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f)
	rec := mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "6", 100, 50, "")

	// Fresh run: no sample yet, not waived.
	total, done, waived, err := s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState (fresh): %v", err)
	}
	if total != 0 || done != 0 || waived {
		t.Fatalf("expected empty unwaived state, got total=%d done=%d waived=%v", total, done, waived)
	}

	if err := s.InsertSpotChecks(ctx, run.ID, []int64{rec.ID}); err != nil {
		t.Fatalf("InsertSpotChecks: %v", err)
	}
	// Idempotent: inserting the same record again must not error or duplicate.
	if err := s.InsertSpotChecks(ctx, run.ID, []int64{rec.ID}); err != nil {
		t.Fatalf("InsertSpotChecks (repeat): %v", err)
	}

	total, done, waived, err = s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState (sampled, no verdict): %v", err)
	}
	if total != 1 || done != 0 || waived {
		t.Fatalf("expected total=1 done=0 unwaived, got total=%d done=%d waived=%v", total, done, waived)
	}

	checks, err := s.ListSpotChecks(ctx, run.ID)
	if err != nil || len(checks) != 1 {
		t.Fatalf("ListSpotChecks: got %d err %v", len(checks), err)
	}

	admin, err := s.Q.CreateUser(ctx, mustAdminParams(t))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := s.SetSpotCheckVerdict(ctx, checks[0].ID, "agree", "matches my read", admin.ID); err != nil {
		t.Fatalf("SetSpotCheckVerdict: %v", err)
	}

	total, done, waived, err = s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState (verdicted): %v", err)
	}
	if total != 1 || done != 1 || waived {
		t.Fatalf("expected gate open (total==done), got total=%d done=%d waived=%v", total, done, waived)
	}

	agreed, agreedTotal, err := s.SpotCheckAgreementRate(ctx, run.ID)
	if err != nil || agreed != 1 || agreedTotal != 1 {
		t.Fatalf("SpotCheckAgreementRate: got agreed=%d total=%d err=%v", agreed, agreedTotal, err)
	}
}

func TestWaiveSpotCheck_OpensGateWithoutSamples(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f)

	if err := s.WaiveSpotCheck(ctx, run.ID, 0, "no material graded leaves"); err != nil {
		t.Fatalf("WaiveSpotCheck: %v", err)
	}
	_, _, waived, err := s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState: %v", err)
	}
	if !waived {
		t.Fatalf("expected waived=true after WaiveSpotCheck")
	}

	// Idempotent re-waive with a different reason updates, doesn't error.
	if err := s.WaiveSpotCheck(ctx, run.ID, 0, "waived again"); err != nil {
		t.Fatalf("WaiveSpotCheck (re-waive): %v", err)
	}
}

// TestSpotCheck_PreExistingRunsWaivedByMigration is the trust-spec §4 backfill: runs
// that were already 'completed' before 0019_spot_checks.sql shipped must not be
// retroactively locked. We simulate this by inserting a completed run directly
// (bypassing CreateRun, which would happen before this migration in real history is
// approximated by creating the run, marking it completed, THEN re-running the
// backfill INSERT the migration performs, since storetest.Fresh already ran 0019
// before any run existed).
func TestSpotCheck_PreExistingRunsWaivedByMigration(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	run := mustRun(t, s, f)

	// storetest.Fresh runs migrations before this run exists, so the automatic
	// backfill in 0019 did not waive it. Simulate the historical scenario: mark the
	// run completed (as if it pre-dated the feature), then re-apply the exact
	// backfill statement the migration runs, to prove the SQL itself is correct.
	if _, err := s.Pool.Exec(ctx, `UPDATE grading_runs SET status = 'completed' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("mark run completed: %v", err)
	}
	_, _, waived, err := s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState before backfill: %v", err)
	}
	if waived {
		t.Fatalf("run should not be waived before the backfill runs")
	}

	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO waived_runs (run_id, reason, waived_by, waived_at)
		SELECT id, 'migration', NULL, now() FROM grading_runs WHERE status = 'completed'
		ON CONFLICT (run_id) DO NOTHING`); err != nil {
		t.Fatalf("apply backfill: %v", err)
	}

	_, _, waived, err = s.SpotCheckState(ctx, run.ID)
	if err != nil {
		t.Fatalf("SpotCheckState after backfill: %v", err)
	}
	if !waived {
		t.Fatalf("expected pre-existing completed run to be waived after the migration backfill")
	}
}

func mustAdminParams(t *testing.T) db.CreateUserParams {
	t.Helper()
	return db.CreateUserParams{Email: "checker-" + t.Name() + "@example.test", Role: "ta", Active: true}
}

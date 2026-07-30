package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// Migration 0037's Down deletes sample-scope runs plus their full referrer
// chain (run_items → spot_checks → grading_records → runs) before narrowing the
// scope_kind CHECK back. Two regressions this pins:
//
//  1. assessments_final_run_fk (0035) is DEFERRABLE INITIALLY DEFERRED, so the
//     runs DELETE queues per-row RI trigger events; without the migration's
//     SET CONSTRAINTS ALL IMMEDIATE the following ALTER TABLE fails with
//     "pending trigger events" (SQLSTATE 55006) whenever ANY sample row exists
//     — a failure TestMigrations_UpDownUp can never see on its empty database.
//  2. The four DELETEs are order-load-bearing (grading_records.run_id,
//     grading_run_items.record_id and spot_checks.grading_record_id have no ON
//     DELETE action), so a data-present down run exercises the ordering.
func TestMigration0037_DownWithSampleRunData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)
	f := mustFixture(t, s)

	run, err := s.Q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: f.AssessmentID, ScopeKind: "sample", ScopeID: 5,
		MethodVersionID: f.MethodVersionID, ExecutionMode: "sync",
	})
	if err != nil {
		t.Fatalf("create sample run: %v", err)
	}
	item, err := s.Q.CreateRunItem(ctx, db.CreateRunItemParams{
		RunID: run.ID, AnswerID: f.AnswerID, ModelID: "fake-vision-1", Provider: "fake",
		RubricVersionID: pgtype.Int8{Int64: f.RubricVersionID, Valid: true},
	})
	if err != nil {
		t.Fatalf("create run item: %v", err)
	}
	rec := mustModelRecord(t, s, run.ID, f.AnswerID, f.RubricVersionID, f.MethodVersionID, "7", 100, 50, "0.001")
	if _, err := s.Q.MarkItemTerminal(ctx, db.MarkItemTerminalParams{
		ID: item.ID, State: "succeeded", RecordID: pgtype.Int8{Int64: rec.ID, Valid: true},
	}); err != nil {
		t.Fatalf("finish item: %v", err)
	}
	if _, err := s.Q.InsertSpotCheck(ctx, db.InsertSpotCheckParams{
		RunID: run.ID, GradingRecordID: rec.ID,
	}); err != nil {
		t.Fatalf("insert spot check: %v", err)
	}

	// The regression: down past 0037 with sample data present must succeed.
	if err := store.MigrateDownTo(ctx, dsn, 36); err != nil {
		t.Fatalf("migrate down to 36 with sample-run data: %v", err)
	}

	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM grading_runs").Scan(&n); err != nil || n != 0 {
		t.Fatalf("sample runs should be deleted (err=%v, n=%d)", err, n)
	}
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM answers").Scan(&n); err != nil || n != 1 {
		t.Fatalf("unrelated data must survive the down (err=%v, n=%d)", err, n)
	}
	// The narrower CHECK is back in force.
	if _, err := s.Pool.Exec(ctx,
		"INSERT INTO grading_runs (assessment_id, scope_kind, scope_id, method_version_id, execution_mode, status) VALUES ($1, 'sample', 5, $2, 'sync', 'pending')",
		f.AssessmentID, f.MethodVersionID); err == nil {
		t.Fatal("inserting a sample-scope run after down should violate the restored CHECK")
	}

	// And the Up path re-widens it.
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("re-migrate up: %v", err)
	}
	if _, err := s.Q.CreateRun(ctx, db.CreateRunParams{
		AssessmentID: f.AssessmentID, ScopeKind: "sample", ScopeID: 3,
		MethodVersionID: f.MethodVersionID, ExecutionMode: "sync",
	}); err != nil {
		t.Fatalf("sample scope should be accepted again after re-up: %v", err)
	}
}

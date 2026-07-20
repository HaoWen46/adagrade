package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestMigration0024_DownSucceedsWithRegradeAIRecordPresent is the reviewer-reproduced
// CRITICAL-finding regression test: migration 0024's Down block used to re-narrow
// grading_records_policy_check back to ('lenient','standard','strict') BEFORE deleting
// the regrade_ai rows (whose policy='regrade_strict' violates that narrower CHECK),
// so Down failed with a check-constraint violation whenever a regrade_ai record
// existed. The fix moves the `DELETE FROM grading_records WHERE source='regrade_ai'`
// to the TOP of the Down block, before any CHECK re-narrow.
//
// This test applies every migration including 0025 (storetest.Fresh always runs to
// latest), inserts one regrade_ai record through the full FK chain (assessment ->
// problem -> rubric version -> student -> answer -> grading_records), links it via a
// v2 sub-item (migration 0025 replaced the v1 request-level ai_record_id this test
// originally used), then exercises down-to-0023 followed by back-up-to-0024 and
// asserts both succeed and the row was actually removed by the down migration
// (proving the delete, not just a lucky no-op). 0025's own down (past the sub-item
// table this test links through) is covered separately by
// TestMigration0025_DownSucceedsWithFullLinkedFixture.
func TestMigration0024_DownSucceedsWithRegradeAIRecordPresent(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	f := mustFixture(t, s)
	itemID := mustPublishItem(t, ctx, s, f)

	// A regrade_ai record (source='regrade_ai', policy='regrade_strict', no created_by)
	// through the real InsertRegradeAIRecord query — the exact row shape 0024 Up made
	// legal and Down must be able to clean up.
	rec, err := s.Q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
		AnswerID:        f.AnswerID,
		ModelID:         pgtype.Text{String: "regrade-model", Valid: true},
		RubricVersionID: f.RubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: []byte(`[]`),
		Comment:         "stricter re-examination",
		Adjustments:     []byte(`[]`),
		Policy:          pgtype.Text{String: "regrade_strict", Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertRegradeAIRecord (seed): %v", err)
	}
	if rec.Source != "regrade_ai" {
		t.Fatalf("seed record source = %q, want regrade_ai", rec.Source)
	}

	// Link the regrade_ai record to a regrade_request's sub-item (v2 schema, migration
	// 0025) — this creates the FK constraint that blocks the Down migration if it
	// doesn't unlink first.
	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: itemID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	subItems, err := s.InsertRequestProblems(ctx, rr.ID, []store.RequestProblemInput{
		{ProblemID: f.ProblemID, ComplaintText: "please regrade"},
	})
	if err != nil {
		t.Fatalf("InsertRequestProblems: %v", err)
	}
	if _, err := s.SetProblemAIRecord(ctx, subItems[0].ID, rec.ID); err != nil {
		t.Fatalf("SetProblemAIRecord (link): %v", err)
	}

	// The actual regression: Down must succeed even with the regrade_ai row present and linked.
	if err := store.MigrateDownTo(ctx, dsn, 23); err != nil {
		t.Fatalf("migrate down to 0023 with a regrade_ai record present: %v", err)
	}

	// Down really did delete the offending row (not a no-op that happened to pass).
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM grading_records WHERE id = $1", rec.ID).Scan(&n); err != nil {
		t.Fatalf("count seed record after down: %v", err)
	}
	if n != 0 {
		t.Fatalf("regrade_ai record should have been deleted by 0024's down migration, still found %d", n)
	}

	// And re-applying 0024 must succeed cleanly (up/down/up).
	if err := store.MigrateUpTo(ctx, dsn, 24); err != nil {
		t.Fatalf("migrate back up to 0024: %v", err)
	}

	// Post re-up, the widened CHECKs must admit a fresh regrade_ai row again.
	if _, err := s.Q.InsertRegradeAIRecord(ctx, db.InsertRegradeAIRecordParams{
		AnswerID:        f.AnswerID,
		ModelID:         pgtype.Text{String: "regrade-model-2", Valid: true},
		RubricVersionID: f.RubricVersionID,
		GradedImageShas: []string{},
		CriterionScores: []byte(`[]`),
		Comment:         "second stricter re-examination",
		Adjustments:     []byte(`[]`),
		Policy:          pgtype.Text{String: "regrade_strict", Valid: true},
	}); err != nil {
		t.Fatalf("InsertRegradeAIRecord after re-up: %v", err)
	}
}

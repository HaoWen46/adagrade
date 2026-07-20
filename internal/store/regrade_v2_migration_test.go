package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestMigration0025_DownSucceedsWithFullLinkedFixture is the test-first coverage for
// migration 0025's Down block, written BEFORE the Up/Down SQL per the task brief's
// hard lesson from 0024 (broken twice in review): apply, insert a full linked
// fixture through every new table (regrade_request_problems, problem_ta_assignments),
// down, up.
//
// Unlike 0024, 0025's new tables are pure additions with nothing else referencing
// them, so Down should be a straightforward two-DROP-TABLE plus column
// add-back/drop -- but the whole point of the hard lesson is not to trust that
// assumption without a test exercising it end to end (down-with-data, then back up
// and confirm the schema is usable again).
func TestMigration0025_DownSucceedsWithFullLinkedFixture(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	f := mustFixture(t, s)
	recordID := mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "6")

	ta, err := s.Q.CreateUser(ctx, db.CreateUserParams{
		Email: "ta-" + t.Name() + "@example.test", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatalf("CreateUser (ta): %v", err)
	}

	_, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}

	// A filed request with a linked sub-item (problem, complaint, AI record, verdict)
	// and a TA assignment -- one row in every new table this migration adds.
	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: items[0].ID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
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
	if len(subItems) != 1 {
		t.Fatalf("expected 1 sub-item, got %d", len(subItems))
	}
	if _, err := s.SetProblemAIRecord(ctx, subItems[0].ID, recordID); err != nil {
		t.Fatalf("SetProblemAIRecord: %v", err)
	}
	if _, err := s.SetProblemVerdict(ctx, store.SetProblemVerdictParams{
		SubItemID: subItems[0].ID, Verdict: "upheld", Note: "grade confirmed correct", VerdictBy: ta.ID,
	}); err != nil {
		t.Fatalf("SetProblemVerdict: %v", err)
	}
	if _, err := s.AssignProblemTA(ctx, f.ProblemID, ta.ID, 0); err != nil {
		t.Fatalf("AssignProblemTA: %v", err)
	}

	// The actual regression coverage: Down must succeed with linked rows present in
	// BOTH new tables plus a populated kind column and the partial unique index built.
	if err := store.MigrateDownTo(ctx, dsn, 24); err != nil {
		t.Fatalf("migrate down to 0024 with a full linked v2 fixture present: %v", err)
	}

	// The new tables are really gone (not just empty) -- querying them must fail.
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM regrade_requests WHERE id = $1", rr.ID).Scan(&n); err != nil {
		t.Fatalf("count regrade_requests row after down: %v", err)
	}
	if n != 1 {
		t.Fatalf("regrade_requests row should survive the down migration (only new columns/tables drop), got count %d", n)
	}
	var tableExists bool
	if err := s.Pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'regrade_request_problems')",
	).Scan(&tableExists); err != nil {
		t.Fatalf("check regrade_request_problems exists: %v", err)
	}
	if tableExists {
		t.Fatal("regrade_request_problems should have been dropped by the down migration")
	}
	if err := s.Pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'problem_ta_assignments')",
	).Scan(&tableExists); err != nil {
		t.Fatalf("check problem_ta_assignments exists: %v", err)
	}
	if tableExists {
		t.Fatal("problem_ta_assignments should have been dropped by the down migration")
	}

	// The v1 columns are back, empty.
	var problemID pgtype.Int8
	var aiRecordID pgtype.Int8
	var escalated bool
	if err := s.Pool.QueryRow(ctx,
		"SELECT problem_id, ai_record_id, escalated FROM regrade_requests WHERE id = $1", rr.ID,
	).Scan(&problemID, &aiRecordID, &escalated); err != nil {
		t.Fatalf("query v1 columns after down: %v", err)
	}
	if problemID.Valid || aiRecordID.Valid || escalated {
		t.Fatalf("v1 columns should be empty after down, got problem_id=%+v ai_record_id=%+v escalated=%v", problemID, aiRecordID, escalated)
	}

	// And re-applying 0025 (and everything after it — currently 0026's
	// result_sent_at, whole-branch review F1) must succeed cleanly (up/down/up), with
	// the schema usable again for a fresh v2 insert. RunMigrations (not a hardcoded
	// MigrateUpTo version) always means "the latest migration", matching
	// storetest.Fresh's own full-head setup — s.Store's sqlc queries are compiled
	// against the full current schema, so leaving the DB stopped at a fixed old
	// version here would drift out from under them every time a later migration adds
	// a column this test doesn't know about (as happened when 0026 landed).
	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("migrate back up to latest: %v", err)
	}
	if _, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: items[0].ID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nsecond time\n</p1>",
		Status: "received", Kind: "unparsed", Turn: 0,
	}); err != nil {
		t.Fatalf("InsertRegradeRequestV2 after re-up: %v", err)
	}
}

// TestMigration0026_DownSucceedsWithResultSentAtPopulated covers the send-failure
// recovery path's schema change (whole-branch review F1): result_sent_at is a single
// nullable column with no FK/CHECK entanglement, but the down migration must still
// drop it cleanly even when a row has it populated (not NULL), and re-applying it
// must leave the column usable again for a fresh SetRegradeResultSentAt call.
func TestMigration0026_DownSucceedsWithResultSentAtPopulated(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	dsn := storetest.DSN(t)

	f := mustFixture(t, s)
	_, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	rr, err := s.InsertRegradeRequestV2(ctx, store.InsertRegradeRequestV2Params{
		PublishItemID: items[0].ID, StudentID: f.StudentID, AssessmentID: f.AssessmentID,
		FromEmail: "student@example.test", Subject: "re: grade", Body: "<p1>\nplease regrade\n</p1>",
		Status: "resolved_upheld", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatalf("InsertRegradeRequestV2: %v", err)
	}
	if _, err := s.SetRegradeResultSentAt(ctx, rr.ID); err != nil {
		t.Fatalf("SetRegradeResultSentAt: %v", err)
	}

	if err := store.MigrateDownTo(ctx, dsn, 25); err != nil {
		t.Fatalf("migrate down to 0025 with result_sent_at populated: %v", err)
	}
	var colExists bool
	if err := s.Pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'regrade_requests' AND column_name = 'result_sent_at')",
	).Scan(&colExists); err != nil {
		t.Fatalf("check result_sent_at column exists: %v", err)
	}
	if colExists {
		t.Fatal("result_sent_at should have been dropped by the down migration")
	}
	// The request row itself survives (only the new column drops).
	var n int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM regrade_requests WHERE id = $1", rr.ID).Scan(&n); err != nil {
		t.Fatalf("count regrade_requests row after down: %v", err)
	}
	if n != 1 {
		t.Fatalf("regrade_requests row should survive the down migration, got count %d", n)
	}

	if err := store.RunMigrations(ctx, dsn); err != nil {
		t.Fatalf("migrate back up to latest: %v", err)
	}
	if _, err := s.SetRegradeResultSentAt(ctx, rr.ID); err != nil {
		t.Fatalf("SetRegradeResultSentAt after re-up: %v", err)
	}
}

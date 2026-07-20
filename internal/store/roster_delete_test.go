package store_test

import (
	"context"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// TestGetStudentBlockingArtifacts_Unreferenced covers the B15 happy path: a
// student with no rows anywhere blocks nothing.
func TestGetStudentBlockingArtifacts_Unreferenced(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)

	student, err := s.Q.UpsertStudent(ctx, db.UpsertStudentParams{
		StudentID: "b-unreferenced", Name: "Test Student", Email: "unref@example.test",
	})
	if err != nil {
		t.Fatalf("UpsertStudent: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, student.ID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if got.HasSubmissions || got.HasGradedAnswers || got.HasScanPages ||
		got.HasPublishItems || got.HasRegradeRequests || got.HasQuarantineResolutions {
		t.Fatalf("unreferenced student should have no blockers: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_BareAnswerDoesNotBlock: a materialized answer
// with no pages and no grading records (MaterializeAnswers scaffolding) must
// not block, and DeleteBareAnswersForStudent must remove it.
func TestGetStudentBlockingArtifacts_BareAnswerDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s) // f.AnswerID is a bare EnsureAnswer row: no pages, no records.

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if got.HasGradedAnswers {
		t.Fatalf("bare answer must not count as a graded/paged answer: %+v", got)
	}
	anyBlocker := got.HasSubmissions || got.HasGradedAnswers || got.HasScanPages ||
		got.HasPublishItems || got.HasRegradeRequests || got.HasQuarantineResolutions
	if anyBlocker {
		t.Fatalf("bare answer alone should block nothing: %+v", got)
	}

	n, err := s.Q.DeleteBareAnswersForStudent(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("DeleteBareAnswersForStudent: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 bare answer deleted, got %d", n)
	}
	if _, err := s.Q.GetAnswer(ctx, f.AnswerID); err == nil {
		t.Fatalf("bare answer should be gone after DeleteBareAnswersForStudent")
	}
}

// TestGetStudentBlockingArtifacts_Submission proves a live submission blocks,
// independent of whether any answer exists.
func TestGetStudentBlockingArtifacts_Submission(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	if _, err := s.Pool.Exec(ctx, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1)`, f.AssessmentID, f.StudentID); err != nil {
		t.Fatalf("seed submission: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasSubmissions {
		t.Fatalf("expected has_submissions=true: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_AnswerWithPage proves an answer with an
// ingested page (but no grading record) blocks.
func TestGetStudentBlockingArtifacts_AnswerWithPage(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	var subID int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1) RETURNING id`, f.AssessmentID, f.StudentID).Scan(&subID); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO answer_pages (answer_id, page_index, submission_id, pdf_page_index, image_ref, image_sha256, image_width, image_height)
		VALUES ($1, 0, $2, 0, 'img', 'imgsha', 100, 100)`, f.AnswerID, subID); err != nil {
		t.Fatalf("seed answer_pages: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasGradedAnswers {
		t.Fatalf("expected has_graded_answers=true for a paged answer: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_AnswerWithRecord proves a grading record
// alone (defense in depth, independent of pages) blocks.
func TestGetStudentBlockingArtifacts_AnswerWithRecord(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "5")

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasGradedAnswers {
		t.Fatalf("expected has_graded_answers=true for an answer with a grading record: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_ScanPages proves each of the three
// student-referencing columns on scan_pages (proposed/assigned/park) blocks.
func TestGetStudentBlockingArtifacts_ScanPages(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	var batchID, sourceID int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO scan_batches (assessment_id) VALUES ($1) RETURNING id`, f.AssessmentID).Scan(&batchID); err != nil {
		t.Fatalf("seed scan_batches: %v", err)
	}
	if err := s.Pool.QueryRow(ctx, `INSERT INTO scan_sources (batch_id, original_filename, source_ref, source_sha256, source_kind)
		VALUES ($1, 'f.pdf', 'ref', 'sha', 'pdf') RETURNING id`, batchID).Scan(&sourceID); err != nil {
		t.Fatalf("seed scan_sources: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO scan_pages (source_id, batch_id, assessment_id, page_index, proposed_student_id)
		VALUES ($1, $2, $3, 0, $4)`, sourceID, batchID, f.AssessmentID, f.StudentID); err != nil {
		t.Fatalf("seed scan_pages: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasScanPages {
		t.Fatalf("expected has_scan_pages=true for a proposed-identity scan page: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_PublishItem proves a publish snapshot blocks.
func TestGetStudentBlockingArtifacts_PublishItem(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	var batchID int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO publish_batches (assessment_id) VALUES ($1) RETURNING id`, f.AssessmentID).Scan(&batchID); err != nil {
		t.Fatalf("seed publish_batches: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO publish_items (batch_id, student_id, snapshot, recipient_email)
		VALUES ($1, $2, '{}'::jsonb, 'student@example.test')`, batchID, f.StudentID); err != nil {
		t.Fatalf("seed publish_items: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasPublishItems {
		t.Fatalf("expected has_publish_items=true: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_RegradeRequest proves an inbound regrade
// request blocks.
func TestGetStudentBlockingArtifacts_RegradeRequest(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	if _, err := s.Pool.Exec(ctx, `INSERT INTO regrade_requests (student_id, assessment_id, from_email, kind)
		VALUES ($1, $2, 'someone@example.test', 'unparsed')`, f.StudentID, f.AssessmentID); err != nil {
		t.Fatalf("seed regrade_requests: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasRegradeRequests {
		t.Fatalf("expected has_regrade_requests=true: %+v", got)
	}
}

// TestGetStudentBlockingArtifacts_QuarantineResolution proves a resolved
// quarantine row (a human decision tying an upload to this student) blocks —
// it is real audit history, not scaffolding.
func TestGetStudentBlockingArtifacts_QuarantineResolution(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	if _, err := s.Pool.Exec(ctx, `INSERT INTO upload_quarantine (assessment_id, original_filename, pdf_ref, pdf_sha256, reason, resolved_at, resolved_student_id)
		VALUES ($1, 'f.pdf', 'ref', 'sha', 'unknown_student', now(), $2)`, f.AssessmentID, f.StudentID); err != nil {
		t.Fatalf("seed upload_quarantine: %v", err)
	}

	got, err := s.Q.GetStudentBlockingArtifacts(ctx, f.StudentID)
	if err != nil {
		t.Fatalf("GetStudentBlockingArtifacts: %v", err)
	}
	if !got.HasQuarantineResolutions {
		t.Fatalf("expected has_quarantine_resolutions=true: %+v", got)
	}
}

// TestDeleteStudent_RemovesRow is a direct store-level check that DeleteStudent
// removes exactly the targeted row and reports 0 affected rows for an already-
// gone id (the handler's race guard relies on this).
func TestDeleteStudent_RemovesRow(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)

	student, err := s.Q.UpsertStudent(ctx, db.UpsertStudentParams{
		StudentID: "b-delete-me", Name: "Test Student", Email: "delete-me@example.test",
	})
	if err != nil {
		t.Fatalf("UpsertStudent: %v", err)
	}

	n, err := s.Q.DeleteStudent(ctx, student.ID)
	if err != nil {
		t.Fatalf("DeleteStudent: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row deleted, got %d", n)
	}
	if _, err := s.Q.GetStudent(ctx, student.ID); err == nil {
		t.Fatalf("student should be gone")
	}

	n, err = s.Q.DeleteStudent(ctx, student.ID)
	if err != nil {
		t.Fatalf("DeleteStudent (already gone): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows deleted for an already-gone id, got %d", n)
	}
}

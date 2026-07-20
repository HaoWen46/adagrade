package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

func TestPublishPreview_CoverageGate(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)

	// No official grade yet, but the answer has a page -> blocked.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO submissions (assessment_id, student_id, original_filename, source_ref, source_sha256, page_count)
		VALUES ($1, $2, 'f.pdf', 'ref', 'sha', 1)`, f.AssessmentID, f.StudentID); err != nil {
		t.Fatalf("seed submission: %v", err)
	}
	var subID int64
	if err := s.Pool.QueryRow(ctx, `SELECT id FROM submissions WHERE assessment_id = $1`, f.AssessmentID).Scan(&subID); err != nil {
		t.Fatalf("get submission id: %v", err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO answer_pages (answer_id, page_index, submission_id, pdf_page_index, image_ref, image_sha256, image_width, image_height)
		VALUES ($1, 0, $2, 0, 'img', 'imgsha', 100, 100)`, f.AnswerID, subID); err != nil {
		t.Fatalf("seed answer_pages: %v", err)
	}

	preview, err := s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview: %v", err)
	}
	if preview.Publishable() {
		t.Fatalf("expected not publishable (missing official grade), got publishable")
	}
	if len(preview.Blockers) != 1 {
		t.Fatalf("expected 1 blocker, got %d: %+v", len(preview.Blockers), preview.Blockers)
	}
	if preview.Blocked != 1 || preview.Graded != 0 {
		t.Fatalf("unexpected counts: %+v", preview)
	}

	// Now set an official grade -> coverage gate opens.
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")

	preview, err = s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview after grading: %v", err)
	}
	if !preview.Publishable() {
		t.Fatalf("expected publishable after official grade set, got blockers %+v", preview.Blockers)
	}
	if preview.Graded != 1 || preview.Blocked != 0 {
		t.Fatalf("unexpected counts after grading: %+v", preview)
	}
	if len(preview.SnapshotInputs) != 1 {
		t.Fatalf("expected 1 snapshot input row, got %d", len(preview.SnapshotInputs))
	}
	if store.NumStr(preview.SnapshotInputs[0].Total) != "8" {
		t.Fatalf("snapshot input total = %q, want 8", store.NumStr(preview.SnapshotInputs[0].Total))
	}
}

// TestPublishPreview_RosterGapBlocksThenOpens is the regression test for the
// roster-add-after-ingest coverage gap: an active roster student with ZERO answers
// rows for the assessment (e.g. added to the roster after ingest ran) must fail
// closed — the coverage gate reports them as a distinct "not_ingested" blocker and
// refuses to publish, rather than silently passing (PublishCoverageCounts/
// PublishBlockers both start FROM answers, so a student with no answers row never
// appears there without a roster-side LEFT JOIN). After the student's answers are
// materialized (however ingest would do it — here, EnsureAnswer directly, mirroring
// mustFixture), the gate opens.
func TestPublishPreview_RosterGapBlocksThenOpens(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "8")

	// mustFixture's one student is already fully covered (officially graded). Add a
	// second roster student who is NOT ingested at all for this assessment: no
	// answers row exists for them.
	gapStudent, err := s.Q.UpsertStudent(ctx, db.UpsertStudentParams{
		StudentID: t.Name() + "-gap-student", Name: "Gap Student", Email: "gap@example.test",
	})
	if err != nil {
		t.Fatalf("UpsertStudent (gap): %v", err)
	}

	preview, err := s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview: %v", err)
	}
	if preview.Publishable() {
		t.Fatalf("expected not publishable (roster gap student has no answers row), got publishable")
	}
	if preview.NotIngested != 1 {
		t.Fatalf("expected NotIngested=1, got %+v", preview)
	}
	var gapBlocker *db.PublishBlockersRow
	for i := range preview.Blockers {
		if preview.Blockers[i].Kind == "not_ingested" {
			gapBlocker = &preview.Blockers[i]
		}
	}
	if gapBlocker == nil {
		t.Fatalf("expected a not_ingested blocker in %+v", preview.Blockers)
	}
	if gapBlocker.StudentExternalID != gapStudent.StudentID {
		t.Fatalf("not_ingested blocker student = %q, want %q", gapBlocker.StudentExternalID, gapStudent.StudentID)
	}

	// Materialize the gap student's answer for the assessment's one problem (the
	// simplest ingest-equivalent fixture, mirroring mustFixture's own EnsureAnswer
	// call) -> the gate must open.
	if _, err := s.Q.EnsureAnswer(ctx, db.EnsureAnswerParams{
		AssessmentID: f.AssessmentID, StudentID: gapStudent.ID, ProblemID: f.ProblemID,
	}); err != nil {
		t.Fatalf("EnsureAnswer (gap): %v", err)
	}

	preview, err = s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview after ingest: %v", err)
	}
	if preview.NotIngested != 0 {
		t.Fatalf("expected NotIngested=0 after materializing the gap student's answer, got %+v", preview)
	}
	// The new answer has no submission (no pages) -> no_submission, not blocked; the
	// gate should now be fully open (the original student was already graded above).
	if !preview.Publishable() {
		t.Fatalf("expected publishable once the roster gap is closed, got blockers %+v", preview.Blockers)
	}
}

func TestPublishPreview_NoSubmissionIsNotBlocked(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	// No pages uploaded at all, no official grade -> no_submission, not blocked.

	preview, err := s.PublishPreview(ctx, f.AssessmentID)
	if err != nil {
		t.Fatalf("PublishPreview: %v", err)
	}
	if !preview.Publishable() {
		t.Fatalf("expected publishable (all no_submission), got blockers %+v", preview.Blockers)
	}
	if preview.NoSubmission != 1 {
		t.Fatalf("expected NoSubmission=1, got %+v", preview)
	}
}

func TestCreatePublishBatch_SetsPublishedAtAndLocksOfficial(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")

	admin, err := s.Q.CreateUser(ctx, db.CreateUserParams{Email: "admin@example.test", Role: "admin", Active: true})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	batch, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Note:         "first publish",
		CreatedBy:    admin.ID,
		Items: []store.CreatePublishItemInput{{
			StudentID:      f.StudentID,
			Snapshot:       []byte(`{"total":"9"}`),
			RecipientEmail: "student@example.test",
			RegradeToken:   "tok-1",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	if batch.ID == 0 {
		t.Fatalf("expected non-zero batch id")
	}
	if len(items) != 1 || items[0].EmailStatus != "pending" {
		t.Fatalf("unexpected items: %+v", items)
	}

	// answers.published_at must now be set for the whole assessment (spec §2).
	var publishedAt any
	if err := s.Pool.QueryRow(ctx, `SELECT published_at FROM answers WHERE id = $1`, f.AnswerID).Scan(&publishedAt); err != nil {
		t.Fatalf("query published_at: %v", err)
	}
	if publishedAt == nil {
		t.Fatalf("expected published_at to be set after CreatePublishBatch")
	}

	// ListPublishBatches / ListPublishItems round-trip.
	batches, err := s.ListPublishBatches(ctx, f.AssessmentID)
	if err != nil || len(batches) != 1 {
		t.Fatalf("ListPublishBatches: got %d err %v", len(batches), err)
	}
	listedItems, err := s.ListPublishItems(ctx, batch.ID)
	if err != nil || len(listedItems) != 1 {
		t.Fatalf("ListPublishItems: got %d err %v", len(listedItems), err)
	}
}

func TestUpdatePublishItemEmailStatus_AndByStatus(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "5")

	batch, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID:      f.StudentID,
			Snapshot:       []byte(`{}`),
			RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}

	if err := s.UpdatePublishItemEmailStatus(ctx, items[0].ID, "failed", "", "smtp: connection refused"); err != nil {
		t.Fatalf("UpdatePublishItemEmailStatus: %v", err)
	}

	failed, err := s.PublishItemsByStatus(ctx, batch.ID, "failed")
	if err != nil || len(failed) != 1 {
		t.Fatalf("PublishItemsByStatus(failed): got %d err %v", len(failed), err)
	}
	if failed[0].Error.String != "smtp: connection refused" {
		t.Fatalf("unexpected error text: %+v", failed[0].Error)
	}

	sent, err := s.PublishItemsByStatus(ctx, batch.ID, "sent")
	if err != nil || len(sent) != 0 {
		t.Fatalf("PublishItemsByStatus(sent): got %d err %v", len(sent), err)
	}
}

func TestSupersedePublishBatch_ClearsPublishedAtAndStampsSuperseded(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "7")

	batch, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}

	if err := s.SupersedePublishBatch(ctx, batch.ID, 0); err != nil {
		t.Fatalf("SupersedePublishBatch: %v", err)
	}

	var publishedAt any
	if err := s.Pool.QueryRow(ctx, `SELECT published_at FROM answers WHERE id = $1`, f.AnswerID).Scan(&publishedAt); err != nil {
		t.Fatalf("query published_at: %v", err)
	}
	if publishedAt != nil {
		t.Fatalf("expected published_at cleared after unpublish, got %v", publishedAt)
	}

	var supersededAt any
	if err := s.Pool.QueryRow(ctx, `SELECT superseded_at FROM publish_batches WHERE id = $1`, batch.ID).Scan(&supersededAt); err != nil {
		t.Fatalf("query superseded_at: %v", err)
	}
	if supersededAt == nil {
		t.Fatalf("expected superseded_at to be stamped")
	}
}

// TestSupersedePublishBatch_LatestBatchSucceeds exercises the latest-batch guard on
// the happy path with two real batches: superseding the second (newer, still-live)
// batch must succeed, proving the guard doesn't just degenerate to "no other batches
// exist" the way the single-batch test above does.
func TestSupersedePublishBatch_LatestBatchSucceeds(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "7")

	batch1, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch (1): %v", err)
	}
	if err := s.SupersedePublishBatch(ctx, batch1.ID, 0); err != nil {
		t.Fatalf("SupersedePublishBatch (1): %v", err)
	}

	// Republish (still-official grade from above) -> batch2 is now the latest
	// non-superseded batch.
	batch2, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch (2): %v", err)
	}

	if err := s.SupersedePublishBatch(ctx, batch2.ID, 0); err != nil {
		t.Fatalf("SupersedePublishBatch on latest batch: %v", err)
	}

	var supersededAt any
	if err := s.Pool.QueryRow(ctx, `SELECT superseded_at FROM publish_batches WHERE id = $1`, batch2.ID).Scan(&supersededAt); err != nil {
		t.Fatalf("query superseded_at: %v", err)
	}
	if supersededAt == nil {
		t.Fatalf("expected batch2.superseded_at to be stamped")
	}

	var publishedAt any
	if err := s.Pool.QueryRow(ctx, `SELECT published_at FROM answers WHERE id = $1`, f.AnswerID).Scan(&publishedAt); err != nil {
		t.Fatalf("query published_at: %v", err)
	}
	if publishedAt != nil {
		t.Fatalf("expected published_at cleared after superseding the latest batch, got %v", publishedAt)
	}
}

// TestSupersedePublishBatch_AlreadySupersededRejected is the F2 guard under the M2
// single-live-batch invariant: two live batches can no longer coexist (migration 0021),
// so the reachable "not the latest live batch" case is superseding a batch that is
// ALREADY superseded (a stale/double unpublish). It must fail with
// store.ErrNotLatestBatch and leave state untouched.
func TestSupersedePublishBatch_AlreadySupersededRejected(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "7")

	batch1, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch (1): %v", err)
	}
	if err := s.SupersedePublishBatch(ctx, batch1.ID, 0); err != nil {
		t.Fatalf("SupersedePublishBatch (1): %v", err)
	}

	// Superseding the already-superseded batch1 again is rejected.
	err = s.SupersedePublishBatch(ctx, batch1.ID, 0)
	if !errors.Is(err, store.ErrNotLatestBatch) {
		t.Fatalf("SupersedePublishBatch(already-superseded batch) = %v, want store.ErrNotLatestBatch", err)
	}
}

// TestCreatePublishBatch_SecondLiveBatchRejected is the M2 regression: the migration-
// 0021 partial unique index rejects a second non-superseded batch for the same
// assessment, and store.CreatePublishBatch maps the unique-violation to
// store.ErrLiveBatchExists (which the publish service turns into ErrAlreadyPublished).
// This is what makes a racing second publish lose cleanly instead of creating an
// ambiguous second live batch.
func TestCreatePublishBatch_SecondLiveBatchRejected(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "7")

	if _, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	}); err != nil {
		t.Fatalf("CreatePublishBatch (1): %v", err)
	}

	// A second live batch for the same assessment must be rejected by the unique index.
	_, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if !errors.Is(err, store.ErrLiveBatchExists) {
		t.Fatalf("CreatePublishBatch (2, second live) = %v, want store.ErrLiveBatchExists", err)
	}
}

// TestCreatePublishBatch_AttachmentDefaultsToNone covers migration 0022's default:
// callers that don't set Attachment (the pre-existing behaviour, before this field
// existed) must still get "none", not an empty string or a constraint violation.
func TestCreatePublishBatch_AttachmentDefaultsToNone(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")

	batch, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	if batch.Attachment != "none" {
		t.Fatalf("Attachment = %q, want %q", batch.Attachment, "none")
	}
	if batch.Zip {
		t.Fatalf("Zip = true, want false")
	}
}

// TestCreatePublishBatch_AttachmentSettingsPersist covers spec §9's publish request
// {attachment, zip} fields (D42/D46) round-tripping through CreatePublishBatch and
// GetPublishItemForResend — the resend action reads these off the parent batch.
func TestCreatePublishBatch_AttachmentSettingsPersist(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")

	batch, items, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Attachment:   "compressed",
		Zip:          true,
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err != nil {
		t.Fatalf("CreatePublishBatch: %v", err)
	}
	if batch.Attachment != "compressed" || !batch.Zip {
		t.Fatalf("unexpected batch attachment settings: %+v", batch)
	}

	resend, err := s.GetPublishItemForResend(ctx, items[0].ID)
	if err != nil {
		t.Fatalf("GetPublishItemForResend: %v", err)
	}
	if resend.Attachment != "compressed" || !resend.Zip {
		t.Fatalf("GetPublishItemForResend attachment settings = %+v, want compressed/true", resend)
	}
	if resend.StudentExternalID == "" || resend.AssessmentName == "" {
		t.Fatalf("GetPublishItemForResend missing joined identity fields: %+v", resend)
	}
}

// TestCreatePublishBatch_InvalidAttachmentRejected covers migration 0022's CHECK
// constraint: only 'none'/'compressed'/'original' are valid attachment values.
func TestCreatePublishBatch_InvalidAttachmentRejected(t *testing.T) {
	ctx := context.Background()
	s := storetest.Fresh(t)
	f := mustFixture(t, s)
	mustOfficialRecord(t, s, f.AnswerID, f.RubricVersionID, "9")

	_, _, err := s.CreatePublishBatch(ctx, store.CreatePublishBatchParams{
		AssessmentID: f.AssessmentID,
		Attachment:   "pdf-please",
		Items: []store.CreatePublishItemInput{{
			StudentID: f.StudentID, Snapshot: []byte(`{}`), RecipientEmail: "student@example.test",
		}},
	})
	if err == nil {
		t.Fatal("expected an error for an invalid attachment value, got nil")
	}
}

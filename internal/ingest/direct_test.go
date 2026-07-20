package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// recordingDirectEnqueue installs a recording EnqueueDirectIngest on the fixture's
// service and returns a pointer to the accumulated ids.
func recordingDirectEnqueue(f fx) *[]int64 {
	ids := &[]int64{}
	f.svc.EnqueueDirectIngest = func(_ context.Context, _ pgx.Tx, batch []int64) error {
		*ids = append(*ids, batch...)
		return nil
	}
	return ids
}

// drainDirect runs IngestDirectUpload for each recorded id (as the worker would).
func drainDirect(t *testing.T, f fx, ids []int64) {
	t.Helper()
	for _, id := range ids {
		if err := f.svc.IngestDirectUpload(f.ctx, id, false); err != nil {
			t.Fatalf("IngestDirectUpload(%d): %v", id, err)
		}
	}
}

func TestStageDirectUpload_HappyPathLandsSubmission(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)

	id, rejected, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-fake"), Kind: "pdf", Force: false, Actor: 0,
	})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if rejected != "" {
		t.Fatalf("valid upload should not be rejected synchronously, got %q", rejected)
	}
	if id == 0 {
		t.Fatal("stage should return a row id")
	}
	// The row was created, blob stored, and the ingest job enqueued in the same tx.
	row, err := f.st.Q.GetDirectUpload(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.FinishedAt.Valid {
		t.Error("row should be pending (finished_at NULL) before the job runs")
	}
	ok, _ := f.svc.Blobs.Exists(f.ctx, row.SourceRef)
	if !ok {
		t.Errorf("staged bytes missing from blobstore: %s", row.SourceRef)
	}
	if len(*ids) != 1 || (*ids)[0] != id {
		t.Fatalf("stage should enqueue the ingest job for id %d, got %v", id, *ids)
	}

	// Run the job: it ingests, lands a submission, and records the result.
	drainDirect(t, f, *ids)
	done, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if !done.FinishedAt.Valid {
		t.Error("finished_at should be set after the job runs")
	}
	if done.Status.String != "ingested" {
		t.Errorf("status: got %q want ingested", done.Status.String)
	}
	if !done.SubmissionID.Valid {
		t.Error("submission_id should be linked after a successful ingest")
	}
	if done.Error.Valid && done.Error.String != "" {
		t.Errorf("no error expected: %q", done.Error.String)
	}
	// The linked submission is real.
	sub, err := f.st.Q.GetSubmission(f.ctx, done.SubmissionID.Int64)
	if err != nil || sub.StudentID == 0 {
		t.Errorf("linked submission should exist: %+v %v", sub, err)
	}
	if !done.StartedAt.Valid {
		t.Error("started_at should be stamped by the job")
	}
}

// A direct whole-assessment PDF is positional: one PDF page maps to one problem.
// An extra page has no representable destination. Reject before any submission or
// page row is written instead of silently discarding the trailing source page.
func TestIngestDirectUpload_WholeAssessmentExtraPageRejected(t *testing.T) {
	f := setup(t)
	f.svc.Renderer = render.NewFake(3)
	ids := recordingDirectEnqueue(f)

	id, rejected, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-extra-page"), Kind: "pdf",
	})
	if err != nil || rejected != "" {
		t.Fatalf("stage: id=%d rejected=%q err=%v", id, rejected, err)
	}
	drainDirect(t, f, *ids)

	done, err := f.st.Q.GetDirectUpload(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !done.FinishedAt.Valid || done.Status.String != "rejected" {
		t.Fatalf("extra page must be a terminal rejected upload: %+v", done)
	}
	wantReason := "PDF has 3 pages but this assessment has 2 problems; correct the PDF so it has exactly one page per problem, then re-upload"
	if done.Reason.String != wantReason {
		t.Errorf("reason: got %q want %q", done.Reason.String, wantReason)
	}
	if done.SubmissionID.Valid {
		t.Errorf("rejected extra page must not link a submission, got %d", done.SubmissionID.Int64)
	}
	if submissions, err := f.st.Q.ListActiveSubmissions(f.ctx, f.aid); err != nil || len(submissions) != 0 {
		t.Errorf("rejected extra page must create no submission: got %d (%v)", len(submissions), err)
	}
	if pages, err := f.st.Q.ListPagesForAssessment(f.ctx, f.aid); err != nil || len(pages) != 0 {
		t.Errorf("rejected extra page must create no answer pages: got %d (%v)", len(pages), err)
	}
}

// Short PDFs remain valid partial submissions: the reconciliation report already
// exposes mapped < expected as a red mismatch so staff can correct a missed scan.
// This preserves the documented blank/skipped-page recovery workflow while the
// extra-page guard closes the actual silent-data-loss path.
func TestIngestDirectUpload_WholeAssessmentMissingPageRemainsIncomplete(t *testing.T) {
	f := setup(t)
	f.svc.Renderer = render.NewFake(1)
	ids := recordingDirectEnqueue(f)

	id, rejected, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-missing-page"), Kind: "pdf",
	})
	if err != nil || rejected != "" {
		t.Fatalf("stage: id=%d rejected=%q err=%v", id, rejected, err)
	}
	drainDirect(t, f, *ids)

	done, err := f.st.Q.GetDirectUpload(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !done.FinishedAt.Valid || done.Status.String != "ingested" || !done.SubmissionID.Valid {
		t.Fatalf("short PDF should remain an ingested partial submission: %+v", done)
	}
	pages, err := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	if err != nil || len(pages) != 1 {
		t.Fatalf("short PDF should map its one available page: got %d (%v)", len(pages), err)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, done.SubmissionID.Int64)
	if err != nil || sub.PageCount != 1 {
		t.Fatalf("partial submission must preserve its source page count: %+v (%v)", sub, err)
	}
}

func TestStageDirectUpload_EmptyOrTooLargeRejectedNoRow(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)

	// Empty file: rejected synchronously with a non-empty reason and NO row.
	id, reason, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: nil, Kind: "pdf",
	})
	if err != nil {
		t.Fatalf("stage empty: unexpected err %v", err)
	}
	if reason == "" {
		t.Error("empty file should be rejected with a reason")
	}
	if id != 0 {
		t.Errorf("empty file must not create a row, got id %d", id)
	}

	// Too-large: same treatment.
	big := make([]byte, MaxPDFBytes+1)
	id2, reason2, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: big, Kind: "pdf",
	})
	if err != nil {
		t.Fatalf("stage too-large: unexpected err %v", err)
	}
	if reason2 == "" {
		t.Error("too-large file should be rejected with a reason")
	}
	if id2 != 0 {
		t.Errorf("too-large file must not create a row, got id %d", id2)
	}

	// No jobs enqueued for either.
	if len(*ids) != 0 {
		t.Errorf("rejected uploads must not enqueue jobs, got %v", *ids)
	}
	rows, _ := f.st.Q.ListDirectUploadsForAssessment(f.ctx, db.ListDirectUploadsForAssessmentParams{AssessmentID: f.aid, Limit: 10})
	if len(rows) != 0 {
		t.Errorf("no rows should exist for rejected uploads, got %d", len(rows))
	}
}

func TestIngestDirectUpload_UnknownFilenameQuarantines(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)

	id, _, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "mystery.pdf", Data: []byte("%PDF-fake"), Kind: "pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	drainDirect(t, f, *ids)

	row, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if row.Status.String != "quarantined" {
		t.Errorf("unknown filename should quarantine: status=%q", row.Status.String)
	}
	if row.Reason.String != "unknown_student" {
		t.Errorf("reason should be preserved: got %q", row.Reason.String)
	}
	if row.SubmissionID.Valid {
		t.Error("quarantined upload should link no submission")
	}
	// A quarantine record was created (the ingest pipeline quarantined the bytes).
	open, _ := f.st.Q.ListOpenQuarantine(f.ctx, f.aid)
	if len(open) != 1 {
		t.Errorf("quarantine record should exist: got %d", len(open))
	}
}

func TestIngestDirectUpload_GradedWithoutForceRejectedReasonPreserved(t *testing.T) {
	f := setup(t)

	// Prime b01 with grading records so a plain re-upload is rejected without force.
	first := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-one"), 0, false)
	if first.Status != "ingested" {
		t.Fatal(first)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	var answerID int64
	for _, a := range answers {
		for _, pg := range pages {
			if pg.AnswerID == a.ID {
				answerID = a.ID
			}
		}
	}
	gradeAnswer(f, t, answerID, "ta-du@x.edu")

	ids := recordingDirectEnqueue(f)
	id, _, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-two"), Kind: "pdf", Force: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainDirect(t, f, *ids)

	row, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if row.Status.String != "rejected" {
		t.Errorf("graded-without-force should be rejected: status=%q", row.Status.String)
	}
	if row.Reason.String == "" {
		t.Error("rejection reason should be preserved from the ingest result")
	}
	if row.SubmissionID.Valid {
		t.Error("rejected upload should link no submission")
	}
}

func TestIngestDirectUpload_ForceReplacesGraded(t *testing.T) {
	f := setup(t)
	first := f.svc.IngestFile(f.ctx, f.aid, "b01.pdf", []byte("%PDF-one"), 0, false)
	if first.Status != "ingested" {
		t.Fatal(first)
	}
	pages, _ := f.st.Q.ListPagesForAssessment(f.ctx, f.aid)
	answers, _ := f.st.Q.ListAnswersForAssessment(f.ctx, f.aid)
	var answerID int64
	for _, a := range answers {
		for _, pg := range pages {
			if pg.AnswerID == a.ID {
				answerID = a.ID
			}
		}
	}
	gradeAnswer(f, t, answerID, "ta-du2@x.edu")

	ids := recordingDirectEnqueue(f)
	id, _, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-forced"), Kind: "pdf", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	drainDirect(t, f, *ids)

	row, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if row.Status.String != "ingested" {
		t.Errorf("force upload over graded should ingest: status=%q reason=%q", row.Status.String, row.Reason.String)
	}
	if !row.SubmissionID.Valid {
		t.Error("forced ingest should link a submission")
	}
}

func TestIngestDirectUpload_RedeliverySkip(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)
	id, _, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-fake"), Kind: "pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	drainDirect(t, f, *ids)
	first, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if !first.FinishedAt.Valid || !first.SubmissionID.Valid {
		t.Fatal("first run should finish and link a submission")
	}

	// Redelivery: a second IngestDirectUpload for the same id must be a no-op — it
	// must NOT create a second submission or change the recorded result.
	if err := f.svc.IngestDirectUpload(f.ctx, id, false); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	second, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if second.SubmissionID.Int64 != first.SubmissionID.Int64 {
		t.Errorf("redelivery must not relink submission: %d -> %d", first.SubmissionID.Int64, second.SubmissionID.Int64)
	}
}

func TestStageDirectUpload_ImageKeepsExtension(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)
	png := pngBytes(t, 200, 150)
	id, reason, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.png", Data: png, Kind: "image",
	})
	if err != nil || reason != "" {
		t.Fatalf("stage image: err=%v reason=%q", err, reason)
	}
	row, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if row.SourceKind != "image" {
		t.Errorf("source_kind: got %q want image", row.SourceKind)
	}
	drainDirect(t, f, *ids)
	done, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if done.Status.String != "ingested" {
		t.Errorf("image direct upload should ingest: %q (%q)", done.Status.String, done.Reason.String)
	}
}

// TestIngestDirectUpload_ShutdownCancellationNotTerminal is the F17 direct-upload
// half: a SIGTERM that cancels Blobs.Get mid-flight surfaces as context.Canceled.
// Even on the FINAL attempt, IngestDirectUpload must NOT write "source bytes
// unreadable" (a misleading terminal state, since the bytes are actually fine — the
// read was just interrupted). It must return the error so River records a plain
// attempt and the worker snoozes it on shutdown, leaving the row to be retried
// cleanly on the next start.
func TestIngestDirectUpload_ShutdownCancellationNotTerminal(t *testing.T) {
	f := setup(t)
	ids := recordingDirectEnqueue(f)
	id, _, err := f.svc.StageDirectUpload(f.ctx, f.aid, DirectUploadInput{
		Filename: "b01.pdf", Data: []byte("%PDF-fake"), Kind: "pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = ids

	f.svc.Blobs = getCanceledBlobs{f.svc.Blobs}
	gotErr := f.svc.IngestDirectUpload(f.ctx, id, true /* finalAttempt */)
	if gotErr == nil {
		t.Fatal("cancellation should be RETURNED (so River records a plain attempt), got nil")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("returned error should wrap context.Canceled, got %v", gotErr)
	}
	row, _ := f.st.Q.GetDirectUpload(f.ctx, id)
	if row.Error.Valid {
		t.Errorf("interruption must NOT write the terminal error column, got %q", row.Error.String)
	}
	if row.FinishedAt.Valid {
		t.Error("interruption must NOT mark the row finished")
	}
	if row.SubmissionID.Valid {
		t.Error("interruption must NOT link a submission")
	}
}

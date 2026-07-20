// Promote-time race coverage (intake-races fixes, 2026-07-11): PromotePage vs
// a concurrent reassign (the success-path link must be conditional on the
// assignment being unchanged), the mirror hole in the manual mutations (an
// unassign racing an in-flight promote link must 409, not silently strand a
// promoted page), and a finalize re-run promoting a stale assigned page over a
// newer Submissions-tab upload (must park as a conflict, never supersede).
package scan

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// hookBlobs wraps a blobstore.Store, invoking hook once before the first Put.
// Ingest stores every blob BEFORE opening its row transaction (F15), so the
// first Put is a deterministic injection point for "a TA mutated the page
// while the promote job's ingest was in flight".
type hookBlobs struct {
	blobstore.Store
	hook  func()
	fired bool
}

func (h *hookBlobs) Put(ctx context.Context, key string, r io.Reader) (string, int64, error) {
	if !h.fired && h.hook != nil {
		h.fired = true
		h.hook()
	}
	return h.Store.Put(ctx, key, r)
}

// TestPromotePage_ReassignDuringPromotion_AbortsAndRepromotes pins the
// promote/reassign race: the TA reassigns the page to the correct student
// while the promote job is in flight (after its eligibility snapshot, before
// ingest's transaction). The stale promote must NOT leave a live submission
// under the old (wrong) student — the whole ingest aborts — and a re-run (the
// queue's retry) must promote the corrected cell.
func TestPromotePage_ReassignDuringPromotion_AbortsAndRepromotes(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	st1 := student(f, t, "B11902001")
	st2 := student(f, t, "B11902002")
	prob := problemByNumber(f, t, 1)

	// Mid-flight reassign: the UI keeps assignment mutations enabled during
	// promotion, so this is a legal concurrent operator action.
	inner := f.svc.Ingest.Blobs
	f.svc.Ingest.Blobs = &hookBlobs{Store: inner, hook: func() {
		if err := f.svc.AssignPage(f.ctx, page.ID, st2.ID, prob.ID, 1); err != nil {
			t.Errorf("mid-flight reassign must succeed (page is not promoted yet): %v", err)
		}
	}}
	t.Cleanup(func() { f.svc.Ingest.Blobs = inner })

	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err == nil {
		t.Fatal("promote must not report success when the assignment changed mid-flight")
	}

	// No submission may survive under the old (wrong) student.
	wrong, err := f.st.Q.ListLiveSubmissionsForStudent(f.ctx, db.ListLiveSubmissionsForStudentParams{
		AssessmentID: f.aid, StudentID: st1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrong) != 0 {
		t.Fatalf("stale promote left %d live submission(s) under the old student", len(wrong))
	}

	// The page keeps its corrected assignment: unpromoted, unerrored, unparked.
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st2.ID {
		t.Fatalf("page must keep the corrected assignment, got %+v", got)
	}
	if got.SubmissionID.Valid {
		t.Fatalf("page must not be linked to the aborted submission: %+v", got)
	}
	if got.Error.Valid {
		t.Fatalf("aborted promote must not stamp a page error, got %q", got.Error.String)
	}
	if got.ParkedReason.Valid {
		t.Fatalf("aborted promote must not park the page: %+v", got)
	}

	// The queue retries the failed job: the retry promotes the corrected cell.
	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatalf("retry under the new assignment must promote: %v", err)
	}
	promoted, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !promoted.SubmissionID.Valid {
		t.Fatalf("corrected cell must promote on retry: %+v", promoted)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, promoted.SubmissionID.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if sub.StudentID != st2.ID {
		t.Fatalf("submission student = %d, want the corrected student %d", sub.StudentID, st2.ID)
	}
	if !sub.ProblemID.Valid || sub.ProblemID.Int64 != prob.ID {
		t.Fatalf("submission problem = %+v, want %d", sub.ProblemID, prob.ID)
	}
}

// TestUnassignPage_DuringInFlightPromotion_ErrPagePromoted pins the mirror
// hole: UnassignPage's promoted-guard runs on a pre-tx snapshot, so a promote
// job that links its submission between that snapshot and the unassign's own
// transaction used to race — the unassign would clear the assignment of a
// just-promoted page. The in-tx re-check must turn this into a clean 409
// (*ErrPagePromoted) and leave the promoted page fully intact.
func TestUnassignPage_DuringInFlightPromotion_ErrPagePromoted(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	st1 := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	// The submission an in-flight promote job is about to link.
	data, err := f.svc.readAll(f.ctx, page.ImageRef.String)
	if err != nil {
		t.Fatal(err)
	}
	res := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.jpg", Data: data, Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture promotion: %+v", res)
	}

	// Simulate the promote link transaction in flight: the row is updated and
	// locked, the commit still pending.
	tx, err := f.st.Pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	n, err := f.st.Q.WithTx(tx).LinkScanPagePromotion(f.ctx, db.LinkScanPagePromotionParams{
		ID:                page.ID,
		SubmissionID:      pgtype.Int8{Int64: res.SubmissionID, Valid: true},
		AssignedStudentID: pgtype.Int8{Int64: st1.ID, Valid: true},
		AssignedProblemID: pgtype.Int8{Int64: prob.ID, Valid: true},
	})
	if err != nil || n != 1 {
		t.Fatalf("fixture link: n=%d err=%v", n, err)
	}

	// UnassignPage concurrently: its pre-tx snapshot still sees the unlinked
	// row, then its own transaction blocks on the promote's row lock.
	done := make(chan error, 1)
	go func() { done <- f.svc.UnassignPage(context.Background(), page.ID, 1) }()
	time.Sleep(150 * time.Millisecond) // let it pass the snapshot check and block
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done

	var promoted *ErrPagePromoted
	if !errors.As(err, &promoted) {
		t.Fatalf("want *ErrPagePromoted, got %T: %v", err, err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st1.ID {
		t.Fatalf("promoted page must keep its assignment, got %+v", got)
	}
	if !got.SubmissionID.Valid || got.SubmissionID.Int64 != res.SubmissionID {
		t.Fatalf("promoted page must keep its submission link, got %+v", got)
	}
}

// TestPromotePage_LiveSubmissionCoversCell_ParksInsteadOfSuperseding pins the
// finalize-supersede hole: a stale assigned-but-unpromoted page (e.g. a blurry
// page the TA replaced by uploading the student's corrected file via the
// Submissions tab) must NOT be silently promoted over the newer upload by a
// later assessment-wide finalize. PromotePage parks it as a submission-backed
// conflict (mirroring identify's D65 semantics) for the TA to adjudicate.
func TestPromotePage_LiveSubmissionCoversCell_ParksInsteadOfSuperseding(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	st1 := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	// The TA uploads the corrected answer via the Submissions tab AFTER the
	// scan page was assigned (direct ingest does not know about scan pages).
	upload := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.png", Data: pngBytes(t, 400, 300), Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if upload.Status != "ingested" {
		t.Fatalf("fixture upload: %+v", upload)
	}
	ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{
		AssessmentID: f.aid, StudentID: st1.ID, ProblemID: prob.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.st.Q.ListAnswerPages(f.ctx, ans.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("upload pages: %d, %v", len(before), err)
	}

	// A later finalize enqueues the still-assigned-unpromoted page; drive it.
	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatal(err)
	}

	// The newer upload stays live and its pages are untouched.
	sub, err := f.st.Q.GetSubmission(f.ctx, upload.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.SupersededBy.Valid || sub.RetractedAt.Valid {
		t.Fatalf("newer upload must stay live, got %+v", sub)
	}
	after, err := f.st.Q.ListAnswerPages(f.ctx, ans.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ImageRef != before[0].ImageRef || after[0].SubmissionID != before[0].SubmissionID {
		t.Fatalf("upload's answer pages must be untouched: before %+v, after %+v", before, after)
	}

	// The stale page parks as a submission-backed conflict, unassigned (like
	// identify's parks), with the contested cell captured for adjudication.
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubmissionID.Valid {
		t.Fatalf("stale page must not promote: %+v", got)
	}
	if got.ParkedReason.String != "conflict" || got.ParkedAgainst.Valid {
		t.Fatalf("want submission-backed conflict park, got %+v", got)
	}
	if got.AssignedStudentID.Valid {
		t.Fatalf("parked page must be unassigned (mirrors identify's parks): %+v", got)
	}
	if !got.ParkStudentID.Valid || got.ParkStudentID.Int64 != st1.ID ||
		!got.ParkProblemID.Valid || got.ParkProblemID.Int64 != prob.ID {
		t.Fatalf("park must capture the contested cell, got %+v", got)
	}
	if got.Error.Valid {
		t.Fatalf("park is an adjudication outcome, not an error: %q", got.Error.String)
	}
}

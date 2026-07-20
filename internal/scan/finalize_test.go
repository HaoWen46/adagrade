package scan

import (
	"errors"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// addGradingRecord fabricates a minimal model grading record on the given answer
// so ingest's guard sees "student already has grading records" (rejects without
// force, accepts with force). It creates a throwaway rubric version to satisfy
// the NOT NULL FK. source='model' lets created_by stay NULL (needs only a
// model_id). Mirrors the pattern from the old internal/scan/scan_test.go
// (commit 54e036d).
func addGradingRecord(f fx, t *testing.T, problemID, answerID int64) {
	t.Helper()
	inc, _ := store.Num("0.5")
	rv, err := f.st.Q.CreateRubricVersion(f.ctx, db.CreateRubricVersionParams{
		ProblemID: problemID, ScoreIncrement: inc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx,
		`INSERT INTO grading_records (answer_id, source, model_id, rubric_version_id, criterion_scores)
		 VALUES ($1, 'model', 'test-model', $2, '[]'::jsonb)`,
		answerID, rv.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFinalize_MissingCellsGate(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	// 2 students x 3 problems, nothing assigned -> 6 missing cells.

	_, err := f.svc.Finalize(f.ctx, f.aid, false, 1)
	if err == nil {
		t.Fatal("want *ErrMissingUnacknowledged, got nil")
	}
	var missing *ErrMissingUnacknowledged
	if !errors.As(err, &missing) {
		t.Fatalf("want *ErrMissingUnacknowledged, got %T: %v", err, err)
	}
	if missing.Count != 6 {
		t.Fatalf("Count = %d, want 6", missing.Count)
	}

	report, err := f.svc.Finalize(f.ctx, f.aid, true, 1)
	if err != nil {
		t.Fatalf("ack=true must succeed, got: %v", err)
	}
	if report.MissingCells != 6 {
		t.Fatalf("MissingCells = %d, want 6", report.MissingCells)
	}
	if report.Enqueued != 0 {
		t.Fatalf("Enqueued = %d, want 0", report.Enqueued)
	}
}

func TestFinalize_EnqueuesAssignedUnpromoted(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid {
		t.Fatalf("fixture setup: page did not auto-assign: %+v", got)
	}

	report, err := f.svc.Finalize(f.ctx, f.aid, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Enqueued != 1 {
		t.Fatalf("Enqueued = %d, want 1", report.Enqueued)
	}
	if len(*f.promotes) != 1 {
		t.Fatalf("promotes recorded = %d, want 1", len(*f.promotes))
	}
	item := (*f.promotes)[0]
	if item.PageID != page.ID {
		t.Fatalf("PageID = %d, want %d", item.PageID, page.ID)
	}

	// Drive the recorded promote item.
	if err := f.svc.PromotePage(f.ctx, item.PageID, item.Force, item.Actor, false); err != nil {
		t.Fatal(err)
	}
	promoted, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !promoted.SubmissionID.Valid {
		t.Fatalf("page must be promoted: %+v", promoted)
	}

	// Re-run Finalize: incremental, only the already-promoted page counts.
	*f.promotes = (*f.promotes)[:0]
	report2, err := f.svc.Finalize(f.ctx, f.aid, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report2.Enqueued != 0 {
		t.Fatalf("Enqueued = %d, want 0 on re-run", report2.Enqueued)
	}
	if report2.AlreadyPromoted != 1 {
		t.Fatalf("AlreadyPromoted = %d, want 1", report2.AlreadyPromoted)
	}
}

func TestPromotePage_CreatesPerProblemImageSubmission(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q2", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 2)

	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatal(err)
	}

	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SubmissionID.Valid {
		t.Fatalf("page.submission_id must be set: %+v", got)
	}
	sub, err := f.st.Q.GetSubmission(f.ctx, got.SubmissionID.Int64)
	if err != nil {
		t.Fatal(err)
	}
	if sub.SourceKind != "image" {
		t.Fatalf("source_kind = %q, want image", sub.SourceKind)
	}
	if !sub.ProblemID.Valid || sub.ProblemID.Int64 != prob.ID {
		t.Fatalf("submission.problem_id = %+v, want %d", sub.ProblemID, prob.ID)
	}

	ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{
		AssessmentID: f.aid, StudentID: st.ID, ProblemID: prob.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListAnswerPages(f.ctx, ans.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("answer_pages for (student, problem) = %d, want 1", len(pages))
	}
}

// TestPromotePage_WholeAssessmentSubmissionCoversCell_Parks: a live
// whole-assessment submission covers EVERY problem for the student
// positionally, so promoting an assigned page into any of those cells must
// park it as a submission-backed conflict (D65 at promote time) rather than
// silently ingesting alongside/over the upload. (Ingest's own scoped-branch
// coexistence behavior stays pinned by TestIngest_WholeThenPerProblem_Ungraded
// in internal/ingest.) The page can only reach assigned-while-covered via a
// direct row write here because identify and manual assign both already
// refuse submission-covered cells — the live hole this guards is a
// Submissions-tab upload landing AFTER the page was assigned.
func TestPromotePage_WholeAssessmentSubmissionCoversCell_Parks(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	// A whole-assessment submission via IngestFile (fake renderer: 1 page ->
	// maps onto the first problem positionally).
	res := f.svc.Ingest.IngestFile(f.ctx, f.aid, "B11902001.pdf", []byte("%PDF-1 whole"), 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture whole-assessment submission: %+v", res)
	}
	wholeSubID := res.SubmissionID

	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 2)
	if _, err := f.st.Pool.Exec(f.ctx,
		`UPDATE scan_pages SET assigned_student_id = $2, assigned_problem_id = $3 WHERE id = $1`,
		page.ID, st.ID, prob.ID); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatal(err)
	}

	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubmissionID.Valid {
		t.Fatalf("promote must not ingest under a submission-covered cell: %+v", got)
	}
	if got.ParkedReason.String != "conflict" || got.ParkedAgainst.Valid || got.AssignedStudentID.Valid {
		t.Fatalf("want unassigned submission-backed conflict park, got %+v", got)
	}
	if !got.ParkStudentID.Valid || got.ParkStudentID.Int64 != st.ID ||
		!got.ParkProblemID.Valid || got.ParkProblemID.Int64 != prob.ID {
		t.Fatalf("park must capture the contested cell, got %+v", got)
	}

	// The whole-assessment submission remains live and untouched.
	wholeSub, err := f.st.Q.GetSubmission(f.ctx, wholeSubID)
	if err != nil {
		t.Fatal(err)
	}
	if wholeSub.SupersededBy.Valid || wholeSub.RetractedAt.Valid {
		t.Fatalf("whole-assessment submission must remain live, got %+v", wholeSub)
	}
}

func TestFinalize_SeedsMaskRegions(t *testing.T) {
	f := setup(t)
	// Distinct rects per kind (unlike addRegions' identical fixture rects) so
	// this test can tell them apart by coordinate.
	studentIDRect := db.CreateIDRegionParams{AssessmentID: f.aid, Kind: "student_id", X: 0.05, Y: 0.02, W: 0.25, H: 0.06, Color: "#4a4a4a", Padding: 0.01}
	nameRect := db.CreateIDRegionParams{AssessmentID: f.aid, Kind: "name", X: 0.35, Y: 0.02, W: 0.25, H: 0.06, Color: "#4a4a4a", Padding: 0.01}
	problemRect := db.CreateIDRegionParams{AssessmentID: f.aid, Kind: "problem_id", X: 0.65, Y: 0.02, W: 0.25, H: 0.06, Color: "#4a4a4a", Padding: 0.01}
	for _, r := range []db.CreateIDRegionParams{studentIDRect, nameRect, problemRect} {
		if _, err := f.st.Q.CreateIDRegion(f.ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.svc.Finalize(f.ctx, f.aid, true, 1); err != nil {
		t.Fatal(err)
	}

	regions, err := f.st.Q.ListMaskRegions(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 2 {
		t.Fatalf("mask_regions count = %d, want 2 (student_id + name only)", len(regions))
	}
	seen := map[string]bool{}
	for _, r := range regions {
		if r.PageScope != "all" {
			t.Fatalf("page_scope = %q, want all", r.PageScope)
		}
		switch {
		case r.X == studentIDRect.X && r.Y == studentIDRect.Y && r.W == studentIDRect.W && r.H == studentIDRect.H:
			seen["student_id"] = true
		case r.X == nameRect.X && r.Y == nameRect.Y && r.W == nameRect.W && r.H == nameRect.H:
			seen["name"] = true
		case r.X == problemRect.X && r.Y == problemRect.Y && r.W == problemRect.W && r.H == problemRect.H:
			t.Fatalf("D66 violation: problem_id region must NEVER be seeded into mask_regions, got %+v", r)
		}
	}
	if !seen["student_id"] || !seen["name"] {
		t.Fatalf("want both student_id and name rects seeded, got %+v", regions)
	}

	// Second Finalize does not duplicate (idempotent).
	if _, err := f.svc.Finalize(f.ctx, f.aid, true, 1); err != nil {
		t.Fatal(err)
	}
	regions2, err := f.st.Q.ListMaskRegions(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions2) != 2 {
		t.Fatalf("mask_regions count after second finalize = %d, want 2 (no duplicates)", len(regions2))
	}
}

func TestPromotePage_ForcePromoteFlagPassesForce(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	// Promote once to create the incumbent submission, then grade it.
	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SubmissionID.Valid {
		t.Fatalf("fixture setup: page must be promoted first: %+v", got)
	}
	ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{
		AssessmentID: f.aid, StudentID: st.ID, ProblemID: prob.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	addGradingRecord(f, t, prob.ID, ans.ID)

	// A crash-window style re-promotion: clear the page's submission link (as
	// ResolveConflict's replace path would after retracting) and retract the
	// graded incumbent submission directly so ingest's re-upload guard is the
	// only thing standing in the way.
	if err := f.svc.Ingest.RetractSubmission(f.ctx, got.SubmissionID.Int64, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{ID: page.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Q.SetScanPageForcePromote(f.ctx, db.SetScanPageForcePromoteParams{ID: page.ID, ForcePromote: true}); err != nil {
		t.Fatal(err)
	}

	// PromotePage with force=false must still replace: page.ForcePromote
	// carries the force through to ingest (force || page.ForcePromote).
	if err := f.svc.PromotePage(f.ctx, page.ID, false, 1, false); err != nil {
		t.Fatal(err)
	}
	final, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !final.SubmissionID.Valid {
		t.Fatalf("force_promote must let ingest replace over the graded incumbent, got %+v", final)
	}
	if final.Error.Valid {
		t.Fatalf("want no promotion error, got %q", final.Error.String)
	}
	if final.SubmissionID.Int64 == got.SubmissionID.Int64 {
		t.Fatal("want a NEW submission id (old one was retracted)")
	}
}

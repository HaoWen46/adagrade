package scan

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

func TestAssignPage_EmptyCellAssigns(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	if err := f.svc.AssignPage(f.ctx, page.ID, st.ID, prob.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st.ID {
		t.Fatalf("student not assigned: %+v", got)
	}
	if !got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob.ID {
		t.Fatalf("problem not assigned: %+v", got)
	}
	if !got.AssignedBy.Valid || got.AssignedBy.Int64 != 1 {
		t.Fatalf("manual assign must record assigned_by = actor, got %+v", got.AssignedBy)
	}
}

func TestAssignPage_OccupiedCellErrCellOccupied(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	if err := f.svc.AssignPage(f.ctx, first.ID, st.ID, prob.ID, 1); err != nil {
		t.Fatal(err)
	}
	err := f.svc.AssignPage(f.ctx, second.ID, st.ID, prob.ID, 1)
	if err == nil {
		t.Fatal("want *ErrCellOccupied, got nil")
	}
	var occ *ErrCellOccupied
	if !errors.As(err, &occ) {
		t.Fatalf("want *ErrCellOccupied, got %T: %v", err, err)
	}
	if occ.IncumbentPageID != first.ID {
		t.Fatalf("IncumbentPageID = %d, want %d", occ.IncumbentPageID, first.ID)
	}
	// Both pages were rendered from identical source content by the fake
	// renderer (same page index), so the incumbent should read as a duplicate.
	p1, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ImageSha256.String != p2.ImageSha256.String {
		t.Skip("fake renderer no longer deterministic; adjust fixture")
	}
	if !occ.Duplicate {
		t.Fatalf("want Duplicate flag set for identical content, got %+v", occ)
	}
	// incumbent untouched by the failed assign
	keep, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !keep.AssignedStudentID.Valid || keep.AssignedStudentID.Int64 != st.ID {
		t.Fatalf("incumbent lost its assignment: %+v", keep)
	}
	// parked page must remain untouched (manual assign never overwrites, D65)
	got, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AssignedStudentID.Valid {
		t.Fatal("second page must not have been assigned")
	}
}

func TestAssignPage_PromotedPageBlocked(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

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
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: page.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	err = f.svc.AssignPage(f.ctx, page.ID, st.ID, prob.ID, 1)
	if err == nil {
		t.Fatal("want *ErrPagePromoted, got nil")
	}
	var promoted *ErrPagePromoted
	if !errors.As(err, &promoted) {
		t.Fatalf("want *ErrPagePromoted, got %T: %v", err, err)
	}
	if promoted.PageID != page.ID {
		t.Fatalf("PageID = %d, want %d", promoted.PageID, page.ID)
	}
}

func TestAssignPage_WithdrawnStudentRejected(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)
	if _, err := f.st.Q.SetStudentWithdrawn(f.ctx, db.SetStudentWithdrawnParams{ID: st.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	err := f.svc.AssignPage(f.ctx, page.ID, st.ID, prob.ID, 1)
	if err == nil {
		t.Fatal("want an error for a withdrawn student, got nil")
	}
	got, gerr := f.st.Q.GetScanPage(f.ctx, page.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.AssignedStudentID.Valid {
		t.Fatal("withdrawn student must not be assigned")
	}
}

func TestUnassignPage_ClearsCellAndForcePromote(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)
	if err := f.svc.AssignPage(f.ctx, page.ID, st.ID, prob.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Q.SetScanPageForcePromote(f.ctx, db.SetScanPageForcePromoteParams{ID: page.ID, ForcePromote: true}); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.UnassignPage(f.ctx, page.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AssignedStudentID.Valid || got.AssignedProblemID.Valid || got.AssignedBy.Valid {
		t.Fatalf("cell not cleared: %+v", got)
	}
	if got.ForcePromote {
		t.Fatal("force_promote must be cleared on unassign")
	}
}

func TestDiscardUndiscard_RoundTrip(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})

	if err := f.svc.DiscardPage(f.ctx, page.ID, "blank page", 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DiscardedAt.Valid || got.DiscardReason.String != "blank page" {
		t.Fatalf("not discarded: %+v", got)
	}

	if err := f.svc.UndiscardPage(f.ctx, page.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, err = f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DiscardedAt.Valid || got.DiscardReason.Valid {
		t.Fatalf("still discarded: %+v", got)
	}
}

func TestResolveConflict_KeepDiscardsParked(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	if err := f.svc.ResolveConflict(f.ctx, second.ID, "keep", false, 1); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DiscardedAt.Valid {
		t.Fatalf("want parked page discarded on keep, got %+v", got)
	}
	if got.DiscardReason.String != "conflict: kept incumbent" {
		t.Fatalf("discard_reason = %q", got.DiscardReason.String)
	}
	// incumbent (first) is untouched
	keep, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !keep.AssignedStudentID.Valid {
		t.Fatal("incumbent lost its assignment")
	}
}

func TestResolveConflict_ReplaceUnpromotedIncumbent(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	if err := f.svc.ResolveConflict(f.ctx, second.ID, "replace", false, 1); err != nil {
		t.Fatal(err)
	}

	// Incumbent (first) returns to orphan: assignment cleared.
	incumbent, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if incumbent.AssignedStudentID.Valid || incumbent.AssignedProblemID.Valid {
		t.Fatalf("incumbent must be unassigned (back to orphan), got %+v", incumbent)
	}

	// Parked page (second) now holds the cell.
	got, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st.ID {
		t.Fatalf("parked page not assigned to student: %+v", got)
	}
	if !got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob.ID {
		t.Fatalf("parked page not assigned to problem: %+v", got)
	}
	if got.ParkedReason.Valid {
		t.Fatalf("parked state must be cleared once assigned: %+v", got)
	}
}

func TestResolveConflict_ReplacePromotedIncumbent_RetractsAndAssigns(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	prob := problemByNumber(f, t, 1)

	// Promote the incumbent manually (Task 8's PromotePage doesn't exist yet):
	// ingest its rendered image directly against the same problem, then stamp
	// the link on the page row, mirroring what PromotePage will eventually do.
	data, err := f.svc.readAll(f.ctx, first.ImageRef.String)
	if err != nil {
		t.Fatal(err)
	}
	res := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.jpg", Data: data, Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture promotion: %+v", res)
	}
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: first.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	if err := f.svc.ResolveConflict(f.ctx, second.ID, "replace", true, 1); err != nil {
		t.Fatal(err)
	}

	// The submission must be retracted.
	sub, err := f.st.Q.GetSubmission(f.ctx, res.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if !sub.RetractedAt.Valid {
		t.Fatal("promoted incumbent's submission must be retracted")
	}

	// The parked page now holds the cell and has force_promote set (force=true).
	got, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st.ID {
		t.Fatalf("parked page not assigned: %+v", got)
	}
	if !got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob.ID {
		t.Fatalf("parked page not assigned to problem: %+v", got)
	}
	if !got.ForcePromote {
		t.Fatal("force_promote must be set when force=true")
	}

	// The incumbent returns to orphan cleanly: assignment cleared AND the
	// submission link cleared (it points at a retracted submission otherwise).
	incumbent, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if incumbent.AssignedStudentID.Valid {
		t.Fatal("incumbent must be unassigned")
	}
	if incumbent.SubmissionID.Valid {
		t.Fatal("incumbent's submission link must be cleared so it returns to orphan cleanly")
	}
}

// TestResolveConflict_GradedIncumbentNeedsForce asserts that when the
// promoted incumbent has grading records, ResolveConflict's replace path
// surfaces ingest's fixed-vocabulary "requires force" guard as *ErrInvalidInput
// (so the HTTP layer maps it to 400, not a 500 catch-all) rather than letting
// it bubble as a plain error. With force=true the same call must succeed.
func TestResolveConflict_GradedIncumbentNeedsForce(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	// Promote the incumbent manually, same as
	// TestResolveConflict_ReplacePromotedIncumbent_RetractsAndAssigns.
	data, err := f.svc.readAll(f.ctx, first.ImageRef.String)
	if err != nil {
		t.Fatal(err)
	}
	res := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.jpg", Data: data, Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture promotion: %+v", res)
	}
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: first.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a grading record for the incumbent's (student, problem), same
	// fixture pattern as TestPromotePage_ForcePromoteFlagPassesForce.
	ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{
		AssessmentID: f.aid, StudentID: st.ID, ProblemID: prob.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	addGradingRecord(f, t, prob.ID, ans.ID)

	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	// force=false: the graded-incumbent guard must map to *ErrInvalidInput
	// (400), carrying ingest's fixed-vocabulary "requires force" message.
	err = f.svc.ResolveConflict(f.ctx, parked.ID, "replace", false, 1)
	if err == nil {
		t.Fatal("want error when incumbent has grading records and force=false")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want errors.Is(err, ErrInvalidInput), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "requires force") {
		t.Fatalf("want message to contain %q, got %q", "requires force", err.Error())
	}

	// force=true: must succeed.
	if err := f.svc.ResolveConflict(f.ctx, parked.ID, "replace", true, 1); err != nil {
		t.Fatalf("force=true must succeed, got %v", err)
	}
}

// TestIsRetractGuardFailure_OnlySentinelsMatch pins ResolveConflict's replace
// path's mapping decision at the unit level: ONLY ingest's two guard
// sentinels (ErrRetractionNeedsForce, ErrRetractionBlocked) are treated as
// operator-fault 400s. A generic/driver error wrapping neither sentinel
// (simulating "guard check failed: %w" wrapping a live pgx error, or any
// other RetractSubmission failure) must be reported as NOT a guard failure,
// so ResolveConflict lets it propagate unwrapped to the HTTP layer's static
// 500 fallback instead of falsely telling the operator this was their
// mistake to fix.
//
// A DB-integration reproduction of a non-guard RetractSubmission failure
// (e.g. a raw pgx error from the guard-check COUNT queries) isn't
// constructible against this schema without violating a FK constraint --
// scan_pages.submission_id and submissions.problem_id are both hard
// REFERENCES with no test seam to inject a failing query underneath
// *db.Queries -- so the classification predicate itself is unit-tested
// directly here, dependency-free, while the two POSITIVE sentinel cases are
// covered end-to-end by TestResolveConflict_GradedIncumbentNeedsForce
// (ErrRetractionNeedsForce) and TestResolveConflict_PublishedIncumbentBlocked
// (ErrRetractionBlocked) below.
func TestIsRetractGuardFailure_OnlySentinelsMatch(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"needs force", fmt.Errorf("wrap: %w", ingest.ErrRetractionNeedsForce), true},
		{"blocked (published)", fmt.Errorf("wrap: %w", ingest.ErrRetractionBlocked), true},
		{"generic driver-ish error", fmt.Errorf("guard check failed: %w", errors.New("connection reset")), false},
		{"already retracted (handled separately, not a guard failure)", ingest.ErrAlreadyRetracted, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetractGuardFailure(c.err); got != c.want {
				t.Errorf("isRetractGuardFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestResolveConflict_PublishedIncumbentBlocked covers the second guard
// sentinel end-to-end (ErrRetractionNeedsForce's sibling, ErrRetractionBlocked):
// a promoted incumbent whose answer is published can never be retracted, even
// with force -- ResolveConflict must map that to *ErrInvalidInput too.
func TestResolveConflict_PublishedIncumbentBlocked(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)

	data, err := f.svc.readAll(f.ctx, first.ImageRef.String)
	if err != nil {
		t.Fatal(err)
	}
	res := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.jpg", Data: data, Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture promotion: %+v", res)
	}
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: first.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	ans, err := f.st.Q.GetAnswerByKey(f.ctx, db.GetAnswerByKeyParams{
		AssessmentID: f.aid, StudentID: st.ID, ProblemID: prob.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExecPage(f, t, ans.ID, "UPDATE answers SET published_at = now() WHERE id = $1")

	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	// Published blocks even with force=true (D1: published can never be replaced).
	err = f.svc.ResolveConflict(f.ctx, parked.ID, "replace", true, 1)
	if err == nil {
		t.Fatal("want error when incumbent's answer is published")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want errors.Is(err, ErrInvalidInput), got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "published") {
		t.Fatalf("want message to contain %q, got %q", "published", err.Error())
	}
}

// TestResolveConflict_SubmissionBackedConflict_ReplaceIsInvalidInput covers
// the "conflict with a live submission, no backing page" case (D65): the
// parked page's parked_against is NULL because the occupying cell is covered
// by a direct Submissions-tab upload, not another scan page. Replace has
// nothing to unassign/retract in that shape, so it must map to
// *ErrInvalidInput (400, operator-fault guidance to use the Submissions tab)
// instead of a bare error that the HTTP layer would surface as a 500.
func TestResolveConflict_SubmissionBackedConflict_ReplaceIsInvalidInput(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	// A live whole-assessment submission (Submissions-tab style) covers every
	// problem for the student, so any agreement page for them parks as
	// conflict with parked_against NULL (mirrors
	// TestIdentifyPage_OccupiedBySubmission_ParksConflict).
	res := f.svc.Ingest.IngestFile(f.ctx, f.aid, "B11902001.pdf", []byte("%PDF-1 direct"), 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture submission: %+v", res)
	}
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String != "conflict" || parked.ParkedAgainst.Valid {
		t.Fatalf("fixture setup: want conflict park with parked_against NULL, got %+v", parked)
	}

	err = f.svc.ResolveConflict(f.ctx, parked.ID, "replace", false, 1)
	if err == nil {
		t.Fatal("want error when the parked page has no incumbent page to replace")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want errors.Is(err, ErrInvalidInput), got %T: %v", err, err)
	}
}

// TestResolveConflict_ReplaceCrashWindowIsRecoverable simulates a crash
// between ResolveConflict's replace-path steps: the submission is retracted
// (step 1) but the follow-up tx that clears the incumbent's assignment and
// submission link, and assigns the parked page, never runs. The incumbent
// scan_page is left with submission_id pointing at a RETRACTED submission —
// a stale link that must not read as "live promoted", or the page (and the
// parked page behind it) would be stuck forever with no API-level recovery.
func TestResolveConflict_ReplaceCrashWindowIsRecoverable(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	prob := problemByNumber(f, t, 1)

	// Promote the incumbent manually (mirrors PromotePage, not built yet).
	data, err := f.svc.readAll(f.ctx, first.ImageRef.String)
	if err != nil {
		t.Fatal(err)
	}
	res := f.svc.Ingest.Ingest(f.ctx, f.aid, ingest.IngestInput{
		Filename: "B11902001.jpg", Data: data, Kind: "image", TargetProblemID: prob.ID,
	}, 1, false)
	if res.Status != "ingested" {
		t.Fatalf("fixture promotion: %+v", res)
	}
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: first.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}

	// Simulate the crash half-state: run ResolveConflict's first step
	// directly (retract the incumbent's submission) and stop there — the
	// incumbent's scan_page.submission_id is now stale, pointing at a
	// retracted submission.
	if err := f.svc.Ingest.RetractSubmission(f.ctx, res.SubmissionID, 1, true); err != nil {
		t.Fatal(err)
	}

	// Re-running ResolveConflict must COMPLETE, not fail with ErrPagePromoted.
	// ErrAlreadyRetracted internally (from the redundant retract attempt) is
	// tolerated — the important thing is forward progress.
	err = f.svc.ResolveConflict(f.ctx, second.ID, "replace", true, 1)
	if err != nil {
		t.Fatalf("resolve conflict must be re-runnable after a crash between retract and follow-up tx, got: %v", err)
	}

	// The incumbent must be fully orphaned: assignment cleared, stale
	// submission link cleared.
	incumbent, err := f.st.Q.GetScanPage(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if incumbent.AssignedStudentID.Valid {
		t.Fatal("incumbent must be unassigned")
	}
	if incumbent.SubmissionID.Valid {
		t.Fatal("incumbent's stale submission link must be cleared")
	}

	// The parked page now holds the cell.
	got, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := student(f, t, "B11902001")
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st.ID {
		t.Fatalf("parked page not assigned to the cell: %+v", got)
	}
	if !got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob.ID {
		t.Fatalf("parked page not assigned to problem: %+v", got)
	}
}

// TestUnassignPage_StaleSubmissionLinkSelfHeals guards the general case (not
// just ResolveConflict's replace path): any page carrying a stale
// submission_id — pointing at a retracted submission — must not be treated
// as live-promoted by other mutations, and touching it must clear the link
// (self-repair on write) so the page doesn't stay wedged.
func TestUnassignPage_StaleSubmissionLinkSelfHeals(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	st := student(f, t, "B11902001")
	prob := problemByNumber(f, t, 1)
	if err := f.svc.AssignPage(f.ctx, page.ID, st.ID, prob.ID, 1); err != nil {
		t.Fatal(err)
	}

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
	if err := f.st.Q.SetScanPageSubmission(f.ctx, db.SetScanPageSubmissionParams{
		ID: page.ID, SubmissionID: pgtype.Int8{Int64: res.SubmissionID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Retract the submission directly, leaving the page's submission_id
	// stale (still Valid, but pointing at a retracted row).
	if err := f.svc.Ingest.RetractSubmission(f.ctx, res.SubmissionID, 1, true); err != nil {
		t.Fatal(err)
	}

	// UnassignPage must NOT return ErrPagePromoted for a stale link.
	if err := f.svc.UnassignPage(f.ctx, page.ID, 1); err != nil {
		t.Fatalf("want stale link tolerated, got: %v", err)
	}

	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AssignedStudentID.Valid || got.AssignedProblemID.Valid {
		t.Fatalf("cell not cleared: %+v", got)
	}
	if got.SubmissionID.Valid {
		t.Fatal("stale submission link must be cleared by self-repair on write")
	}
}

// parkedPair drives identify twice for the same (B11902001, Q1) cell: the
// first page auto-assigns, the second parks against it. Returns (incumbent,
// parked) page ids.
func parkedPair(f fx, t *testing.T) (int64, int64) {
	t.Helper()
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	first := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	parked, err := f.st.Q.GetScanPage(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.ParkedReason.String == "" {
		t.Fatalf("fixture setup: second page did not park: %+v", parked)
	}
	return first.ID, second.ID
}

// TestResolveConflict_ReplaceAfterIncumbentReassigned_TargetsContestedCell
// pins the replace-targets-wrong-cell corruption: the incumbent was reassigned
// to a different (correct) cell after the park. Replace must put the parked
// page into the CONTESTED cell (where the fight happened, captured at park
// time) and must NOT evict the incumbent from its new cell.
func TestResolveConflict_ReplaceAfterIncumbentReassigned_TargetsContestedCell(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	firstID, secondID := parkedPair(f, t)
	st1 := student(f, t, "B11902001")
	prob1 := problemByNumber(f, t, 1)
	prob2 := problemByNumber(f, t, 2)

	// The incumbent moves on: a TA reassigns it to its correct cell.
	if err := f.svc.AssignPage(f.ctx, firstID, st1.ID, prob2.ID, 1); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.ResolveConflict(f.ctx, secondID, "replace", false, 1); err != nil {
		t.Fatalf("replace into the now-free contested cell must succeed, got %v", err)
	}

	// Incumbent keeps its NEW cell — replace never touches it once it moved.
	inc, err := f.st.Q.GetScanPage(f.ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if !inc.AssignedStudentID.Valid || inc.AssignedStudentID.Int64 != st1.ID ||
		!inc.AssignedProblemID.Valid || inc.AssignedProblemID.Int64 != prob2.ID {
		t.Fatalf("incumbent must keep its new cell, got %+v", inc)
	}

	// Parked page lands in the contested cell, not the incumbent's new one.
	got, err := f.st.Q.GetScanPage(f.ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st1.ID ||
		!got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob1.ID {
		t.Fatalf("parked page must take the contested cell (student %d, problem %d), got %+v", st1.ID, prob1.ID, got)
	}
	if got.ParkedReason.Valid {
		t.Fatalf("parked state must be cleared once assigned: %+v", got)
	}
}

// TestResolveConflict_ReplaceAfterIncumbentUnassigned_TakesContestedCell pins
// the NULL-assignment corruption: the incumbent was unassigned after the park.
// Replace must assign the parked page into the (now free) contested cell —
// never write a NULL-cell assignment.
func TestResolveConflict_ReplaceAfterIncumbentUnassigned_TakesContestedCell(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	firstID, secondID := parkedPair(f, t)
	st1 := student(f, t, "B11902001")
	prob1 := problemByNumber(f, t, 1)

	if err := f.svc.UnassignPage(f.ctx, firstID, 1); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.ResolveConflict(f.ctx, secondID, "replace", false, 1); err != nil {
		t.Fatalf("replace into the now-free contested cell must succeed, got %v", err)
	}

	got, err := f.st.Q.GetScanPage(f.ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != st1.ID ||
		!got.AssignedProblemID.Valid || got.AssignedProblemID.Int64 != prob1.ID {
		t.Fatalf("parked page must take the contested cell, got %+v", got)
	}
	// Incumbent stays a plain orphan.
	inc, err := f.st.Q.GetScanPage(f.ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.AssignedStudentID.Valid || inc.AssignedProblemID.Valid {
		t.Fatalf("unassigned incumbent must stay untouched, got %+v", inc)
	}
}

// TestResolveConflict_ReplaceContestedCellTakenByThirdParty_Stale: the
// incumbent moved away AND a third page took the contested cell since. The
// recorded conflict is stale — replace must refuse (409 *ErrConflictStale) and
// change nothing; the TA re-adjudicates against the new occupant.
func TestResolveConflict_ReplaceContestedCellTakenByThirdParty_Stale(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	firstID, secondID := parkedPair(f, t)
	st1 := student(f, t, "B11902001")
	prob1 := problemByNumber(f, t, 1)
	prob2 := problemByNumber(f, t, 2)

	if err := f.svc.AssignPage(f.ctx, firstID, st1.ID, prob2.ID, 1); err != nil {
		t.Fatal(err)
	}
	third := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	if err := f.svc.AssignPage(f.ctx, third.ID, st1.ID, prob1.ID, 1); err != nil {
		t.Fatal(err)
	}

	err := f.svc.ResolveConflict(f.ctx, secondID, "replace", false, 1)
	var stale *ErrConflictStale
	if !errors.As(err, &stale) {
		t.Fatalf("want *ErrConflictStale, got %T: %v", err, err)
	}

	// Nothing moved.
	inc, _ := f.st.Q.GetScanPage(f.ctx, firstID)
	if !inc.AssignedProblemID.Valid || inc.AssignedProblemID.Int64 != prob2.ID {
		t.Fatalf("incumbent must keep its new cell, got %+v", inc)
	}
	occ, _ := f.st.Q.GetScanPage(f.ctx, third.ID)
	if !occ.AssignedProblemID.Valid || occ.AssignedProblemID.Int64 != prob1.ID {
		t.Fatalf("third party must keep the contested cell, got %+v", occ)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, secondID)
	if got.AssignedStudentID.Valid || !got.ParkedReason.Valid {
		t.Fatalf("parked page must stay parked, got %+v", got)
	}
}

// TestResolveConflict_LegacyParkNoCell_IncumbentGone_Stale covers park rows
// that predate the park-cell columns (0031): with no recorded contested cell
// and an incumbent that no longer occupies any cell, the conflict is
// unrecoverable — replace must refuse (409) instead of writing the historical
// NULL-cell assignment.
func TestResolveConflict_LegacyParkNoCell_IncumbentGone_Stale(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	firstID, secondID := parkedPair(f, t)

	// Simulate a pre-0031 park row: no contested cell recorded.
	mustExecPage(f, t, secondID, "UPDATE scan_pages SET park_student_id = NULL, park_problem_id = NULL WHERE id = $1")
	if err := f.svc.UnassignPage(f.ctx, firstID, 1); err != nil {
		t.Fatal(err)
	}

	err := f.svc.ResolveConflict(f.ctx, secondID, "replace", false, 1)
	var stale *ErrConflictStale
	if !errors.As(err, &stale) {
		t.Fatalf("want *ErrConflictStale, got %T: %v", err, err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, secondID)
	if got.AssignedStudentID.Valid || got.AssignedProblemID.Valid || got.AssignedAt.Valid {
		t.Fatalf("replace must never write a NULL/loose assignment, got %+v", got)
	}
	if !got.ParkedReason.Valid {
		t.Fatalf("parked page must stay parked for re-adjudication, got %+v", got)
	}
}

func TestRetryPage(t *testing.T) {
	f := setup(t)
	addRegions(f, t)

	t.Run("all crops exist re-enqueues identify", func(t *testing.T) {
		page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
		if err := f.svc.setPageError(f.ctx, page.ID, "OCR provider unavailable"); err != nil {
			t.Fatal(err)
		}
		*f.identifies = (*f.identifies)[:0]
		*f.renders = (*f.renders)[:0]

		if err := f.svc.RetryPage(f.ctx, page.ID, 1); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Error.Valid {
			t.Fatalf("error not cleared: %+v", got)
		}
		found := false
		for _, id := range *f.identifies {
			if id == page.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("want page re-enqueued for identify, got identifies=%v renders=%v", *f.identifies, *f.renders)
		}
		if len(*f.renders) != 0 {
			t.Fatalf("must not re-enqueue render when crops already exist, got %+v", *f.renders)
		}
	})

	t.Run("missing crops re-enqueues render", func(t *testing.T) {
		src := looseSource(f, t, "run2.pdf", "%PDF-1 no render yet", NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
		if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
			t.Fatal(err)
		}
		pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
		if err != nil || len(pages) != 1 {
			t.Fatalf("pages: %d, %v", len(pages), err)
		}
		pageID := pages[0].ID
		if err := f.svc.setPageError(f.ctx, pageID, "page render failed"); err != nil {
			t.Fatal(err)
		}
		*f.identifies = (*f.identifies)[:0]
		*f.renders = (*f.renders)[:0]

		if err := f.svc.RetryPage(f.ctx, pageID, 1); err != nil {
			t.Fatal(err)
		}
		got, err := f.st.Q.GetScanPage(f.ctx, pageID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Error.Valid {
			t.Fatalf("error not cleared: %+v", got)
		}
		if len(*f.identifies) != 0 {
			t.Fatalf("must not re-enqueue identify when crops are missing, got %v", *f.identifies)
		}
		found := false
		for _, chunk := range *f.renders {
			for _, id := range chunk.PageIDs {
				if id == pageID {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("want a render chunk covering this page, got %+v", *f.renders)
		}
	})
}

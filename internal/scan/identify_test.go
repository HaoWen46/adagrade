package scan

import (
	"context"
	"fmt"
	"testing"

	"github.com/HaoWen46/adagrade/internal/imaging"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/ocr"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// fakeReader is a scripted ocr.Reader for the local-OCR rung: it returns the
// same canned lines for every crop it's asked to read (student_id, name,
// problem, in that order), regardless of which crop it's given — the tests
// that use it only care about what identifyLocal does with the lines, not
// about per-crop routing.
type fakeReader struct {
	lines []ocr.Line
	err   error
}

func (f fakeReader) ReadLines(_ context.Context, _ imaging.IDCrop) ([]ocr.Line, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.lines, nil
}

// identityJSON builds the provider's scripted response.
func identityJSON(id, name, problem string, idLeg, nameLeg, probLeg bool) string {
	return fmt.Sprintf(`{"student_id":%q,"name":%q,"problem":%q,"student_id_legible":%v,"name_legible":%v,"problem_legible":%v}`,
		id, name, problem, idLeg, nameLeg, probLeg)
}

// renderedPage drives upload->split->render for one single-page PDF source and
// returns the page row, with the scripted provider wired as "p".
func renderedPage(f fx, t *testing.T, prov llm.Provider) db.ScanPage {
	t.Helper()
	f.svc.Providers = llm.StaticSource{"p": prov}
	f.svc.Renderer = render.NewFake(1)
	src := looseSource(f, t, "run.pdf", "%PDF-1 "+t.Name(), NewBatch{OCREnabled: true, OCRProvider: "p", OCRModel: "m"})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[len(*f.renders)-1]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages: %d, %v", len(pages), err)
	}
	return pages[0]
}

func student(f fx, t *testing.T, ext string) db.Student {
	t.Helper()
	st, err := f.st.Q.GetStudentByExternalID(f.ctx, ext)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func problemByNumber(f fx, t *testing.T, n int32) db.Problem {
	t.Helper()
	probs, err := f.st.Q.ListProblems(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		if p.Number == n {
			return p
		}
	}
	t.Fatalf("no problem %d", n)
	return db.Problem{}
}

func TestIdentifyPage_AgreementAutoAssigns(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q2", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if !got.AssignedStudentID.Valid || got.AssignedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("not auto-assigned: %+v", got)
	}
	if got.AssignedProblemID.Int64 != problemByNumber(f, t, 2).ID {
		t.Fatalf("wrong problem: %+v", got)
	}
	if got.AssignedBy.Valid {
		t.Fatal("auto-assign must leave assigned_by NULL")
	}
}

func TestIdentifyPage_CleanIDIllegibleName_Orphans(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "", "Q1", true, false, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("clean ID with illegible name must NOT auto-assign (user rule)")
	}
	if got.ProposalSource.String != "ocr_id" || got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("want ocr_id pre-fill, got %+v", got)
	}
	if !got.ProposedProblemID.Valid {
		t.Fatal("valid problem read should still be pre-filled")
	}
	if !got.IdentifiedAt.Valid {
		t.Fatal("identified_at must be stamped (orphan, not processing)")
	}
}

func TestIdentifyPage_DisagreementOrphansNoPrefill(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "李大華", "Q1", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid || got.ProposedStudentID.Valid {
		t.Fatalf("disagreement must not assign or pre-fill a student: %+v", got)
	}
	if got.ProposalSource.String != "ocr_disagree" {
		t.Fatalf("want ocr_disagree flag, got %q", got.ProposalSource.String)
	}
}

// TestIdentifyPage_NearMissID_ProposesOnly: an OCR'd ID with no exact roster
// hit but exactly one roster ID within edit distance 1 must surface as an
// orphan with an "ocr_id_near" proposal (never an assignment), and the
// proposal must flow through the staff-facing ScanPageRows query with the
// roster-resolved external ID + name — the same row the scan-pages list
// handler serializes.
func TestIdentifyPage_NearMissID_ProposesOnly(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	// "A11902001" is distance 1 from B11902001 only (distance 2 from
	// B11902002); the name box is illegible.
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("A11902001", "", "Q1", true, false, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("a near-miss ID must NEVER auto-assign (wrong-student firewall)")
	}
	if got.ProposalSource.String != "ocr_id_near" || got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("want ocr_id_near pre-fill for B11902001, got %+v", got)
	}
	if !got.ProposedProblemID.Valid {
		t.Fatal("valid problem read should still be pre-filled")
	}
	if !got.IdentifiedAt.Valid {
		t.Fatal("identified_at must be stamped (orphan, not processing)")
	}

	// API plumbing: ScanPageRows resolves the proposal against the roster.
	rows, err := f.st.Q.ScanPageRows(f.ctx, f.aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID != page.ID {
			continue
		}
		if r.ProposalSource.String != "ocr_id_near" {
			t.Fatalf("ScanPageRows proposal_source = %q, want ocr_id_near", r.ProposalSource.String)
		}
		if r.ProposedExternalID.String != "B11902001" || r.ProposedNameRoster.String != "王小明" {
			t.Fatalf("ScanPageRows proposed identity not roster-resolved: %+v", r)
		}
		if r.OcrStudentID.String != "A11902001" {
			t.Fatalf("raw OCR'd ID must survive for the compare-at-a-glance UI, got %q", r.OcrStudentID.String)
		}
		return
	}
	t.Fatalf("page %d not in ScanPageRows", page.ID)
}

// TestIdentifyPage_NearMissIDWithMatchingName_NeverAutoAssigns: the name box
// agreeing with the near-miss student upgrades confidence in the PROPOSAL only
// — auto-assign still requires the exact-ID rung, because one OCR digit error
// is exactly how a page lands on the wrong real student.
func TestIdentifyPage_NearMissIDWithMatchingName_NeverAutoAssigns(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("A11902001", "王小明", "Q1", true, true, true)},
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("near-miss ID + matching name must still NOT auto-assign")
	}
	if got.ProposalSource.String != "ocr_id_near" || got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("want ocr_id_near pre-fill for B11902001, got %+v", got)
	}
	if !got.IdentifiedAt.Valid {
		t.Fatal("identified_at must be stamped (orphan, not processing)")
	}
}

func TestIdentifyPage_InvalidProblemOrphans(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q9", true, true, true)}, // only 3 problems exist
	}}
	page := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("out-of-range problem must not auto-assign")
	}
	if got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatal("student agreement should still pre-fill")
	}
}

func TestIdentifyPage_OccupiedCell_IdenticalParksDuplicate(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	script := []fake.JSONStep{{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)}}
	prov := &fake.ScriptedProvider{NameStr: "p", Steps: script}
	first := renderedPage(f, t, prov)
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	// Re-upload: same source content in a NEW batch -> same rendered image sha.
	second := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p", Steps: script})
	// force identical image sha (fake renderer derives pixels from page index,
	// so two page-0 renders are byte-identical already; assert to be safe)
	p1, _ := f.st.Q.GetScanPage(f.ctx, first.ID)
	p2, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if p1.ImageSha256.String != p2.ImageSha256.String {
		t.Skip("fake renderer no longer deterministic; adjust fixture")
	}
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("occupied cell must never be overwritten")
	}
	if got.ParkedReason.String != "duplicate" || got.ParkedAgainst.Int64 != first.ID {
		t.Fatalf("want duplicate park against first page, got %+v", got)
	}
	// incumbent untouched
	keep, _ := f.st.Q.GetScanPage(f.ctx, first.ID)
	if !keep.AssignedStudentID.Valid {
		t.Fatal("incumbent lost its assignment")
	}
}

func TestIdentifyPage_OccupiedCell_DifferentParksConflict(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	prov1 := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)},
	}}
	first := renderedPage(f, t, prov1)
	if err := f.svc.IdentifyPage(f.ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	// different pixels: use the 2-page fake so page index differs? Simpler:
	// overwrite the second page's image_sha256 directly to simulate different
	// content OCRing to the same cell.
	prov2 := &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "王小明", "Q1", true, true, true)},
	}}
	second := renderedPage(f, t, prov2)
	mustExecPage(f, t, second.ID, "UPDATE scan_pages SET image_sha256 = 'different' WHERE id = $1")
	if err := f.svc.IdentifyPage(f.ctx, second.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, second.ID)
	if got.ParkedReason.String != "conflict" {
		t.Fatalf("want conflict park, got %+v", got)
	}
}

func mustExecPage(f fx, t *testing.T, id int64, sql string) {
	t.Helper()
	if _, err := f.st.Pool.Exec(f.ctx, sql, id); err != nil {
		t.Fatal(err)
	}
}

func TestIdentifyPage_OccupiedBySubmission_ParksConflict(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	// A live whole-assessment submission (Submissions-tab style) covers every
	// problem for the student, so any agreement page for them parks as conflict
	// with no incumbent page to point at.
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
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.AssignedStudentID.Valid {
		t.Fatal("cell covered by a live submission must never be auto-assigned")
	}
	if got.ParkedReason.String != "conflict" {
		t.Fatalf("want conflict park, got %+v", got)
	}
	if got.ParkedAgainst.Valid {
		t.Fatal("submission incumbent has no page: parked_against must be NULL")
	}
}

func TestIdentifyPage_ProviderUnavailable_Terminal(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	page := renderedPage(f, t, &fake.ScriptedProvider{NameStr: "p"})
	// point the batch at an unknown provider name
	f.svc.Providers = llm.StaticSource{}
	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal("provider-unavailable is terminal, not retryable")
	}
	got, _ := f.st.Q.GetScanPage(f.ctx, page.ID)
	if got.Error.String != "OCR provider unavailable" {
		t.Fatalf("error = %q", got.Error.String)
	}
}

// TestIdentifyPage_OCRDisabledWithLocal_PartialReadsBecomeOrphan covers the
// rung ABOVE commit eca4252 (which fixed the no-local-reader-at-all case):
// here s.Local IS installed, but its read is only partial — a confident ID
// line with no confident name line, so identifyLocal correctly declines to
// auto-assign (done=false) and falls through. With the batch's cloud OCR
// off, IdentifyPage used to just `return nil` at that point: identified_at
// was never stamped, so the page stayed "processing" forever, invisible to
// the OrphanQueue, with no surface for manual assignment. The D24 ladder's
// last rung must instead stamp the page identified WITH the local rung's
// partial reads (via placeAuto, exactly as identifyLocal's own success path
// does), so it surfaces as an orphan with whatever pre-fill proposal the
// partial reads support.
func TestIdentifyPage_OCRDisabledWithLocal_PartialReadsBecomeOrphan(t *testing.T) {
	f := setup(t)
	addRegions(f, t)
	local := fakeReader{lines: []ocr.Line{
		{Text: "B11902001", Confidence: 0.95}, // confident ID line, no name
	}}
	f.svc.Local = local
	f.svc.Renderer = render.NewFake(1)
	src := looseSource(f, t, "run.pdf", "%PDF-1 "+t.Name(), NewBatch{OCREnabled: false})
	if err := f.svc.SplitSource(f.ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	chunk := (*f.renders)[len(*f.renders)-1]
	if err := f.svc.RenderPages(f.ctx, chunk.SourceID, chunk.PageIDs); err != nil {
		t.Fatal(err)
	}
	pages, err := f.st.Q.ListScanPagesForSource(f.ctx, src.ID)
	if err != nil || len(pages) != 1 {
		t.Fatalf("pages: %d, %v", len(pages), err)
	}
	page := pages[0]

	if err := f.svc.IdentifyPage(f.ctx, page.ID, false); err != nil {
		t.Fatal(err)
	}

	got, err := f.st.Q.GetScanPage(f.ctx, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IdentifiedAt.Valid {
		t.Fatal("identified_at must be stamped so the page surfaces as an orphan, not stuck processing")
	}
	if got.AssignedStudentID.Valid {
		t.Fatal("a partial local read (ID only, no name) must not auto-assign")
	}
	if got.ParkedReason.Valid {
		t.Fatal("a partial local read has nothing to conflict with; it must orphan, not park")
	}
	if got.ProposalSource.String != "ocr_id" || got.ProposedStudentID.Int64 != student(f, t, "B11902001").ID {
		t.Fatalf("want ocr_id pre-fill from the local ID read, got %+v", got)
	}
	if got.OcrEngine.String != engineLocal {
		t.Fatalf("want ocr_engine = %q (local rung wrote these reads), got %q", engineLocal, got.OcrEngine.String)
	}
}

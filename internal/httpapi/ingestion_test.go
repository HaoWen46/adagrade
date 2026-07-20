package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// pngBytes builds a tiny valid PNG for image-submission upload tests.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHandleSubmissionSource_StreamsByKind(t *testing.T) {
	e := harnessEnv(t)
	ts, st := e.ts, e.st
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Quiz"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	postExpect(t, lecturer, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "Q1", "max_points": "10"}, http.StatusCreated)
	seedStudent(t, st, "b01", "Alice", "a@x.edu")
	seedStudent(t, st, "b02", "Bob", "b@x.edu")

	// PDF upload → application/pdf.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "b01.pdf")
	_, _ = fw.Write([]byte("%PDF-fake"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/submissions", ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := lecturer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	upload := struct {
		Results []struct {
			Status   string `json:"status"`
			UploadID int64  `json:"upload_id"`
		} `json:"results"`
	}{}
	_ = json.NewDecoder(resp.Body).Decode(&upload)
	resp.Body.Close()
	if len(upload.Results) != 1 || upload.Results[0].Status != "queued" {
		t.Fatalf("pdf upload: %+v", upload)
	}
	// The upload only stages + enqueues now (D27, F1); drive the worker directly.
	driveDirectUploads(t, e, aid)
	row, err := e.st.Q.GetDirectUpload(t.Context(), upload.Results[0].UploadID)
	if err != nil {
		t.Fatalf("GetDirectUpload: %v", err)
	}
	if !row.Status.Valid || row.Status.String != "ingested" {
		t.Fatalf("direct upload row status: %+v", row)
	}
	sid := row.SubmissionID.Int64

	srcResp, err := lecturer.Get(fmt.Sprintf("%s/api/submissions/%d/pdf", ts.URL, sid))
	if err != nil {
		t.Fatal(err)
	}
	if ct := srcResp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("pdf submission content-type: got %q want application/pdf", ct)
	}
	srcResp.Body.Close()

	// Image submission (driven directly through the ingest service, since the
	// multipart upload endpoint only accepts PDFs today) → image/png.
	imgRes := e.ing.Ingest(t.Context(), aid, ingest.IngestInput{
		Filename: "b02.png", Data: pngBytes(t, 200, 100), Kind: "image",
	}, 0, false)
	if imgRes.Status != "ingested" {
		t.Fatalf("image ingest: %+v", imgRes)
	}
	imgResp, err := lecturer.Get(fmt.Sprintf("%s/api/submissions/%d/pdf", ts.URL, imgRes.SubmissionID))
	if err != nil {
		t.Fatal(err)
	}
	defer imgResp.Body.Close()
	if ct := imgResp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("image submission content-type: got %q want image/png", ct)
	}
}

// TestUploadSubmissions_AsyncQueuedThenIngestedAndQuarantined drives the full D27,
// F1 async contract for handleUploadSubmissions + the new uploads reconciliation
// endpoint: the upload request only stages + enqueues (202, status "queued" for
// every accepted file with no sync ingest result yet); driving the ingest.direct
// worker body per staged row then lands one submission (roster match) and one
// quarantine entry (unknown filename); GET .../uploads reflects both terminal
// states; and the pre-existing ingest-report endpoint is unaffected by the new
// staging table.
func TestUploadSubmissions_AsyncQueuedThenIngestedAndQuarantined(t *testing.T) {
	env := harnessEnv(t)
	ts, st := env.ts, env.st
	lecturer := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lecturer, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Bulk"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	postExpect(t, lecturer, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "Q1", "max_points": "10"}, http.StatusCreated)
	seedStudent(t, st, "b01", "Alice", "a@x.edu")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", "b01.pdf")
	_, _ = fw.Write([]byte("%PDF-b01"))
	fw2, _ := mw.CreateFormFile("files", "not-on-roster.pdf")
	_, _ = fw2.Write([]byte("%PDF-mystery"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/submissions", ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := lecturer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upload: got %d want 202", resp.StatusCode)
	}
	var upload struct {
		Results []struct {
			Filename string `json:"filename"`
			UploadID int64  `json:"upload_id"`
			Status   string `json:"status"`
			Reason   string `json:"reason"`
		} `json:"results"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&upload)
	resp.Body.Close()
	if len(upload.Results) != 2 {
		t.Fatalf("results: %+v", upload.Results)
	}
	for _, r := range upload.Results {
		if r.Status != "queued" || r.UploadID == 0 {
			t.Fatalf("both files should be queued with no sync result yet: %+v", r)
		}
	}

	// Before the worker runs, the uploads endpoint shows both pending.
	pending := getJSON[map[string]any](t, lecturer, fmt.Sprintf("%s/api/assessments/%d/uploads", ts.URL, aid), http.StatusOK)
	pendingRows := pending["uploads"].([]any)
	if len(pendingRows) != 2 {
		t.Fatalf("pending uploads: %v", pendingRows)
	}
	for _, row := range pendingRows {
		rm := row.(map[string]any)
		if rm["status"] != "pending" {
			t.Errorf("row should be pending before the worker runs: %v", rm)
		}
	}

	driveDirectUploads(t, env, aid)

	after := getJSON[map[string]any](t, lecturer, fmt.Sprintf("%s/api/assessments/%d/uploads", ts.URL, aid), http.StatusOK)
	afterRows := after["uploads"].([]any)
	if len(afterRows) != 2 {
		t.Fatalf("uploads after drive: %v", afterRows)
	}
	var sawIngested, sawQuarantined bool
	for _, row := range afterRows {
		rm := row.(map[string]any)
		switch rm["status"] {
		case "ingested":
			sawIngested = true
			if rm["submission_id"] == nil {
				t.Errorf("ingested row should carry a submission_id: %v", rm)
			}
		case "quarantined":
			sawQuarantined = true
		}
	}
	if !sawIngested || !sawQuarantined {
		t.Fatalf("uploads should show one ingested + one quarantined: %v", afterRows)
	}

	// The pre-existing ingest-report reconciliation view is unaffected by staging.
	report := getJSON[map[string]any](t, lecturer, fmt.Sprintf("%s/api/assessments/%d/ingest/report", ts.URL, aid), http.StatusOK)
	students := report["students"].([]any)
	found := false
	for _, s := range students {
		sm := s.(map[string]any)
		if sm["student_id"] == "b01" && sm["submission_id"] != nil {
			found = true
		}
	}
	if !found {
		t.Errorf("ingest report should show b01 with a submission: %v", students)
	}
	quarantine := report["quarantine"].([]any)
	if len(quarantine) != 1 {
		t.Errorf("ingest report should list 1 open quarantine entry: %v", quarantine)
	}
}

// TestAcceptPendingMasks_BulkAcceptsOnlyMaskedPending drives the bulk-accept
// endpoint: it must accept every page that is masked AND still pending, record
// the reviewer, and leave unmasked or flagged pages untouched; a repeat call
// with nothing left pending reports 0.
func TestAcceptPendingMasks_BulkAcceptsOnlyMaskedPending(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	ctx := context.Background()

	// Regions + apply plan both pages, but only drive the mask job for the first:
	// the second stays pending with no masked image (must NOT be bulk-accepted).
	put, _ := json.Marshal(map[string]any{"regions": []map[string]any{
		{"page_scope": "all", "x": 0.05, "y": 0.02, "w": 0.4, "h": 0.08, "color": "#4a4a4a", "padding": 0.01},
	}})
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/assessments/%d/mask-regions", env.ts.URL, aid), bytes.NewReader(put))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put regions: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/apply", env.ts.URL, aid), nil, http.StatusAccepted)

	pages, err := env.st.Q.ListPagesForAssessment(ctx, aid)
	if err != nil {
		t.Fatalf("ListPagesForAssessment: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages: got %d want 2", len(pages))
	}
	if err := env.ing.MaskPage(ctx, pages[0].ID, false); err != nil {
		t.Fatalf("MaskPage(%d): %v", pages[0].ID, err)
	}

	got := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/accept-pending", env.ts.URL, aid), nil, http.StatusOK)
	if got["accepted"].(float64) != 1 {
		t.Fatalf("accepted: got %v want 1", got["accepted"])
	}

	review := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	for _, pg := range review["pages"] {
		switch int64(pg["page_id"].(float64)) {
		case pages[0].ID:
			if pg["review_status"] != "accepted" {
				t.Errorf("masked pending page should be accepted: %v", pg)
			}
		default:
			if pg["review_status"] != "pending" {
				t.Errorf("unmasked page must stay pending: %v", pg)
			}
		}
	}

	// The bulk accept records who reviewed.
	after, err := env.st.Q.ListPagesForAssessment(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, pg := range after {
		if pg.ID == pages[0].ID && !pg.MaskReviewedBy.Valid {
			t.Errorf("bulk-accepted page should record the reviewer")
		}
	}

	// Mask the second page, then flag it: a repeat bulk accept must leave the
	// flagged page alone and report 0.
	if err := env.ing.MaskPage(ctx, pages[1].ID, false); err != nil {
		t.Fatalf("MaskPage(%d): %v", pages[1].ID, err)
	}
	flagResp := postJSON(t, c, fmt.Sprintf("%s/api/answer-pages/%d/mask-review", env.ts.URL, pages[1].ID), map[string]string{"status": "flagged"})
	flagResp.Body.Close()

	again := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/accept-pending", env.ts.URL, aid), nil, http.StatusOK)
	if again["accepted"].(float64) != 0 {
		t.Fatalf("second accept-pending: got %v want 0", again["accepted"])
	}
	reviewAfter := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	for _, pg := range reviewAfter["pages"] {
		if int64(pg["page_id"].(float64)) == pages[1].ID && pg["review_status"] != "flagged" {
			t.Errorf("flagged page must survive bulk accept: %v", pg)
		}
	}
}

// TestPutMaskRegions_EditAfterAcceptanceInvalidatesStaleMasks pins the
// stale-mask fix (2026-07-11): editing regions after review acceptance must
// knock the affected pages' acceptance back to pending, so the "masked +
// accepted" grading gates block until re-mask + re-accept — instead of runs
// silently sending the OLD (possibly identity-revealing) masked images to
// providers forever. A PUT that doesn't change any page's mask inputs is a
// review no-op.
func TestPutMaskRegions_EditAfterAcceptanceInvalidatesStaleMasks(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	ctx := context.Background()
	acceptAllMasks(t, env, c, aid) // 2 pages (b01 + b02), masked + accepted

	preview := getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if preview["mask_blockers"].(float64) != 0 {
		t.Fatalf("mask_blockers before edit = %v, want 0", preview["mask_blockers"])
	}

	// Re-saving the SAME region set (acceptAllMasks' values) changes no page's
	// mask inputs: acceptances survive.
	same := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/mask-regions", env.ts.URL, aid),
		map[string]any{"regions": []map[string]any{
			{"page_scope": "all", "x": 0.05, "y": 0.02, "w": 0.4, "h": 0.08, "color": "#4a4a4a", "padding": 0.01},
		}}, http.StatusOK)
	if same["stale"].(float64) != 0 {
		t.Fatalf("no-op PUT stale = %v, want 0", same["stale"])
	}

	// Moving the region (the TA fixes a rect that was leaking a student id)
	// invalidates every accepted page masked under the old set.
	moved := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/mask-regions", env.ts.URL, aid),
		map[string]any{"regions": []map[string]any{
			{"page_scope": "all", "x": 0.1, "y": 0.05, "w": 0.5, "h": 0.1, "color": "#4a4a4a", "padding": 0.01},
		}}, http.StatusOK)
	if moved["stale"].(float64) != 2 {
		t.Fatalf("region-change PUT stale = %v, want 2", moved["stale"])
	}
	review := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	for _, pg := range review["pages"] {
		if pg["review_status"] != "pending" {
			t.Errorf("page %v should be pending after region change, got %v", pg["page_id"], pg["review_status"])
		}
	}
	preview = getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if preview["mask_blockers"].(float64) != 2 {
		t.Errorf("mask_blockers after edit = %v, want 2", preview["mask_blockers"])
	}

	// The reviewer stamp is cleared with the acceptance.
	pages, err := env.st.Q.ListPagesForAssessment(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, pg := range pages {
		if pg.MaskReviewedBy.Valid || pg.MaskReviewedAt.Valid {
			t.Errorf("page %d should have its reviewer stamp cleared: %+v", pg.ID, pg)
		}
	}

	// The normal apply flow recovers: re-mask (fingerprints now match the new
	// regions) + re-accept re-opens the gate.
	postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/apply", env.ts.URL, aid), nil, http.StatusAccepted)
	for _, pg := range pages {
		if err := env.ing.MaskPage(ctx, pg.ID, false); err != nil {
			t.Fatalf("MaskPage(%d): %v", pg.ID, err)
		}
	}
	postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/accept-pending", env.ts.URL, aid), nil, http.StatusOK)
	preview = getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if preview["mask_blockers"].(float64) != 0 {
		t.Errorf("mask_blockers after re-mask + re-accept = %v, want 0", preview["mask_blockers"])
	}
}

// TestMaterializeAnswersRoute pins the materialize action's contract
// (roster-lifecycle plan 2026-07-10, task R3) through the route R1 registered
// (POST /api/assessments/{id}/materialize-answers, lecturer+): MaterializeAnswers
// runs inside a tx and the response reports {"created": n} from before/after
// counts, a second call creates 0, withdrawn students are excluded, a
// late-added student is backfilled, each call is audit-logged (counts only),
// and a TA gets 403.
func TestMaterializeAnswersRoute(t *testing.T) {
	e := harnessEnv(t)
	lect := loginAs(t, e.ts, e.st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, e.ts, e.st, "ta@ntu.edu.tw", "ta")

	a := postExpect(t, lect, e.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	for n := 1; n <= 2; n++ {
		postExpect(t, lect, fmt.Sprintf("%s/api/assessments/%d/problems", e.ts.URL, aid),
			map[string]any{"number": n, "title": fmt.Sprintf("Q%d", n), "max_points": "10"}, http.StatusCreated)
	}

	ctx := context.Background()
	seedStudent(t, e.st, "b01", "Alice", "a@x.edu")
	seedStudent(t, e.st, "b02", "Bob", "b@x.edu")
	gone, err := e.st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "b03", Name: "Gone", Email: "g@x.edu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Q.SetStudentWithdrawn(ctx, db.SetStudentWithdrawnParams{ID: gone.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("%s/api/assessments/%d/materialize-answers", e.ts.URL, aid)

	// Lecturer+ route: TA is refused.
	resp := postJSON(t, ta, url, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta materialize: got %d want 403", resp.StatusCode)
	}

	first := postExpect(t, lect, url, nil, http.StatusOK)
	if first["created"].(float64) != 4 {
		t.Fatalf("first materialize: %v, want created=4 (2 active students x 2 problems, withdrawn excluded)", first)
	}
	second := postExpect(t, lect, url, nil, http.StatusOK)
	if second["created"].(float64) != 0 {
		t.Fatalf("second materialize: %v, want created=0", second)
	}

	// Late add: a student joins after the first upload — the whole point of the action.
	seedStudent(t, e.st, "b04", "Late", "l@x.edu")
	third := postExpect(t, lect, url, nil, http.StatusOK)
	if third["created"].(float64) != 2 {
		t.Fatalf("late-add materialize: %v, want created=2", third)
	}
	total, err := e.st.Q.CountAnswersForAssessment(ctx, aid)
	if err != nil || total != 6 {
		t.Fatalf("answers after materialize: %d %v, want 6", total, err)
	}

	entries, err := e.st.ListAudit(ctx, store.ListAuditParams{TargetKind: "assessment", TargetID: strconv.FormatInt(aid, 10), Action: "answers.materialize"})
	if err != nil || len(entries) != 3 {
		t.Fatalf("audit rows: %d %v, want 3", len(entries), err)
	}

	resp = postJSON(t, lect, e.ts.URL+"/api/assessments/999999/materialize-answers", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown assessment: got %d want 404", resp.StatusCode)
	}
}

// TestAssignQuarantineRoute_NormalizedAmbiguousAndWithdrawn drives the existing
// POST /api/quarantine/{id}/assign route through the shared exact-then-normalized
// student lookup (roster-lifecycle plan 2026-07-10, task R3): a case-variant id
// resolves to the roster's canonical student, a normalization-colliding id 400s
// with "ambiguous student id", and an exact match on a withdrawn student keeps
// the existing rejection message.
func TestAssignQuarantineRoute_NormalizedAmbiguousAndWithdrawn(t *testing.T) {
	e := harnessEnv(t)
	lect := loginAs(t, e.ts, e.st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, lect, e.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Quiz"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	postExpect(t, lect, fmt.Sprintf("%s/api/assessments/%d/problems", e.ts.URL, aid),
		map[string]any{"number": 1, "title": "Q1", "max_points": "10"}, http.StatusCreated)

	ctx := context.Background()
	seedStudent(t, e.st, "B11902066", "Alice", "alice@x.edu")
	seedStudent(t, e.st, "B66", "Bob", "bob@x.edu")
	seedStudent(t, e.st, "b66", "Carol", "carol@x.edu")
	gone, err := e.st.Q.UpsertStudent(ctx, db.UpsertStudentParams{StudentID: "B41", Name: "Dave", Email: "dave@x.edu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Q.SetStudentWithdrawn(ctx, db.SetStudentWithdrawnParams{ID: gone.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}

	// Two quarantined files to resolve (distinct bytes -> distinct rows).
	for i, name := range []string{"mystery1.pdf", "mystery2.pdf"} {
		res := e.ing.IngestFile(ctx, aid, name, []byte(fmt.Sprintf("%%PDF-mystery-%d", i)), 0, false)
		if res.Status != "quarantined" {
			t.Fatalf("quarantine %s: %+v", name, res)
		}
	}
	open, err := e.st.Q.ListOpenQuarantine(ctx, aid)
	if err != nil || len(open) != 2 {
		t.Fatalf("open quarantine: %d %v", len(open), err)
	}

	// Ambiguous under normalization -> 400, entry stays open.
	resp := postJSON(t, lect, fmt.Sprintf("%s/api/quarantine/%d/assign", e.ts.URL, open[0].ID), map[string]any{"student_id": "b-66"})
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "ambiguous student id") {
		t.Fatalf("ambiguous assign: got %d — %s", resp.StatusCode, body)
	}

	// Withdrawn student via EXACT id -> the existing rejection, entry stays open.
	rej := postExpect(t, lect, fmt.Sprintf("%s/api/quarantine/%d/assign", e.ts.URL, open[0].ID), map[string]any{"student_id": "B41"}, http.StatusOK)
	if rej["status"] != "rejected" || rej["reason"] != "student is withdrawn; reinstate before uploading" {
		t.Fatalf("withdrawn assign: %+v", rej)
	}

	// Case-variant id resolves via the normalized fallback and ingests.
	okRes := postExpect(t, lect, fmt.Sprintf("%s/api/quarantine/%d/assign", e.ts.URL, open[1].ID), map[string]any{"student_id": "b11902066"}, http.StatusOK)
	if okRes["status"] != "ingested" || okRes["student_id"] != "B11902066" {
		t.Fatalf("normalized assign: %+v", okRes)
	}
	afterOpen, _ := e.st.Q.ListOpenQuarantine(ctx, aid)
	if len(afterOpen) != 1 || afterOpen[0].ID != open[0].ID {
		t.Fatalf("exactly the ambiguous/withdrawn entry should remain open: %+v", afterOpen)
	}
}

func TestDismissQuarantineRoute_ClosesUnreadableEntryAndAudits(t *testing.T) {
	e := harnessEnv(t)
	lect := loginAs(t, e.ts, e.st, "lect@ntu.edu.tw", "lecturer")
	a := postExpect(t, lect, e.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Quiz"}, http.StatusCreated)
	aid := int64(a["id"].(float64))

	q, err := e.st.Q.CreateQuarantine(context.Background(), db.CreateQuarantineParams{
		AssessmentID: aid, OriginalFilename: "broken.pdf",
		PdfRef: "assessments/quarantine/broken.pdf", PdfSha256: "broken", Reason: "invalid_pdf",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := postExpect(t, lect, fmt.Sprintf("%s/api/quarantine/%d/dismiss", e.ts.URL, q.ID), nil, http.StatusOK)
	if got["dismissed"] != true {
		t.Fatalf("dismiss response: %+v", got)
	}
	open, err := e.st.Q.ListOpenQuarantine(context.Background(), aid)
	if err != nil || len(open) != 0 {
		t.Fatalf("dismissed entry should leave the open queue: %+v %v", open, err)
	}
	audit, err := e.st.ListAudit(context.Background(), store.ListAuditParams{
		TargetKind: "quarantine", TargetID: strconv.FormatInt(q.ID, 10), Action: "quarantine.dismiss",
	})
	if err != nil || len(audit) != 1 {
		t.Fatalf("dismiss audit rows: %d %v", len(audit), err)
	}
}

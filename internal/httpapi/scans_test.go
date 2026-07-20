package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/scan"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// scanSetup: lecturer login, an exam assessment, THREE problems (1-3), a
// two-student roster (synthetic 9-char external ids matching the design
// spec's B11902xxx shape), and the three typed id-regions already PUT so
// CreateBatch's regionsComplete gate is satisfied by default. Returns the env,
// logged-in lecturer client, assessment id, and the three problem ids keyed by
// number.
func scanSetup(t *testing.T) (*testEnv, *http.Client, int64, map[int]int64) {
	t.Helper()
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, c, env.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Scan Exam"}, http.StatusCreated)
	aid := int64(a["id"].(float64))

	problems := make(map[int]int64, 3)
	for n := 1; n <= 3; n++ {
		p := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", env.ts.URL, aid),
			map[string]any{"number": n, "title": fmt.Sprintf("Q%d", n), "max_points": "10"}, http.StatusCreated)
		problems[n] = int64(p["id"].(float64))
	}

	seedStudent(t, env.st, "B11902001", "Student One", "b11902001@ntu.edu.tw")
	seedStudent(t, env.st, "B11902002", "Student Two", "b11902002@ntu.edu.tw")

	putIDRegions(t, c, env.ts, aid, []map[string]any{
		{"kind": "student_id", "x": 0.05, "y": 0.02, "w": 0.3, "h": 0.06, "color": "#4a4a4a", "padding": 0.01},
		{"kind": "name", "x": 0.4, "y": 0.02, "w": 0.3, "h": 0.06, "color": "#4a4a4a", "padding": 0.01},
		{"kind": "problem_id", "x": 0.75, "y": 0.02, "w": 0.2, "h": 0.06, "color": "#4a4a4a", "padding": 0.01},
	})

	return env, c, aid, problems
}

// putIDRegions PUTs the assessment's typed id-regions via the API.
func putIDRegions(t *testing.T, c *http.Client, ts *httptest.Server, aid int64, regions []map[string]any) {
	t.Helper()
	resp := putJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/id-regions", ts.URL, aid), map[string]any{"regions": regions})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var eb bytes.Buffer
		_, _ = eb.ReadFrom(resp.Body)
		t.Fatalf("put id-regions: got %d — %s", resp.StatusCode, eb.String())
	}
}

// uploadLooseFiles posts a multipart scan-batches request with the given loose
// filenames (each gets trivial %PDF- bytes) and returns the decoded batch view.
func uploadLooseFiles(t *testing.T, c *http.Client, ts *httptest.Server, aid int64, filenames []string, extra map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, fn := range filenames {
		fw, err := mw.CreateFormFile("files", fn)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("%PDF-" + fn))
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/scan-batches", ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	var v map[string]any
	_ = json.Unmarshal(b.Bytes(), &v)
	// Recreate a response with the status preserved for callers that check it.
	return &http.Response{StatusCode: resp.StatusCode}, v
}

func uploadLooseFilesExpect(t *testing.T, c *http.Client, ts *httptest.Server, aid int64, filenames []string, extra map[string]string, wantStatus int) map[string]any {
	t.Helper()
	resp, v := uploadLooseFiles(t, c, ts, aid, filenames, extra)
	if resp.StatusCode != wantStatus {
		t.Fatalf("upload scan batch: got %d want %d — %v", resp.StatusCode, wantStatus, v)
	}
	return v
}

// driveSplit runs SplitSource for every recorded source id and returns the
// resulting page ids across all sources (order not significant).
func driveSplit(t *testing.T, env *testEnv, sourceIDs []int64) {
	t.Helper()
	for _, sid := range sourceIDs {
		if err := env.scans.SplitSource(context.Background(), sid); err != nil {
			t.Fatalf("SplitSource(%d): %v", sid, err)
		}
	}
}

// wireScanEnqueues wraps env.scans' real (queue-backed) enqueue closures with
// recording slices, mirroring the internal/scan package tests' fx pattern —
// the queue itself is real but never Start()ed, so jobs are driven directly by
// calling the recorded ids through the corresponding service method.
type scanRecorder struct {
	splits     []int64
	renders    []renderChunkIDs
	identifies []int64
}

type renderChunkIDs struct {
	SourceID int64
	PageIDs  []int64
}

func wireScanEnqueues(env *testEnv) *scanRecorder {
	rec := &scanRecorder{}
	wiredSplit := env.scans.EnqueueSplit
	wiredRender := env.scans.EnqueueRenderPages
	wiredIdentify := env.scans.EnqueueIdentifyPages
	env.scans.EnqueueSplit = func(ctx context.Context, tx pgx.Tx, ids []int64) error {
		rec.splits = append(rec.splits, ids...)
		return wiredSplit(ctx, tx, ids)
	}
	env.scans.EnqueueRenderPages = func(ctx context.Context, tx pgx.Tx, sourceID int64, pageIDs []int64) error {
		rec.renders = append(rec.renders, renderChunkIDs{SourceID: sourceID, PageIDs: pageIDs})
		return wiredRender(ctx, tx, sourceID, pageIDs)
	}
	env.scans.EnqueueIdentifyPages = func(ctx context.Context, tx pgx.Tx, ids []int64) error {
		rec.identifies = append(rec.identifies, ids...)
		return wiredIdentify(ctx, tx, ids)
	}
	return rec
}

func identityJSON(studentID, name, problem string) string {
	b, _ := json.Marshal(map[string]any{
		"student_id": studentID, "name": name, "problem": problem,
		"student_id_legible": studentID != "", "name_legible": name != "", "problem_legible": problem != "",
	})
	return string(b)
}

// --- Test 1: TestIDRegions_KindValidation ----------------------------------

func TestIDRegions_KindValidation(t *testing.T) {
	env, c, aid, _ := scanSetup(t)

	// Duplicate kind -> 400.
	resp := putJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/id-regions", env.ts.URL, aid), map[string]any{
		"regions": []map[string]any{
			{"kind": "student_id", "x": 0.1, "y": 0.1, "w": 0.2, "h": 0.05, "color": "#000", "padding": 0},
			{"kind": "student_id", "x": 0.5, "y": 0.5, "w": 0.2, "h": 0.05, "color": "#000", "padding": 0},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate kind: got %d want 400", resp.StatusCode)
	}

	// Bad kind -> 400.
	resp = putJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/id-regions", env.ts.URL, aid), map[string]any{
		"regions": []map[string]any{
			{"kind": "bogus", "x": 0.1, "y": 0.1, "w": 0.2, "h": 0.05, "color": "#000", "padding": 0},
		},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kind: got %d want 400", resp.StatusCode)
	}

	// Roundtrip GET keeps kind+color (scanSetup already PUT the three regions).
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/id-regions", env.ts.URL, aid), http.StatusOK)
	regions := got["regions"].([]any)
	if len(regions) != 3 {
		t.Fatalf("regions: got %d want 3 — %v", len(regions), regions)
	}
	seenKinds := map[string]string{}
	for _, raw := range regions {
		r := raw.(map[string]any)
		seenKinds[r["kind"].(string)] = r["color"].(string)
	}
	for _, k := range []string{"student_id", "name", "problem_id"} {
		if seenKinds[k] != "#4a4a4a" {
			t.Errorf("kind %s: color got %q want #4a4a4a", k, seenKinds[k])
		}
	}
}

// --- Test 2: TestCreateScanBatch_RequiresRegions ---------------------------

func TestCreateScanBatch_RequiresRegions(t *testing.T) {
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")
	a := postExpect(t, c, env.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "No Regions"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	// No id-regions PUT at all.

	resp, body := uploadLooseFiles(t, c, env.ts, aid, []string{"b01.pdf"}, map[string]string{"ocr_enabled": "0"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create batch w/o regions: got %d want 409 — %v", resp.StatusCode, body)
	}
	if v, ok := body["regions_incomplete"].(bool); !ok || !v {
		t.Errorf("409 body should carry regions_incomplete=true: %v", body)
	}
	if body["error"] == nil {
		t.Errorf("409 body should carry an error message: %v", body)
	}
}

// --- Test 3: TestScanPipeline_UploadToMatrix -------------------------------

func TestScanPipeline_UploadToMatrix(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	// Wire the OCR provider so identify can resolve student+problem agreement.
	env.scans.Providers = llm.StaticSource{
		"p": &fake.ScriptedProvider{Steps: []fake.JSONStep{
			{JSON: identityJSON("B11902001", "Student One", "Q1")},
		}},
	}

	view := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"b11902001.pdf"}, map[string]string{
		"ocr_enabled": "1", "ocr_provider": "p", "ocr_model": "m",
	}, http.StatusOK)
	if int(view["created"].(float64)) != 1 {
		t.Fatalf("created: %v", view)
	}
	batch := view["batch"].(map[string]any)
	if !batch["ocr_enabled"].(bool) || batch["ocr_provider"] != "p" || batch["ocr_model"] != "m" {
		t.Fatalf("batch OCR fields: %v", batch)
	}

	if len(rec.splits) != 1 {
		t.Fatalf("split enqueues: got %d want 1", len(rec.splits))
	}
	driveSplit(t, env, rec.splits)

	if len(rec.renders) == 0 {
		t.Fatal("render was not enqueued after split")
	}
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	if len(rec.identifies) == 0 {
		t.Fatal("identify was not enqueued after render")
	}
	for _, pid := range rec.identifies {
		if err := env.scans.IdentifyPage(context.Background(), pid, true); err != nil {
			t.Fatalf("IdentifyPage(%d): %v", pid, err)
		}
	}

	// GET scan-pages shows at least one page "assigned" with assigned_by_user=false.
	pagesResp := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := pagesResp["pages"].([]any)
	if len(pages) == 0 {
		t.Fatal("no scan pages returned")
	}
	var assignedFound bool
	for _, raw := range pages {
		p := raw.(map[string]any)
		if p["state"] == "assigned" {
			assignedFound = true
			if v, ok := p["assigned_by_user"].(bool); !ok || v {
				t.Errorf("auto-assigned page should have assigned_by_user=false: %v", p)
			}
		}
	}
	if !assignedFound {
		t.Fatalf("expected at least one assigned page: %v", pages)
	}

	// GET scan-matrix shows the (B11902001, problem 1) cell as "auto", and
	// problems carry {id, number} objects so the UI can label columns.
	matrix := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-matrix", env.ts.URL, aid), http.StatusOK)
	matrixProbs := matrix["problems"].([]any)
	if len(matrixProbs) != len(problems) {
		t.Fatalf("matrix problems: got %d want %d — %v", len(matrixProbs), len(problems), matrix)
	}
	for i, raw := range matrixProbs {
		n := i + 1
		pj, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("matrix problems[%d]: got %T want object — %v", i, raw, matrix)
		}
		if int64(pj["id"].(float64)) != problems[n] || int(pj["number"].(float64)) != n {
			t.Fatalf("matrix problems[%d]: got %v want id=%d number=%d", i, pj, problems[n], n)
		}
	}
	cellState := matrixCellFor(t, matrix, "B11902001", problems[1])
	if cellState != "auto" {
		t.Fatalf("matrix cell (B11902001,Q1): got %q want auto — %v", cellState, matrix)
	}

	// POST scan-finalize (ack_missing=true) -> 202, drive PromotePage.
	var promoted []int64
	env.scans.EnqueuePromotePages = func(ctx context.Context, tx pgx.Tx, items []scan.PromotePage) error {
		for _, it := range items {
			promoted = append(promoted, it.PageID)
		}
		return nil
	}
	finResp := postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/scan-finalize", env.ts.URL, aid), map[string]any{"ack_missing": true})
	fb := drainBody(t, finResp)
	if finResp.StatusCode != http.StatusAccepted {
		t.Fatalf("finalize: got %d want 202 — %s", finResp.StatusCode, fb)
	}
	if len(promoted) == 0 {
		t.Fatal("finalize should have enqueued at least one promote job")
	}
	for _, pid := range promoted {
		if err := env.scans.PromotePage(context.Background(), pid, false, 0, false); err != nil {
			t.Fatalf("PromotePage(%d): %v", pid, err)
		}
	}

	// GET scan-matrix now shows "promoted".
	matrix2 := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-matrix", env.ts.URL, aid), http.StatusOK)
	cellState2 := matrixCellFor(t, matrix2, "B11902001", problems[1])
	if cellState2 != "promoted" {
		t.Fatalf("matrix cell after finalize: got %q want promoted — %v", cellState2, matrix2)
	}

	// GET scan-pages ?state=orphan is empty.
	orphans := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages?state=orphan", env.ts.URL, aid), http.StatusOK)
	if len(orphans["pages"].([]any)) != 0 {
		t.Errorf("orphan filter should be empty: %v", orphans)
	}
}

// matrixCellFor finds the cell state for (studentExternalID, problemID) in a
// decoded matrixJSON.
func matrixCellFor(t *testing.T, matrix map[string]any, studentID string, problemID int64) string {
	t.Helper()
	rows := matrix["rows"].([]any)
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["student_id"] != studentID {
			continue
		}
		cells := row["cells"].([]any)
		for _, craw := range cells {
			cell := craw.(map[string]any)
			if int64(cell["problem_id"].(float64)) == problemID {
				return cell["state"].(string)
			}
		}
	}
	t.Fatalf("no cell found for student %s problem %d in %v", studentID, problemID, matrix)
	return ""
}

func drainBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return b.String()
}

// --- Test 4: TestScanPages_OrphanFilterAndAssign ---------------------------

func TestScanPages_OrphanFilterAndAssign(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	// Illegible-name script: student id and problem read fine, but name isn't —
	// this still isn't a full agreement resolution (id matches, name doesn't
	// independently confirm), so the page lands an orphan with a proposed id.
	env.scans.Providers = llm.StaticSource{
		"p": &fake.ScriptedProvider{Steps: []fake.JSONStep{
			{JSON: identityJSON("B11902001", "", "Q1")},
		}},
	}

	view := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"illegible.pdf"}, map[string]string{
		"ocr_enabled": "1", "ocr_provider": "p", "ocr_model": "m",
	}, http.StatusOK)
	_ = view
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}
	for _, pid := range rec.identifies {
		if err := env.scans.IdentifyPage(context.Background(), pid, true); err != nil {
			t.Fatalf("IdentifyPage(%d): %v", pid, err)
		}
	}

	pagesResp := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages?state=orphan", env.ts.URL, aid), http.StatusOK)
	pages := pagesResp["pages"].([]any)
	if len(pages) == 0 {
		t.Fatalf("expected at least one orphan page: %v", pagesResp)
	}
	firstPage := pages[0].(map[string]any)
	if firstPage["proposed_student_id"] != "B11902001" {
		t.Errorf("orphan page proposed_student_id: got %v want B11902001", firstPage["proposed_student_id"])
	}
	pageID := int64(firstPage["id"].(float64))

	// Assign the orphan page to (B11902001, problem 1).
	postExpect(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, pageID),
		map[string]any{"student_id": "B11902001", "problem_id": problems[1]}, http.StatusOK)

	// A second page assigned to the SAME cell must 409 with incumbent_page_id.
	view2 := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"second.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	_ = view2
	// New source id(s) appended to rec.splits; drive only the new ones.
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	// Find the second page (unassigned, unidentified — batch 2 has OCR off, so
	// it stays an unidentified processing row we can manually assign).
	allPages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	var secondPageID int64
	for _, raw := range allPages["pages"].([]any) {
		p := raw.(map[string]any)
		if int64(p["id"].(float64)) != pageID {
			secondPageID = int64(p["id"].(float64))
		}
	}
	if secondPageID == 0 {
		t.Fatalf("could not find a second page: %v", allPages)
	}

	resp := postJSON(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, secondPageID),
		map[string]any{"student_id": "B11902001", "problem_id": problems[1]})
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting assign: got %d want 409 — %s", resp.StatusCode, body)
	}
	var conflictBody map[string]any
	_ = json.Unmarshal([]byte(body), &conflictBody)
	if int64(conflictBody["incumbent_page_id"].(float64)) != pageID {
		t.Errorf("incumbent_page_id: got %v want %d", conflictBody["incumbent_page_id"], pageID)
	}
}

// --- Test 5: TestScanFinalize_MissingGate ----------------------------------

func TestScanFinalize_MissingGate(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	// No pages assigned at all: every (student, problem) cell is missing.

	resp := postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/scan-finalize", env.ts.URL, aid), nil)
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("finalize w/o ack: got %d want 409 — %s", resp.StatusCode, body)
	}
	var errBody map[string]any
	_ = json.Unmarshal([]byte(body), &errBody)
	if errBody["missing_cells"] == nil {
		t.Errorf("409 body should carry missing_cells: %v", errBody)
	}
	if v, ok := errBody["missing_cells"].(float64); !ok || v <= 0 {
		t.Errorf("missing_cells should be positive: %v", errBody)
	}

	// With ack_missing -> 202.
	resp2 := postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/scan-finalize", env.ts.URL, aid), map[string]any{"ack_missing": true})
	body2 := drainBody(t, resp2)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("finalize w/ ack: got %d want 202 — %s", resp2.StatusCode, body2)
	}
}

// --- Test 6: TestScanPageCrop_KindParam ------------------------------------

func TestScanPageCrop_KindParam(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	rec := wireScanEnqueues(env)

	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"b01.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	allPages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := allPages["pages"].([]any)
	if len(pages) == 0 {
		t.Fatal("no pages after render")
	}
	pageID := int64(pages[0].(map[string]any)["id"].(float64))

	// kind=name streams jpeg.
	resp, err := c.Get(fmt.Sprintf("%s/api/scan-pages/%d/crop?kind=name", env.ts.URL, pageID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("crop kind=name: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("crop content-type: got %q want image/jpeg", ct)
	}

	// kind=bogus -> 400.
	resp2, err := c.Get(fmt.Sprintf("%s/api/scan-pages/%d/crop?kind=bogus", env.ts.URL, pageID))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("crop kind=bogus: got %d want 400", resp2.StatusCode)
	}
}

// --- Test 7: TestScanPages_NoOCRTextInBatchList ----------------------------

func TestScanPages_NoOCRTextInBatchList(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	rec := wireScanEnqueues(env)

	env.scans.Providers = llm.StaticSource{
		"p": &fake.ScriptedProvider{Steps: []fake.JSONStep{
			{JSON: identityJSON("B11902001", "Student One", "Q1")},
		}},
	}
	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"b11902001.pdf"}, map[string]string{
		"ocr_enabled": "1", "ocr_provider": "p", "ocr_model": "m",
	}, http.StatusOK)
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}
	for _, pid := range rec.identifies {
		if err := env.scans.IdentifyPage(context.Background(), pid, true); err != nil {
			t.Fatalf("IdentifyPage(%d): %v", pid, err)
		}
	}

	body := getJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/scan-batches", env.ts.URL, aid))
	for _, piiField := range []string{
		`"ocr_student_id"`, `"ocr_name"`, `"ocr_problem"`, `"ocr_engine"`,
		`"proposed_student_id"`, `"proposal_source"`,
	} {
		if bytes.Contains(body, []byte(piiField)) {
			t.Errorf("scan-batches list response should never carry %s: %s", piiField, body)
		}
	}
}

// --- Test 8: TestAssignScanPage_WithdrawnStudent400 ------------------------
//
// Operator-input errors (bad student, bad problem, withdrawn student) are
// caller mistakes, not server faults — they must map to 400, not the assign
// catch-all's 500.
func TestAssignScanPage_WithdrawnStudent400(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	if _, err := env.st.Pool.Exec(t.Context(),
		"UPDATE students SET withdrawn_at = now() WHERE student_id = $1", "B11902001"); err != nil {
		t.Fatalf("withdraw student: %v", err)
	}

	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"b01.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	allPages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := allPages["pages"].([]any)
	if len(pages) == 0 {
		t.Fatal("no pages after render")
	}
	pageID := int64(pages[0].(map[string]any)["id"].(float64))

	resp := postJSON(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, pageID),
		map[string]any{"student_id": "B11902001", "problem_id": problems[1]})
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("assign withdrawn student: got %d want 400 — %s", resp.StatusCode, body)
	}
	var errBody map[string]any
	_ = json.Unmarshal([]byte(body), &errBody)
	msg, _ := errBody["error"].(string)
	if !strings.Contains(msg, "withdrawn") {
		t.Errorf("400 body error should mention withdrawn: %v", errBody)
	}
}

// --- Test 9: TestResolveScanPageConflict_NotParked400 ----------------------
//
// resolve-conflict on a page that was never parked is an operator mistake
// (wrong page id / stale UI state) and must map to 400, not
// handlePageMutationError's 500 catch-all.
func TestResolveScanPageConflict_NotParked400(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	rec := wireScanEnqueues(env)

	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"b01.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	allPages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := allPages["pages"].([]any)
	if len(pages) == 0 {
		t.Fatal("no pages after render")
	}
	// A freshly-rendered, unidentified page is an orphan — never parked.
	pageID := int64(pages[0].(map[string]any)["id"].(float64))

	resp := postJSON(t, c, fmt.Sprintf("%s/api/scan-pages/%d/resolve-conflict", env.ts.URL, pageID),
		map[string]any{"action": "keep"})
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("resolve-conflict on non-parked page: got %d want 400 — %s", resp.StatusCode, body)
	}
}

// --- OCR-provider trap (2026-07-11): creation guard + bulk recovery ---------

// TestCreateScanBatch_OCRWithoutProvider400: cloud identification has no
// default provider, so ocr_enabled with an empty ocr_provider is rejected at
// the door — otherwise every page of the batch terminal-errors "OCR provider
// unavailable" with no recovery (the batch's provider is immutable).
func TestCreateScanBatch_OCRWithoutProvider400(t *testing.T) {
	env, c, aid, _ := scanSetup(t)

	resp, body := uploadLooseFiles(t, c, env.ts, aid, []string{"b01.pdf"}, map[string]string{"ocr_enabled": "1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ocr on w/o provider: got %d want 400 — %v", resp.StatusCode, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "choose an OCR provider") {
		t.Errorf("400 message should tell the operator to choose a provider: %v", body)
	}
	batches := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-batches", env.ts.URL, aid), http.StatusOK)
	if len(batches["batches"].([]any)) != 0 {
		t.Errorf("no batch should be created: %v", batches)
	}
}

// erroredScanBatch uploads a one-source batch against an unconfigured provider
// and drives split→render→identify so every page terminal-errors "OCR provider
// unavailable". Returns the batch id.
func erroredScanBatch(t *testing.T, env *testEnv, c *http.Client, aid int64, rec *scanRecorder) int64 {
	t.Helper()
	view := uploadLooseFilesExpect(t, c, env.ts, aid, []string{"ghost.pdf"}, map[string]string{
		"ocr_enabled": "1", "ocr_provider": "ghost", "ocr_model": "m",
	}, http.StatusOK)
	batchID := int64(view["batch"].(map[string]any)["id"].(float64))
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}
	for _, pid := range rec.identifies {
		if err := env.scans.IdentifyPage(context.Background(), pid, true); err != nil {
			t.Fatalf("IdentifyPage(%d): %v", pid, err)
		}
	}
	return batchID
}

// erroredScanPageCount returns how many of the assessment's pages read "error".
func erroredScanPageCount(t *testing.T, c *http.Client, ts *httptest.Server, aid int64) int {
	t.Helper()
	pages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages?state=error", ts.URL, aid), http.StatusOK)
	return len(pages["pages"].([]any))
}

func TestScanBatchRetryErrored_SwitchesProviderAndReruns(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	rec := wireScanEnqueues(env)
	env.scans.Providers = llm.StaticSource{"p": &fake.ScriptedProvider{NameStr: "p", Steps: []fake.JSONStep{
		{JSON: identityJSON("B11902001", "Student One", "Q1")},
		{JSON: identityJSON("B11902002", "Student Two", "Q2")},
	}}}
	batchID := erroredScanBatch(t, env, c, aid, rec)

	errored := erroredScanPageCount(t, c, env.ts, aid)
	if errored == 0 {
		t.Fatal("fixture: expected errored pages")
	}

	// Unknown batch -> 404.
	resp404 := postJSON(t, c, fmt.Sprintf("%s/api/scan-batches/%d/retry-errored", env.ts.URL, int64(999999)), nil)
	body404 := drainBody(t, resp404)
	if resp404.StatusCode != http.StatusNotFound {
		t.Fatalf("retry-errored on unknown batch: got %d want 404 — %s", resp404.StatusCode, body404)
	}

	// Unknown provider -> 400, nothing retried.
	resp400 := postJSON(t, c, fmt.Sprintf("%s/api/scan-batches/%d/retry-errored", env.ts.URL, batchID),
		map[string]any{"ocr_provider": "nope"})
	body400 := drainBody(t, resp400)
	if resp400.StatusCode != http.StatusBadRequest {
		t.Fatalf("retry-errored w/ unknown provider: got %d want 400 — %s", resp400.StatusCode, body400)
	}
	if got := erroredScanPageCount(t, c, env.ts, aid); got != errored {
		t.Fatalf("errored pages after rejected retry: got %d want %d", got, errored)
	}

	// A working provider: the batch is repointed, every errored page cleared
	// and re-enqueued for identify.
	rec.identifies = nil
	out := postExpect(t, c, fmt.Sprintf("%s/api/scan-batches/%d/retry-errored", env.ts.URL, batchID),
		map[string]any{"ocr_provider": "p", "ocr_model": "m2"}, http.StatusOK)
	if int(out["retried"].(float64)) != errored {
		t.Fatalf("retried: got %v want %d", out["retried"], errored)
	}
	batches := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-batches", env.ts.URL, aid), http.StatusOK)
	b := batches["batches"].([]any)[0].(map[string]any)
	if b["ocr_provider"] != "p" || b["ocr_model"] != "m2" {
		t.Fatalf("batch provider not switched: %v", b)
	}
	if len(rec.identifies) != errored {
		t.Fatalf("identify enqueues: got %d want %d", len(rec.identifies), errored)
	}

	// Driving the re-enqueued identifies against the working provider succeeds:
	// no page is left in state "error".
	for _, pid := range rec.identifies {
		if err := env.scans.IdentifyPage(context.Background(), pid, true); err != nil {
			t.Fatalf("IdentifyPage(%d): %v", pid, err)
		}
	}
	if got := erroredScanPageCount(t, c, env.ts, aid); got != 0 {
		t.Fatalf("errored pages after retry+identify: got %d want 0", got)
	}
}

func TestScanBatchDiscardErrored(t *testing.T) {
	env, c, aid, _ := scanSetup(t)
	rec := wireScanEnqueues(env)
	batchID := erroredScanBatch(t, env, c, aid, rec)

	errored := erroredScanPageCount(t, c, env.ts, aid)
	if errored == 0 {
		t.Fatal("fixture: expected errored pages")
	}

	out := postExpect(t, c, fmt.Sprintf("%s/api/scan-batches/%d/discard-errored", env.ts.URL, batchID), nil, http.StatusOK)
	if int(out["discarded"].(float64)) != errored {
		t.Fatalf("discarded: got %v want %d", out["discarded"], errored)
	}
	if got := erroredScanPageCount(t, c, env.ts, aid); got != 0 {
		t.Fatalf("errored pages after discard: got %d want 0", got)
	}
	discarded := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages?state=discarded", env.ts.URL, aid), http.StatusOK)
	if got := len(discarded["pages"].([]any)); got != errored {
		t.Fatalf("discarded pages: got %d want %d", got, errored)
	}
}

// --- Orphan manual assign: normalized student-id lookup ---------------------

// TestAssignScanPage_NormalizedIDLookup: manual page assignment accepts a
// case-variant external id via the shared exact-then-normalized lookup
// (roster-lifecycle plan 2026-07-10, task R3), 400s a normalization-ambiguous
// id with "ambiguous student id", keeps 404 for a true unknown, and keeps the
// withdraw guard's existing rejection on an exact match.
func TestAssignScanPage_NormalizedIDLookup(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	// OCR off: pages stay unidentified "processing" rows, manually assignable.
	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"stack.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)
	for _, chunk := range rec.renders {
		if err := env.scans.RenderPages(context.Background(), chunk.SourceID, chunk.PageIDs); err != nil {
			t.Fatalf("RenderPages: %v", err)
		}
	}

	allPages := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := allPages["pages"].([]any)
	if len(pages) == 0 {
		t.Fatalf("no scan pages: %v", allPages)
	}
	pageID := int64(pages[0].(map[string]any)["id"].(float64))
	assignURL := fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, pageID)

	// Ambiguity: two roster ids colliding under studentid.Normalize -> 400.
	seedStudent(t, env.st, "B66", "Coll One", "c1@x.edu")
	seedStudent(t, env.st, "b66", "Coll Two", "c2@x.edu")
	resp := postJSON(t, c, assignURL, map[string]any{"student_id": "b-66", "problem_id": problems[1]})
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "ambiguous student id") {
		t.Fatalf("ambiguous assign: got %d — %s", resp.StatusCode, body)
	}

	// A true unknown keeps its 404.
	resp = postJSON(t, c, assignURL, map[string]any{"student_id": "Z9999", "problem_id": problems[1]})
	body = drainBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown assign: got %d — %s", resp.StatusCode, body)
	}

	// Withdraw guard survives the new lookup: exact id of a withdrawn student
	// keeps scan.AssignPage's existing rejection.
	gone, err := env.st.Q.UpsertStudent(context.Background(), db.UpsertStudentParams{StudentID: "B44", Name: "Gone", Email: "gone@x.edu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.Q.SetStudentWithdrawn(context.Background(), db.SetStudentWithdrawnParams{ID: gone.ID, Withdrawn: true}); err != nil {
		t.Fatal(err)
	}
	resp = postJSON(t, c, assignURL, map[string]any{"student_id": "B44", "problem_id": problems[1]})
	body = drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "withdrawn") {
		t.Fatalf("withdrawn assign: got %d — %s", resp.StatusCode, body)
	}

	// Case-variant id resolves to the roster's canonical B11902001 and assigns.
	postExpect(t, c, assignURL, map[string]any{"student_id": "b11902001", "problem_id": problems[1]}, http.StatusOK)
	after := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	var assigned string
	for _, raw := range after["pages"].([]any) {
		p := raw.(map[string]any)
		if int64(p["id"].(float64)) == pageID {
			assigned, _ = p["assigned_student_id"].(string)
		}
	}
	if assigned != "B11902001" {
		t.Fatalf("assigned_student_id: got %q want B11902001", assigned)
	}
}

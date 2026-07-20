package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// phase4Setup: assessment + 1 problem (max 10, rubric 6+4) + 2 students uploaded.
// Returns env, logged-in client, and ids.
func phase4Setup(t *testing.T) (*testEnv, *http.Client, int64, int64, int64) {
	t.Helper()
	env := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	seedRole(t, env.st, "lect@ntu.edu.tw", "lecturer")
	resp := devLogin(t, env.ts, c, "lect@ntu.edu.tw")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	aid, pid, rvID := phase4SetupOn(t, env, c)
	return env, c, aid, pid, rvID
}

// phase4SetupOn builds the same assessment + 1 problem (max 10, rubric 6+4) + 2
// students fixture as phase4Setup, but against an already-wired env and an
// already-logged-in client — for tests that need a non-default harnessEnv (e.g. a
// specific config.Config for the monthly budget cap).
func phase4SetupOn(t *testing.T, env *testEnv, c *http.Client) (int64, int64, int64) {
	t.Helper()
	a := postExpect(t, c, env.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "AI Exam"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", env.ts.URL, aid),
		map[string]any{"number": 1, "title": "DP", "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))
	rv := postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Recurrence", "points": "6"},
			{"description": "Complexity", "points": "4"},
		},
	}, http.StatusCreated)
	_ = rv

	for _, sid := range []string{"b01", "b02"} {
		seedStudent(t, env.st, sid, "Student "+sid, sid+"@x.edu")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, sid := range []string{"b01", "b02"} {
		fw, _ := mw.CreateFormFile("files", sid+".pdf")
		_, _ = fw.Write([]byte("%PDF-" + sid))
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/submissions", env.ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Upload only stages + enqueues now (D27, F1); drive the ingest worker directly
	// so the submissions exist before the rest of phase4's tests run masks/grading.
	driveDirectUploads(t, env, aid)

	return aid, pid, int64(rv["id"].(float64))
}

// createFakeMethod makes a method on the fake provider via the API.
func createFakeMethod(t *testing.T, env *testEnv, c *http.Client) int64 {
	t.Helper()
	// Seed the default prompt template (idempotent), then read its id.
	if err := grading.EnsureSeeds(context.Background(), env.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake method",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	return int64(m["id"].(float64))
}

// acceptAllMasks applies regions, drives the enqueued mask.page jobs directly (D27,
// F2: the handler only enqueues now, mirroring how scan tests drive PromoteFile),
// and accepts every page through the API.
func acceptAllMasks(t *testing.T, env *testEnv, c *http.Client, aid int64) {
	t.Helper()
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
	pages, err := env.st.Q.ListPagesForAssessment(context.Background(), aid)
	if err != nil {
		t.Fatalf("ListPagesForAssessment: %v", err)
	}
	for _, pg := range pages {
		if err := env.ing.MaskPage(context.Background(), pg.ID, false); err != nil {
			t.Fatalf("MaskPage(%d): %v", pg.ID, err)
		}
	}

	review := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	for _, pg := range review["pages"] {
		r := postJSON(t, c, fmt.Sprintf("%s/api/answer-pages/%d/mask-review", env.ts.URL, int64(pg["page_id"].(float64))), map[string]string{"status": "accepted"})
		r.Body.Close()
	}
}

// driveRun executes all pending leaves of a run synchronously.
func driveRun(t *testing.T, env *testEnv, runID int64, finalAttempt bool) {
	t.Helper()
	ctx := context.Background()
	if err := env.runner.Plan(ctx, runID); err != nil {
		t.Fatalf("plan: %v", err)
	}
	items, err := env.st.Q.ListRunItems(ctx, db.ListRunItemsParams{RunID: runID, ItemLimit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.State == "pending" || it.State == "running" {
			_ = env.runner.ExecuteLeaf(ctx, it.ID, finalAttempt) // leaf errors land on items
		}
	}
}

// spotCheckAgreeAll satisfies the spot-check gate (trust spec §4, D37) for a
// completed run by verdicting every sampled record "agree" — the fast path
// tests take when they just need the gate open, not to exercise spot-check
// itself.
func spotCheckAgreeAll(t *testing.T, c *http.Client, baseURL string, runID int64) {
	t.Helper()
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", baseURL, runID), http.StatusOK)
	samples, _ := got["samples"].([]any)
	for _, s := range samples {
		sc := s.(map[string]any)
		id := int64(sc["id"].(float64))
		postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/%d", baseURL, runID, id),
			map[string]any{"verdict": "agree", "note": ""}, http.StatusOK)
	}
}

func TestPhase4_MaskGateBlocksRun(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)

	// Preview shows blockers before masking.
	preview := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if preview["answers"].(float64) != 2 || preview["mask_blockers"].(float64) != 2 {
		t.Fatalf("preview: %v", preview)
	}

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))

	if err := env.runner.Plan(context.Background(), runID); err != nil {
		t.Fatalf("plan returned transport error: %v", err)
	}
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	r := got["run"].(map[string]any)
	if r["status"] != "failed" {
		t.Fatalf("run should fail the mask gate, got %v", r["status"])
	}
}

// TestApplyMasks_EnqueuesThenSecondRunSkipsAll drives the D27/F2 async contract
// directly: handleApplyMasks only plans + enqueues (202 {enqueued, skipped}), the
// mask.page jobs must be driven separately to actually mask, and a second apply once
// everything is up to date reports enqueued=0 with every page skipped (fingerprint
// match) while review status is left untouched (masking is idempotent).
func TestApplyMasks_EnqueuesThenSecondRunSkipsAll(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)

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

	// First apply: both students' pages need masking, nothing skipped yet.
	first := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/apply", env.ts.URL, aid), nil, http.StatusAccepted)
	if first["enqueued"].(float64) != 2 || first["skipped"].(float64) != 0 {
		t.Fatalf("first apply: got %v want enqueued=2 skipped=0", first)
	}

	// Drive the enqueued mask.page jobs directly (no River worker in this test).
	pages, err := env.st.Q.ListPagesForAssessment(context.Background(), aid)
	if err != nil {
		t.Fatalf("ListPagesForAssessment: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("pages: got %d want 2", len(pages))
	}
	for _, pg := range pages {
		if err := env.ing.MaskPage(context.Background(), pg.ID, false); err != nil {
			t.Fatalf("MaskPage(%d): %v", pg.ID, err)
		}
	}

	// Review reflects both pages masked, review status reset to pending (D10).
	review := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	if len(review["pages"]) != 2 {
		t.Fatalf("review pages: %v", review["pages"])
	}
	for _, pg := range review["pages"] {
		if masked, _ := pg["masked"].(bool); !masked {
			t.Errorf("page %v should be masked: %v", pg["page_id"], pg)
		}
		if pg["review_status"] != "pending" {
			t.Errorf("page %v review_status: got %v want pending", pg["page_id"], pg["review_status"])
		}
	}

	// Accept one page's review, then re-apply: fingerprint matches for both pages
	// (same regions, same source images) so the second run enqueues nothing and the
	// accepted review status must survive (the skip path never resets review, D27/F2).
	acceptResp := postJSON(t, c, fmt.Sprintf("%s/api/answer-pages/%d/mask-review", env.ts.URL, int64(review["pages"][0]["page_id"].(float64))), map[string]string{"status": "accepted"})
	acceptResp.Body.Close()

	second := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/masks/apply", env.ts.URL, aid), nil, http.StatusAccepted)
	if second["enqueued"].(float64) != 0 || second["skipped"].(float64) != 2 {
		t.Fatalf("second apply: got %v want enqueued=0 skipped=2", second)
	}

	reviewAfter := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/masks/review", env.ts.URL, aid), http.StatusOK)
	for _, pg := range reviewAfter["pages"] {
		if int64(pg["page_id"].(float64)) == int64(review["pages"][0]["page_id"].(float64)) {
			if pg["review_status"] != "accepted" {
				t.Errorf("accepted page's review_status should survive a no-op re-apply: got %v", pg["review_status"])
			}
		}
	}
}

func TestPhase4_FullRunGradesAndDerivesOfficials(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	r := got["run"].(map[string]any)
	counts := r["counts"].(map[string]any)
	if r["status"] != "completed" || counts["succeeded"].(float64) != 2 {
		t.Fatalf("run: status=%v counts=%v", r["status"], counts)
	}

	// Each answer got a model record: fake scores 1 per criterion → total 2.
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	recs := detail["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	rec := recs[0].(map[string]any)
	if rec["source"] != "model" || rec["total"] != "2" || rec["confidence"] != "high" {
		t.Errorf("model record: source=%v total=%v confidence=%v", rec["source"], rec["total"], rec["confidence"])
	}
	// The record pins the grading stance (D25): the fake method sets no policy, so it
	// defaults to 'standard' and is written non-null on the model record.
	dbRec, err := env.st.Q.GetRecord(context.Background(), int64(rec["id"].(float64)))
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if !dbRec.Policy.Valid || dbRec.Policy.String != "standard" {
		t.Errorf("record policy = %+v, want valid 'standard'", dbRec.Policy)
	}

	// Round-based grading (0027): the completed run alone sets nothing official.
	// Choosing its method as the exam's final source derives an official for
	// every unflagged graded answer — the replacement for the old bulk accept.
	if totals := officialTotals(t, c, env.ts.URL, pid); len(totals) != 0 {
		t.Fatalf("completed run without a chosen source should set no officials, got %v", totals)
	}
	res := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	if moved := int(res["officials_moved"].(float64)); moved != 2 {
		t.Fatalf("choosing the run's method should derive 2 officials, moved %d", moved)
	}
	if totals := officialTotals(t, c, env.ts.URL, pid); len(totals) != 2 {
		t.Fatalf("expected derived officials for both students, got %v", totals)
	}
}

// TestPhase4_RecordPinsConfiguredPolicy: a method configured with a non-default
// policy writes that stance onto the produced model record (D25).
func TestPhase4_RecordPinsConfiguredPolicy(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)

	// Method on the fake provider with policy 'strict'.
	if err := grading.EnsureSeeds(context.Background(), env.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Strict method",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2, "policy": "strict",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	methodID := int64(m["id"].(float64))

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	recs := detail["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	rec := recs[0].(map[string]any)
	dbRec, err := env.st.Q.GetRecord(context.Background(), int64(rec["id"].(float64)))
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if !dbRec.Policy.Valid || dbRec.Policy.String != "strict" {
		t.Errorf("record policy = %+v, want valid 'strict'", dbRec.Policy)
	}
}

func TestPhase4_FailureRetryAndIllegible(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Scope to one answer so scripting the fake is deterministic.
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))

	// 1) Provider error on final attempt → terminal failed item, run completed-with-failure...
	env.fakeProv.Script = []fake.Step{{Err: errors.New("provider exploded")}}
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": answerID, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true) // finalAttempt → convert to terminal failure

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	counts := got["run"].(map[string]any)["counts"].(map[string]any)
	if counts["failed"].(float64) != 1 {
		t.Fatalf("expected 1 failed item: %v", counts)
	}

	// 2) retry-failed re-runs only the failed leaf; script is exhausted → success.
	retried := postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil, http.StatusOK)
	if retried["retried"].(float64) != 1 {
		t.Fatalf("retry: %v", retried)
	}
	driveRun(t, env, runID, false)
	got = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	r := got["run"].(map[string]any)
	if r["status"] != "completed" || r["counts"].(map[string]any)["succeeded"].(float64) != 1 {
		t.Fatalf("after retry: %v", r)
	}

	// 3) Illegible refusal path: record with NULL total + answer flagged (D12).
	// The script cursor is the call count — reset it before scripting anew.
	env.fakeProv.Calls = nil
	env.fakeProv.Script = []fake.Step{{Confidence: "illegible"}}
	otherAnswer := int64(students["students"][1]["answer_id"].(float64))
	run2 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": otherAnswer, "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run2["id"].(float64)), false)

	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, otherAnswer), http.StatusOK)
	recs := detail["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	rec := recs[0].(map[string]any)
	if rec["confidence"] != "illegible" || rec["total"] != nil {
		t.Errorf("illegible record: confidence=%v total=%v", rec["confidence"], rec["total"])
	}
	flags := detail["answer"].(map[string]any)["flags"].([]any)
	found := false
	for _, f := range flags {
		if f == "illegible" {
			found = true
		}
	}
	if !found {
		t.Errorf("answer should be flagged illegible: %v", flags)
	}
}

// TestHandleGetRun_DefaultFiltersToInterestingItems pins F20's shape contract:
// by default handleGetRun returns only failed/running items (the counts summary
// already covers succeeded/pending/skipped), `?all=1` returns every item, and
// both echo a `truncated` flag (false here, under the 2000-row cap).
func TestHandleGetRun_DefaultFiltersToInterestingItems(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// One student's leaf errors permanently (failed), the other succeeds.
	env.fakeProv.Script = []fake.Step{{Err: errors.New("boom")}}
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true) // finalAttempt on the errored leaf → terminal failed

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	counts := got["run"].(map[string]any)["counts"].(map[string]any)
	if counts["failed"].(float64) != 1 || counts["succeeded"].(float64) != 1 {
		t.Fatalf("setup: expected 1 failed + 1 succeeded, got %v", counts)
	}
	if got["all"] != false {
		t.Fatalf("default response should echo all=false: %v", got["all"])
	}
	if got["truncated"] != false {
		t.Fatalf("default response should not be truncated: %v", got["truncated"])
	}
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("default items should be filtered to the 1 failed item, got %d: %v", len(items), items)
	}
	if state := items[0].(map[string]any)["state"]; state != "failed" {
		t.Fatalf("default item state: got %v want failed", state)
	}

	all := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d?all=1", env.ts.URL, runID), http.StatusOK)
	if all["all"] != true {
		t.Fatalf("all=1 response should echo all=true: %v", all["all"])
	}
	if all["truncated"] != false {
		t.Fatalf("all=1 response should not be truncated: %v", all["truncated"])
	}
	allItems := all["items"].([]any)
	if len(allItems) != 2 {
		t.Fatalf("all=1 should return every item, got %d: %v", len(allItems), allItems)
	}
	states := map[string]bool{}
	for _, it := range allItems {
		states[it.(map[string]any)["state"].(string)] = true
	}
	if !states["failed"] || !states["succeeded"] {
		t.Fatalf("all=1 states: %v", states)
	}
}

// TestListRuns_Filters: GET /api/runs?assessment_id=…&status=… filters
// server-side — the list endpoint caps at 50 rows, so client-side filtering in
// the UI would silently miss older runs past the cap.
func TestListRuns_Filters(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)

	launch := func(assessmentID int64) int64 {
		run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
			"assessment_id": assessmentID, "scope_kind": "assessment", "method_id": methodID,
		}, http.StatusCreated)
		return int64(run["id"].(float64))
	}
	id1 := launch(aid)
	id2 := launch(aid)
	// Cheap state poke (house style): the filter only cares about the column value.
	mustExec(t, env.st, "UPDATE grading_runs SET status = 'completed' WHERE id = $1", id2)

	a2 := postExpect(t, c, env.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Other Exam"}, http.StatusCreated)
	aid2 := int64(a2["id"].(float64))
	id3 := launch(aid2)

	listIDs := func(qs string) map[int64]bool {
		got := getJSON[map[string]any](t, c, env.ts.URL+"/api/runs"+qs, http.StatusOK)
		ids := map[int64]bool{}
		for _, r := range got["runs"].([]any) {
			ids[int64(r.(map[string]any)["id"].(float64))] = true
		}
		return ids
	}

	all := listIDs("")
	if !all[id1] || !all[id2] || !all[id3] {
		t.Fatalf("unfiltered list should contain all three runs, got %v", all)
	}

	byAssessment := listIDs(fmt.Sprintf("?assessment_id=%d", aid))
	if !byAssessment[id1] || !byAssessment[id2] || byAssessment[id3] {
		t.Fatalf("assessment_id=%d filter: got %v", aid, byAssessment)
	}

	byStatus := listIDs("?status=completed")
	if !byStatus[id2] || byStatus[id1] || byStatus[id3] {
		t.Fatalf("status=completed filter: got %v", byStatus)
	}

	combined := listIDs(fmt.Sprintf("?assessment_id=%d&status=pending", aid))
	if !combined[id1] || combined[id2] || combined[id3] {
		t.Fatalf("combined filter: got %v", combined)
	}

	// Bad filter values are a 400, not a silently empty list — catches typos.
	getJSON[map[string]any](t, c, env.ts.URL+"/api/runs?status=bogus", http.StatusBadRequest)
	getJSON[map[string]any](t, c, env.ts.URL+"/api/runs?assessment_id=abc", http.StatusBadRequest)
}

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// runPreviewWarnings fetches GET /api/runs/preview with the given query string and
// indexes the response's warnings array by code (each code appears at most
// once) — the launch-dialog counterpart of warnings_test.go's warningsFor.
func runPreviewWarnings(t *testing.T, c *http.Client, baseURL, query string) map[string]map[string]any {
	t.Helper()
	got := getJSON[map[string]any](t, c, baseURL+"/api/runs/preview?"+query, http.StatusOK)
	raw, ok := got["warnings"].([]any)
	if !ok {
		t.Fatalf("run preview must carry a warnings array: %v", got)
	}
	out := make(map[string]map[string]any, len(raw))
	for _, w := range raw {
		wm := w.(map[string]any)
		code := wm["code"].(string)
		if _, dup := out[code]; dup {
			t.Fatalf("warning code %q emitted twice: %v", code, raw)
		}
		out[code] = wm
	}
	return out
}

// setProviderEnabled toggles a provider row's enabled flag through the PATCH API.
func setProviderEnabled(t *testing.T, c *http.Client, baseURL string, providerID int64, enabled bool) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"enabled": enabled})
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/providers/%d", baseURL, providerID), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch provider enabled=%v: got %d want 200", enabled, resp.StatusCode)
	}
}

// The launch preview repeats the assessment-wide intake hazards (stranded /
// assigned-unpromoted pages — same derivations and detail string as the
// standing warnings endpoint) so the Launch dialog can warn before tokens are
// spent, and scanSetup's three rubric-less problems flag no_rubric_problems
// under assessment scope.
func TestRunPreview_IntakeWarningsAssessmentWide(t *testing.T) {
	env, c, aid, problems := scanSetup(t)
	rec := wireScanEnqueues(env)

	uploadLooseFilesExpect(t, c, env.ts, aid, []string{"a.pdf", "b.pdf", "c.pdf"}, map[string]string{"ocr_enabled": "0"}, http.StatusOK)
	driveSplit(t, env, rec.splits)

	pagesResp := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/scan-pages", env.ts.URL, aid), http.StatusOK)
	pages := pagesResp["pages"].([]any)
	if len(pages) < 4 {
		t.Fatalf("need at least 4 pages to cover the states, got %d", len(pages))
	}
	ids := make([]int64, 0, len(pages))
	for _, raw := range pages {
		ids = append(ids, int64(raw.(map[string]any)["id"].(float64)))
	}

	// Nudge column facts into each derived state (D2: state derives, never
	// stored). ids[0] is orphaned WITH a proposed cell that nothing covers
	// (stranded_scan_pages); ids[1]/ids[2] are parked/errored with no identity
	// at all (unidentified_scan_pages).
	mustExec(t, env.st, `UPDATE scan_pages SET identified_at = now(),
		proposed_student_id = (SELECT id FROM students WHERE student_id = 'B11902001'),
		proposed_problem_id = $2 WHERE id = $1`, ids[0], problems[2]) // orphaned, uncovered cell
	mustExec(t, env.st, `UPDATE scan_pages SET identified_at = now(), parked_reason = 'conflict' WHERE id = $1`, ids[1]) // parked
	mustExec(t, env.st, `UPDATE scan_pages SET error = 'render failed: boom' WHERE id = $1`, ids[2])                     // errored
	postExpect(t, c, fmt.Sprintf("%s/api/scan-pages/%d/assign", env.ts.URL, ids[3]),
		map[string]any{"student_id": "B11902001", "problem_id": problems[1]}, http.StatusOK) // assigned, unpromoted

	ws := runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid))
	stranded := wantWarning(t, ws, "stranded_scan_pages", "warning", 1)
	if stranded["detail"] != "1 orphaned, 0 parked, 0 failed; answers affected: 1" {
		t.Errorf("stranded detail = %v, want '1 orphaned, 0 parked, 0 failed; answers affected: 1'", stranded["detail"])
	}
	wantWarning(t, ws, "unidentified_scan_pages", "warning", 2)
	wantWarning(t, ws, "assigned_unpromoted_pages", "warning", 1)
	wantWarning(t, ws, "no_rubric_problems", "warning", 3)
	refuteWarning(t, ws, "quarantined_uploads")
	refuteWarning(t, ws, "active_run_overlap")
	refuteWarning(t, ws, "provider_disabled") // no method_id in the query
}

// An open quarantine row surfaces in the launch preview: a run over "all
// answers" silently excludes whatever is stuck in quarantine.
func TestRunPreview_QuarantinedUploads(t *testing.T) {
	f := publishSetup(t)

	uploadFakePDF(t, f.c, f.ts, f.aid, "not-on-roster.pdf")
	driveDirectUploads(t, f.env, f.aid)

	ws := runPreviewWarnings(t, f.c, f.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=assessment", f.aid))
	wantWarning(t, ws, "quarantined_uploads", "warning", 1)
	refuteWarning(t, ws, "stranded_scan_pages")
	refuteWarning(t, ws, "no_rubric_problems") // publishSetup's problem has a rubric
}

// no_rubric_problems is scoped to the RUN, not the assessment: a problem-scoped
// launch only warns when THAT problem lacks a rubric, and an answer-scoped
// launch follows its answer's problem.
func TestRunPreview_NoRubricProblemsScopedToRun(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t) // problem 1 has a rubric

	p2 := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", env.ts.URL, aid),
		map[string]any{"number": 2, "title": "Greedy", "max_points": "10"}, http.StatusCreated)
	pid2 := int64(p2["id"].(float64)) // no rubric

	// Assessment scope sees the one rubric-less problem.
	ws := runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid))
	wantWarning(t, ws, "no_rubric_problems", "warning", 1)

	// Problem scope: the rubric'd problem is clean, the bare one warns.
	ws = runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=problem&scope_id=%d", aid, pid))
	refuteWarning(t, ws, "no_rubric_problems")
	ws = runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=problem&scope_id=%d", aid, pid2))
	wantWarning(t, ws, "no_rubric_problems", "warning", 1)

	// Answer scope follows the answer's problem (problem 1 — rubric'd, clean).
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	if len(students["students"]) == 0 {
		t.Fatal("phase4Setup should have answers for problem 1")
	}
	answerID := int64(students["students"][0]["answer_id"].(float64))
	ws = runPreviewWarnings(t, c, env.ts.URL, fmt.Sprintf("assessment_id=%d&scope_kind=answer&scope_id=%d", aid, answerID))
	refuteWarning(t, ws, "no_rubric_problems")
}

// active_run_overlap appears while a pending/running run exists for the same
// assessment (two overlapping runs race officials) and clears on completion.
func TestRunPreview_ActiveRunOverlap(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	query := fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid)
	ws := runPreviewWarnings(t, c, env.ts.URL, query)
	refuteWarning(t, ws, "active_run_overlap")

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	ws = runPreviewWarnings(t, c, env.ts.URL, query)
	wantWarning(t, ws, "active_run_overlap", "warning", 1)

	driveRun(t, env, int64(run["id"].(float64)), false)
	ws = runPreviewWarnings(t, c, env.ts.URL, query)
	refuteWarning(t, ws, "active_run_overlap")
}

// TestRunScope_ExcludesWithdrawn_InFlightRunUnaffected pins locked semantics (c)
// (roster-lifecycle plan 2026-07-10): a NEW run's scope excludes withdrawn students'
// answers (no tokens spent on someone who will never receive results), while an
// already-launched run keeps its items — the scope resolved at launch.
func TestRunScope_ExcludesWithdrawn_InFlightRunUnaffected(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Both students in scope while active.
	preview := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if int(preview["answers"].(float64)) != 2 {
		t.Fatalf("preview answers = %v, want 2 before any withdrawal", preview["answers"])
	}

	// Launch + plan run1 with both active: two items.
	run1 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	run1ID := int64(run1["id"].(float64))
	if err := env.runner.Plan(t.Context(), run1ID); err != nil {
		t.Fatalf("plan run1: %v", err)
	}
	items, err := env.st.Q.ListRunItems(t.Context(), db.ListRunItemsParams{RunID: run1ID, ItemLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("run1 items = %d, want 2 (both students active at launch)", len(items))
	}

	// Withdraw b02 mid-flight: run1's items are untouched.
	setStudentWithdrawnByExt(t, env.st, "b02", true)
	items, err = env.st.Q.ListRunItems(t.Context(), db.ListRunItemsParams{RunID: run1ID, ItemLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("in-flight run1 items after withdrawal = %d, want 2 (unaffected)", len(items))
	}

	// New scopes exclude the withdrawn student: preview and a fresh run see 1 answer.
	preview = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment", env.ts.URL, aid), http.StatusOK)
	if int(preview["answers"].(float64)) != 1 {
		t.Fatalf("preview answers after withdrawal = %v, want 1", preview["answers"])
	}
	run2 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	run2ID := int64(run2["id"].(float64))
	if err := env.runner.Plan(t.Context(), run2ID); err != nil {
		t.Fatalf("plan run2: %v", err)
	}
	items, err = env.st.Q.ListRunItems(t.Context(), db.ListRunItemsParams{RunID: run2ID, ItemLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("run2 items = %d, want 1 (withdrawn b02 excluded from the new scope)", len(items))
	}
}

// provider_disabled is the one launch warning that is a guaranteed failure in
// production: it fires (danger) when the chosen method's latest version names a
// provider row that is missing or disabled, and a DISABLED provider also hard-
// blocks handleCreateRun with a 409 (mirroring the mask-gate precedent). A
// missing row only warns — the provider source is injectable (tests/dev run
// methods against llm.StaticSource with no llm_providers row at all).
func TestRunPreview_ProviderDisabledWarnsAndBlocksCreate(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c) // provider "fake" — no llm_providers row yet

	query := fmt.Sprintf("assessment_id=%d&scope_kind=assessment&method_id=%d", aid, methodID)
	ws := runPreviewWarnings(t, c, env.ts.URL, query)
	wantWarning(t, ws, "provider_disabled", "danger", 0) // missing row → warn, count-less

	// A method the frontend could never legitimately send is a 400, not a silent skip.
	resp, err := c.Get(fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=assessment&method_id=999999", env.ts.URL, aid))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("preview with unknown method: got %d want 400", resp.StatusCode)
	}

	// An enabled row clears the warning and the launch goes through.
	providerID := createFakeProviderRow(t, env, c, "fake")
	ws = runPreviewWarnings(t, c, env.ts.URL, query)
	refuteWarning(t, ws, "provider_disabled")
	postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)

	// Disabling the provider flips the warning back on AND blocks the launch.
	setProviderEnabled(t, c, env.ts.URL, providerID, false)
	ws = runPreviewWarnings(t, c, env.ts.URL, query)
	wantWarning(t, ws, "provider_disabled", "danger", 0)

	resp = postJSON(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create run on disabled provider: got %d want 409", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "method's provider is disabled" {
		t.Errorf("409 error = %q, want \"method's provider is disabled\"", body.Error)
	}

	// Re-enabling unblocks: same request now launches.
	setProviderEnabled(t, c, env.ts.URL, providerID, true)
	postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
}

// countLeafJobsForItem counts committed run.leaf jobs for one item id — the
// observable half of the "items reset ⇒ jobs exist" invariant.
func countLeafJobsForItem(t *testing.T, env *testEnv, itemID int64) int64 {
	t.Helper()
	var n int64
	if err := env.st.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM river_job WHERE kind = 'run.leaf' AND (args->>'item_id')::bigint = $1", itemID,
	).Scan(&n); err != nil {
		t.Fatalf("count leaf jobs: %v", err)
	}
	return n
}

// TestRetryFailed_AtomicResetStatusEnqueue (adversarial audit 2026-07-11):
// retry-failed's reset + status flip + leaf enqueue must be ONE transaction.
// Before the fix they were three autocommit steps, so a failure after
// ResetFailedItems left pending items with no jobs — the run wedged 'running'
// forever and a SECOND retry 400'd ("run has no failed items"), making the
// wedge permanent. A mid-sequence failure (simulated by a trigger refusing the
// status flip) must roll everything back so retrying again still works.
func TestRetryFailed_AtomicResetStatusEnqueue(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Scope to one answer so scripting the fake is deterministic.
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))

	env.fakeProv.Script = []fake.Step{{Err: errors.New("provider exploded")}}
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": answerID, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true) // finalAttempt → terminal failed item, run completed-with-failure

	items, err := env.st.Q.ListRunItems(context.Background(), db.ListRunItemsParams{RunID: runID, ItemLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].State != "failed" {
		t.Fatalf("setup: want exactly 1 failed item, got %+v", items)
	}
	itemID := items[0].ID
	jobsBefore := countLeafJobsForItem(t, env, itemID)

	// Sabotage the middle step: refuse any flip to 'running'. Whatever order the
	// handler's steps run in, one of them fails mid-sequence.
	mustExec(t, env.st, `CREATE FUNCTION test_refuse_running() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'test: refuse running'; END $$ LANGUAGE plpgsql`)
	mustExec(t, env.st, `CREATE TRIGGER test_refuse_running BEFORE UPDATE ON grading_runs FOR EACH ROW WHEN (NEW.status = 'running' AND OLD.status <> 'running') EXECUTE FUNCTION test_refuse_running()`)

	resp := postJSON(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("sabotaged retry: got %d want 500", resp.StatusCode)
	}

	// Everything must have rolled back: item still failed (retryable), run
	// status untouched, no half-committed jobs.
	item, err := env.st.Q.GetRunItem(context.Background(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "failed" {
		t.Errorf("item state after failed retry = %q, want failed (reset must roll back)", item.State)
	}
	runRow, err := env.st.Q.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if runRow.Status != "completed" {
		t.Errorf("run status after failed retry = %q, want completed (unchanged)", runRow.Status)
	}
	if n := countLeafJobsForItem(t, env, itemID); n != jobsBefore {
		t.Errorf("leaf jobs after failed retry = %d, want %d (no orphan/partial inserts)", n, jobsBefore)
	}

	mustExec(t, env.st, `DROP TRIGGER test_refuse_running ON grading_runs`)
	mustExec(t, env.st, `DROP FUNCTION test_refuse_running()`)

	// The wedge check: a second retry must still find the failed item and work.
	retried := postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil, http.StatusOK)
	if retried["retried"].(float64) != 1 {
		t.Fatalf("second retry: %v", retried)
	}
	if n := countLeafJobsForItem(t, env, itemID); n != jobsBefore+1 {
		t.Errorf("leaf jobs after successful retry = %d, want %d (job committed with the reset)", n, jobsBefore+1)
	}

	// Fake script is exhausted → the retried leaf succeeds and the run completes.
	driveRun(t, env, runID, false)
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	r := got["run"].(map[string]any)
	if r["status"] != "completed" || r["counts"].(map[string]any)["succeeded"].(float64) != 1 {
		t.Fatalf("after retry: %v", r)
	}

	// And a retry with nothing failed stays a clean 400.
	resp = postJSON(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("retry with no failed items: got %d want 400", resp.StatusCode)
	}
}

func TestRetryFailed_RejectsPinnedFinalRunWithoutResettingItems(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Assessment-scoped (A4 requires that to be pinnable) with one leaf
	// succeeding (default fake behavior) and one failing terminally: the run
	// has a real succeeded leaf (A3 requires that too) AND a failed one to
	// exercise retry-failed against.
	env.fakeProv.Script = []fake.Step{{}, {Err: errors.New("provider exploded")}}
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true)
	items, err := env.st.Q.ListRunItems(t.Context(), db.ListRunItemsParams{RunID: runID, ItemLimit: 10})
	if err != nil || len(items) != 2 {
		t.Fatalf("setup items: items=%+v err=%v", items, err)
	}
	var failedID int64
	for _, it := range items {
		if it.State == "failed" {
			failedID = it.ID
		}
	}
	if failedID == 0 {
		t.Fatalf("setup: no failed item among %+v", items)
	}
	jobsBefore := countLeafJobsForItem(t, env, failedID)

	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	resp := postJSON(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retry pinned final run: got %d want 409", resp.StatusCode)
	}
	item, err := env.st.Q.GetRunItem(t.Context(), failedID)
	if err != nil || item.State != "failed" {
		t.Fatalf("rejected retry changed item: item=%+v err=%v", item, err)
	}
	if n := countLeafJobsForItem(t, env, failedID); n != jobsBefore {
		t.Fatalf("rejected retry enqueued a job: got %d want %d", n, jobsBefore)
	}

	// The operator flow is explicit: unselect, then retry the formerly pinned run.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": nil}, http.StatusOK)
	postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil, http.StatusOK)
}

// TestCreateRun_RefSolutionRequiredBeforeLaunch (adversarial audit 2026-07-11):
// a method that includes a reference solution used to pass POST /api/runs and
// only fail at plan time — the TA discovered the requirement via a dead run
// row. The same per-problem check must now 400 the launch BEFORE any run row
// exists (same message as Runner.Plan), and the preview must warn so the
// dialog can say so before submit.
func TestCreateRun_RefSolutionRequiredBeforeLaunch(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)

	// Method on the fake provider that requires a reference solution.
	if err := grading.EnsureSeeds(context.Background(), env.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Refsol method",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 1, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	methodID := int64(m["id"].(float64))

	wantMsg := fmt.Sprintf("method includes a reference solution but problem %d has none", pid)

	// Preview warns before submit.
	query := fmt.Sprintf("assessment_id=%d&scope_kind=assessment&method_id=%d", aid, methodID)
	ws := runPreviewWarnings(t, c, env.ts.URL, query)
	w := wantWarning(t, ws, "missing_reference_solutions", "danger", 0)
	if w["detail"] != wantMsg {
		t.Errorf("warning detail = %v, want %q", w["detail"], wantMsg)
	}

	// The launch is refused with Plan's exact message, and NO run row exists.
	resp := postJSON(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create run without solution: got %d want 400", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != wantMsg {
		t.Errorf("400 error = %q, want %q", body.Error, wantMsg)
	}
	var runCount int64
	if err := env.st.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM grading_runs WHERE assessment_id = $1", aid,
	).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("refused launch must create no run row, found %d", runCount)
	}

	// Adding a solution clears the warning and the same launch goes through.
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/solutions", env.ts.URL, pid),
		map[string]any{"content": "model answer"}, http.StatusCreated)
	ws = runPreviewWarnings(t, c, env.ts.URL, query)
	refuteWarning(t, ws, "missing_reference_solutions")
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, int64(run["id"].(float64))), http.StatusOK)
	if got["run"].(map[string]any)["status"] != "completed" {
		t.Fatalf("run after solution added: %v", got["run"])
	}
}

// runPreviewBlockers fetches GET /api/runs/preview and returns its blockers
// array (the launch-dialog counterpart of runPreviewWarnings) — a []any of
// {code, problem_id?, message} objects.
func runPreviewBlockers(t *testing.T, c *http.Client, baseURL, query string) []any {
	t.Helper()
	got := getJSON[map[string]any](t, c, baseURL+"/api/runs/preview?"+query, http.StatusOK)
	raw, ok := got["blockers"].([]any)
	if !ok {
		t.Fatalf("run preview must carry a blockers array: %v", got)
	}
	return raw
}

// TestRunPreview_BlockersForMissingReferenceSolutions is B9-backend: the
// planner's per-problem "method includes a reference solution but problem N
// has none" check (Runner.Plan, also enforced at launch by handleCreateRun)
// must ALSO run at estimate time, machine-readably, so the launch dialog can
// disable Launch before spending a request on a run guaranteed to fail
// instantly (audit finding B9 — runs died this way with no pre-flight
// signal). A clean assessment (no method chosen, or a method needing no
// reference solution) reports an empty blockers array.
func TestRunPreview_BlockersForMissingReferenceSolutions(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)

	// No method selected yet: nothing to validate, blockers empty.
	bareQuery := fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid)
	if bs := runPreviewBlockers(t, c, env.ts.URL, bareQuery); len(bs) != 0 {
		t.Fatalf("blockers with no method chosen: got %v want empty", bs)
	}

	// A method that does NOT require a reference solution: still empty.
	plainMethodID := createFakeMethod(t, env, c)
	plainQuery := fmt.Sprintf("assessment_id=%d&scope_kind=assessment&method_id=%d", aid, plainMethodID)
	if bs := runPreviewBlockers(t, c, env.ts.URL, plainQuery); len(bs) != 0 {
		t.Fatalf("blockers for a method with no reference-solution requirement: got %v want empty", bs)
	}

	// A method that DOES require one, against a problem with none: blocker present.
	if err := grading.EnsureSeeds(context.Background(), env.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Refsol method (preview)",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 1, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	refsolMethodID := int64(m["id"].(float64))
	wantMsg := fmt.Sprintf("method includes a reference solution but problem %d has none", pid)

	refsolQuery := fmt.Sprintf("assessment_id=%d&scope_kind=assessment&method_id=%d", aid, refsolMethodID)
	bs := runPreviewBlockers(t, c, env.ts.URL, refsolQuery)
	if len(bs) != 1 {
		t.Fatalf("blockers for missing reference solution: got %v want exactly 1", bs)
	}
	b := bs[0].(map[string]any)
	if b["code"] != "missing_reference_solutions" {
		t.Errorf("blocker code = %v, want missing_reference_solutions", b["code"])
	}
	if int64(b["problem_id"].(float64)) != pid {
		t.Errorf("blocker problem_id = %v, want %d", b["problem_id"], pid)
	}
	if b["message"] != wantMsg {
		t.Errorf("blocker message = %v, want %q", b["message"], wantMsg)
	}

	// Adding the solution clears the blocker — a clean assessment again.
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/solutions", env.ts.URL, pid),
		map[string]any{"content": "model answer"}, http.StatusCreated)
	if bs := runPreviewBlockers(t, c, env.ts.URL, refsolQuery); len(bs) != 0 {
		t.Fatalf("blockers after adding the solution: got %v want empty", bs)
	}
}

// TestRunPreview_BlockersForMissingRubric is B9-backend's second guaranteed-
// failure class: Runner.Plan unconditionally hard-fails a run whose scope
// touches a problem with no rubric ("problem N has no rubric — grading needs
// one") — the exact reaches-the-queue-dies-instantly pattern blockers exist to
// pre-empt. The estimate must report it per problem in blockers[], following
// Plan's semantics (only problems with in-scope GRADABLE answers block — a
// rubric-less problem nothing would grade doesn't fail a launch, though the
// count-based no_rubric_problems warning still mentions it). No method_id is
// needed: rubrics are required regardless of the method.
func TestRunPreview_BlockersForMissingRubric(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	query := fmt.Sprintf("assessment_id=%d&scope_kind=assessment", aid)

	// Baseline: the fixture's one problem has a rubric — blockers empty.
	if bs := runPreviewBlockers(t, c, env.ts.URL, query); len(bs) != 0 {
		t.Fatalf("baseline blockers: got %v want empty", bs)
	}

	// A rubric-less problem with NO in-scope answers: Plan would never touch
	// it, so it must warn (count) but not block — pin the distinction.
	p2 := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", env.ts.URL, aid),
		map[string]any{"number": 2, "title": "Greedy", "max_points": "10"}, http.StatusCreated)
	p2ID := int64(p2["id"].(float64))
	if bs := runPreviewBlockers(t, c, env.ts.URL, query); len(bs) != 0 {
		t.Fatalf("rubric-less problem with no answers must not block: got %v", bs)
	}
	ws := runPreviewWarnings(t, c, env.ts.URL, query)
	wantWarning(t, ws, "no_rubric_problems", "warning", 1)

	// Give problem 2 an in-scope gradable answer (the mapping-correction move,
	// same as a TA fixing a page landed on the wrong problem).
	pages, err := env.st.Q.ListPagesForAssessment(context.Background(), aid)
	if err != nil || len(pages) == 0 {
		t.Fatalf("ListPagesForAssessment: %v (%d pages)", err, len(pages))
	}
	postExpect(t, c, fmt.Sprintf("%s/api/answer-pages/%d/move", env.ts.URL, pages[0].ID),
		map[string]any{"problem_id": p2ID}, http.StatusNoContent)

	bs := runPreviewBlockers(t, c, env.ts.URL, query)
	if len(bs) != 1 {
		t.Fatalf("blockers with a gradable answer on a rubric-less problem: got %v want exactly 1", bs)
	}
	b := bs[0].(map[string]any)
	if b["code"] != "no_rubric_problems" {
		t.Errorf("blocker code = %v, want no_rubric_problems", b["code"])
	}
	if int64(b["problem_id"].(float64)) != p2ID {
		t.Errorf("blocker problem_id = %v, want %d", b["problem_id"], p2ID)
	}
	wantMsg := fmt.Sprintf("problem %d has no rubric — grading needs one", p2ID)
	if b["message"] != wantMsg {
		t.Errorf("blocker message = %v, want %q", b["message"], wantMsg)
	}

	// Adding the rubric clears the blocker.
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, p2ID), map[string]any{
		"score_increment": "0.5",
		"criteria":        []map[string]string{{"description": "Exchange argument", "points": "10"}},
	}, http.StatusCreated)
	if bs := runPreviewBlockers(t, c, env.ts.URL, query); len(bs) != 0 {
		t.Fatalf("blockers after adding the rubric: got %v want empty", bs)
	}
	_ = pid
}

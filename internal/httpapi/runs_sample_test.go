package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// A sample-scope run (calibration, spec 2026-07-20) launches like any other
// scope: scope_id carries N, the preview's unit count is min(N, pool), and the
// planned run grades exactly that many answers.
func TestCreateRun_SampleScope(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t) // 1 problem, 2 uploaded students → pool of 2
	acceptAllMasks(t, env, c, aid)
	methodID := createFakeMethod(t, env, c)

	// Preview: N=1 estimates 1 unit; N=50 clamps to the pool of 2.
	pv := getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=sample&scope_id=1&method_id=%d", env.ts.URL, aid, methodID),
		http.StatusOK)
	if got := int(pv["answers"].(float64)); got != 1 {
		t.Fatalf("preview answers for N=1: got %d, want 1", got)
	}
	pv = getJSON[map[string]any](t, c,
		fmt.Sprintf("%s/api/runs/preview?assessment_id=%d&scope_kind=sample&scope_id=50&method_id=%d", env.ts.URL, aid, methodID),
		http.StatusOK)
	if got := int(pv["answers"].(float64)); got != 2 {
		t.Fatalf("preview answers for N=50: got %d, want 2 (pool)", got)
	}

	res := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "sample", "scope_id": 1, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(res["id"].(float64))

	driveRun(t, env, runID, true)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d?all=1", env.ts.URL, runID), http.StatusOK)
	run := got["run"].(map[string]any)
	if run["status"] != "completed" {
		t.Fatalf("run status = %v, want completed (%v)", run["status"], run["error"])
	}
	if run["scope_kind"] != "sample" {
		t.Fatalf("scope_kind = %v, want sample", run["scope_kind"])
	}
	items := got["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("sampled run has %d items, want 1", len(items))
	}
}

// A sample scope gates over the WHOLE assessment (the draw may touch any
// problem): a rubric-less problem elsewhere in the assessment must surface in
// the sample preview's warnings exactly as it does for an assessment scope.
func TestRunPreview_SampleScope_WarnsOnAssessmentWideRubricGaps(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)
	methodID := createFakeMethod(t, env, c)

	// A second problem with no rubric (and no uploads — the warning must still
	// fire, since it derives from the assessment's problems, not from answers).
	postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", env.ts.URL, aid),
		map[string]any{"number": 2, "title": "No rubric yet", "max_points": "5"}, http.StatusCreated)

	ws := runPreviewWarnings(t, c, env.ts.URL,
		fmt.Sprintf("assessment_id=%d&scope_kind=sample&scope_id=2&method_id=%d", aid, methodID))
	w, ok := ws["no_rubric_problems"]
	if !ok {
		t.Fatalf("sample preview must warn about the assessment's rubric-less problem: %v", ws)
	}
	if int(w["count"].(float64)) != 1 {
		t.Fatalf("no_rubric_problems count = %v, want 1", w["count"])
	}
}

// N < 1 is rejected at launch with an actionable 400, before any run row exists.
func TestCreateRun_SampleScope_RejectsBadN(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	acceptAllMasks(t, env, c, aid)
	methodID := createFakeMethod(t, env, c)

	resp := postJSON(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "sample", "scope_id": 0, "method_id": methodID,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("N=0 launch: got %d, want 400", resp.StatusCode)
	}
}

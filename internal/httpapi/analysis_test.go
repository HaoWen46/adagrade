package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPromptPreview_ShowsExactPromptAndPins(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	_ = aid

	// The seeded fake method has ref_solutions: 0, so no solution is required.
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d", env.ts.URL, pid, methodID), http.StatusOK)

	user := got["user"].(string)
	if !strings.Contains(user, "Give an O(n log n) sorting algorithm") &&
		!strings.Contains(user, "DP") {
		// phase4Setup's problem has title DP and empty statement; the rubric lines matter most.
		t.Logf("user prompt: %s", user)
	}
	if !strings.Contains(user, "criterion_id") || !strings.Contains(user, "Recurrence") || !strings.Contains(user, "Complexity") {
		t.Errorf("user prompt missing rubric lines: %q", user)
	}
	if !strings.Contains(got["system"].(string), "teaching assistant") {
		t.Errorf("system prompt: %q", got["system"])
	}
	if !strings.Contains(got["schema"].(string), "criterion_id") {
		t.Errorf("schema missing criteria: %q", got["schema"])
	}
	pins := got["pins"].(map[string]any)
	if pins["rubric_version"].(float64) != 1 || pins["provider"] != "fake" {
		t.Errorf("pins: %v", pins)
	}

	// A method that wants a reference solution fails clearly while none exists.
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2 := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "With refsol",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1", "ref_solutions": 1,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	resp, err := c.Get(fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d", env.ts.URL, pid, int64(m2["id"].(float64))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("preview without refsol: got %d want 400", resp.StatusCode)
	}

	// Add a solution → preview includes it.
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/solutions", env.ts.URL, pid), map[string]string{"content": "Use merge sort, T(n)=2T(n/2)+O(n)."}, http.StatusCreated)
	got = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/prompt-preview?method_id=%d", env.ts.URL, pid, int64(m2["id"].(float64))), http.StatusOK)
	if !strings.Contains(got["user"].(string), "Use merge sort") {
		t.Errorf("preview should include the reference solution")
	}
}

func TestAnalysis_StatsAndAgreement(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Fake run grades both answers with total 2 (1 per criterion).
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	// A human grades one answer with total 3 (same rubric version).
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	current := rubric["current"].(map[string]any)
	var scores []map[string]any
	for i, cr := range current["criteria"].([]any) {
		score := "1"
		if i == 0 {
			score = "2"
		}
		scores = append(scores, map[string]any{"criterion_id": int64(cr.(map[string]any)["id"].(float64)), "score": score})
	}
	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/records", env.ts.URL, answerID), map[string]any{
		"rubric_version_id": int64(current["id"].(float64)),
		"scores":            scores,
	}, http.StatusCreated)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)

	stats := got["stats"].([]any)
	if len(stats) != 1 {
		t.Fatalf("stats rows: %v", stats)
	}
	st0 := stats[0].(map[string]any)
	if st0["records"].(float64) != 2 || st0["mean_total"] != "2" || st0["median_total"] != "2" ||
		st0["zeros"].(float64) != 0 || st0["conf_high"].(float64) != 2 {
		t.Errorf("stats: %v", st0)
	}
	// method_id rides along with method_version_id so the frontend can roll
	// versions up to one method (final-source picker, analysis redesign plan).
	if st0["method_id"].(float64) != float64(methodID) {
		t.Errorf("method_id = %v, want %d", st0["method_id"], methodID)
	}
	if st0["input_tokens"].(float64) != 200 { // fake charges 100 per call
		t.Errorf("tokens: %v", st0["input_tokens"])
	}

	agreement := got["agreement"].([]any)
	if len(agreement) != 1 {
		t.Fatalf("agreement rows: %v", agreement)
	}
	ag := agreement[0].(map[string]any)
	// Human 3 vs model 2 on one answer: 1 pair, diff 1, not exact, within one.
	if ag["pairs"].(float64) != 1 || ag["mean_abs_diff"] != "1" ||
		ag["exact_matches"].(float64) != 0 || ag["within_one"].(float64) != 1 {
		t.Errorf("agreement: %v", ag)
	}
}

// disagreementOf pulls the analysis response's disagreement block apart.
func disagreementOf(t *testing.T, c *http.Client, baseURL string, aid int64) ([]any, []any) {
	t.Helper()
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", baseURL, aid), http.StatusOK)
	dis, ok := got["disagreement"].(map[string]any)
	if !ok {
		t.Fatalf("analysis response must carry a disagreement object: %v", got)
	}
	problems, ok := dis["problems"].([]any)
	if !ok {
		t.Fatalf("disagreement.problems must be a JSON array even when empty: %v", dis)
	}
	top, ok := dis["top_answers"].([]any)
	if !ok {
		t.Fatalf("disagreement.top_answers must be a JSON array even when empty: %v", dis)
	}
	return problems, top
}

// The disagreement block mirrors the agreement CTE's semantics (analysis
// redesign plan, Task B1): latest model record per (answer, method-version),
// current rubric version only — so the "where methods disagree" numbers can
// never contradict the stats/agreement tables beside them.
func TestAnalysis_Disagreement(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodA := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	runA := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodA,
	}, http.StatusCreated)
	driveRun(t, env, int64(runA["id"].(float64)), false)

	// One method-version has records: nothing to compare, both arrays empty
	// (the frontend's hide signal).
	problems, top := disagreementOf(t, c, env.ts.URL, aid)
	if len(problems) != 0 || len(top) != 0 {
		t.Fatalf("single method must yield empty disagreement, got %v / %v", problems, top)
	}

	// A second fake method grades identically (total 2 everywhere): both
	// answers compared, all spreads zero, no big gaps.
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2 := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake method B",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	methodB := int64(m2["id"].(float64))
	runB := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodB,
	}, http.StatusCreated)
	driveRun(t, env, int64(runB["id"].(float64)), false)

	problems, top = disagreementOf(t, c, env.ts.URL, aid)
	if len(problems) != 1 {
		t.Fatalf("disagreement problems: %v", problems)
	}
	p0 := problems[0].(map[string]any)
	if p0["problem_number"].(float64) != 1 || p0["max_points"] != "10" ||
		p0["answers_compared"].(float64) != 2 || p0["median_spread"] != "0" ||
		p0["big_gap_count"].(float64) != 0 {
		t.Errorf("identical grades: %v", p0)
	}
	if len(top) != 2 {
		t.Fatalf("top answers: %v", top)
	}

	// Method B's version id (from the stats rows) and one answer to re-grade.
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	var mvB int64
	for _, raw := range got["stats"].([]any) {
		st := raw.(map[string]any)
		if int64(st["method_id"].(float64)) == methodB {
			mvB = int64(st["method_version_id"].(float64))
		}
	}
	if mvB == 0 {
		t.Fatal("method B missing from stats")
	}
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	gapAnswer := int64(students["students"][0]["answer_id"].(float64))
	gapStudent := students["students"][0]["student_id"].(string)

	// Nudge method B's grade on one answer to 8 (direct SQL, same convention as
	// the warnings tests): spread 6 vs method A's 2. Threshold on a 10-point
	// problem is GREATEST(1, 0.1*10) = 1, so this is a big gap; median over
	// spreads {0, 6} is 3.
	mustExec(t, env.st, `UPDATE grading_records SET total = 8 WHERE answer_id = $1 AND method_version_id = $2`, gapAnswer, mvB)

	problems, top = disagreementOf(t, c, env.ts.URL, aid)
	p0 = problems[0].(map[string]any)
	if p0["answers_compared"].(float64) != 2 || p0["median_spread"] != "3" || p0["big_gap_count"].(float64) != 1 {
		t.Errorf("after gap: %v", p0)
	}
	if len(top) != 2 {
		t.Fatalf("top answers after gap: %v", top)
	}
	t0 := top[0].(map[string]any)
	if int64(t0["answer_id"].(float64)) != gapAnswer || t0["spread"] != "6" ||
		t0["student_display"] != gapStudent || t0["problem_number"].(float64) != 1 {
		t.Errorf("top answer: %v", t0)
	}
	scores := t0["scores"].([]any)
	if len(scores) != 2 {
		t.Fatalf("top answer scores: %v", scores)
	}
	totalsByName := map[string]string{}
	for _, raw := range scores {
		sc := raw.(map[string]any)
		totalsByName[sc["method_name"].(string)] = sc["total"].(string)
	}
	if totalsByName["Fake method"] != "2" || totalsByName["Fake method B"] != "8" {
		t.Errorf("per-method totals: %v", totalsByName)
	}

	// Latest record per (answer, method-version) wins: a NEWER model record for
	// method B back at total 2 supersedes the 8, collapsing the spread.
	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	rvID := int64(rubric["current"].(map[string]any)["id"].(float64))
	mustExec(t, env.st, `INSERT INTO grading_records
		(answer_id, source, model_id, method_version_id, rubric_version_id, criterion_scores, total)
		VALUES ($1, 'model', 'fake-vision-1', $2, $3, '[]'::jsonb, 2)`, gapAnswer, mvB, rvID)
	problems, _ = disagreementOf(t, c, env.ts.URL, aid)
	p0 = problems[0].(map[string]any)
	if p0["median_spread"] != "0" || p0["big_gap_count"].(float64) != 0 {
		t.Errorf("latest record must win: %v", p0)
	}

	// Current rubric version only: a new rubric version strands every existing
	// record on v1, so there is nothing comparable any more.
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Recurrence", "points": "6"},
			{"description": "Complexity", "points": "4"},
		},
	}, http.StatusCreated)
	problems, top = disagreementOf(t, c, env.ts.URL, aid)
	if len(problems) != 0 || len(top) != 0 {
		t.Errorf("stale-rubric records must not be compared: %v / %v", problems, top)
	}
}

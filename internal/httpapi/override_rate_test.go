package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// TestAnalysis_OverrideRateByMethod hand-builds a fixture (trust spec §7, D40):
// one method grades 2 answers, both total=2 (fake provider, 1pt/criterion). A
// human then grades answer[0] with total=3 and that becomes its official;
// answer[1]'s official is its own AI record unchanged (no human touched it).
// Officials are derived since 0027, so the fixture pokes the pointers directly
// — the analysis query reads whatever they say. Expected:
//
//	answers=2, human_overrides=1, override_rate=0.5 (NumStr trims "0.5000")
//	mean_abs_diff = mean(|2-3|, |2-2|) = mean(1,0) = 0.5
func TestAnalysis_OverrideRateByMethod(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answer0 := int64(students["students"][0]["answer_id"].(float64))
	answer1 := int64(students["students"][1]["answer_id"].(float64))

	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	current := rubric["current"].(map[string]any)
	var scores []map[string]any
	for i, cr := range current["criteria"].([]any) {
		score := "1"
		if i == 0 {
			score = "2" // total 3: 2+1
		}
		scores = append(scores, map[string]any{"criterion_id": int64(cr.(map[string]any)["id"].(float64)), "score": score})
	}
	humanRec := postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/records", env.ts.URL, answer0), map[string]any{
		"rubric_version_id": int64(current["id"].(float64)),
		"scores":            scores,
	}, http.StatusCreated)
	humanRecID := int64(humanRec["id"].(float64))

	// answer1's official is the AI record itself — the AI record IS the human's
	// own review decision to accept it as-is, not "no official set" (that would say
	// nothing about override behavior, per the query's doc comment).
	detail1 := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answer1), http.StatusOK)
	aiRec1 := int64(detail1["records"].([]any)[0].(map[string]any)["id"].(float64))

	// Poke both officials AFTER the human-record POST above — that POST is the
	// last recompute trigger in this fixture (no final source is chosen, so a
	// recompute would clear poked pointers).
	mustExec(t, env.st, `UPDATE answers SET official_record_id = $1, official_set_at = now() WHERE id = $2`, humanRecID, answer0)
	mustExec(t, env.st, `UPDATE answers SET official_record_id = $1, official_set_at = now() WHERE id = $2`, aiRec1, answer1)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	rows, ok := got["override_rate"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("override_rate: %v", got["override_rate"])
	}
	row := rows[0].(map[string]any)
	if row["answers"].(float64) != 2 {
		t.Fatalf("answers: %v", row)
	}
	if row["human_overrides"].(float64) != 1 {
		t.Errorf("human_overrides: got %v want 1", row["human_overrides"])
	}
	if row["override_rate"] != "0.5" {
		t.Errorf("override_rate: got %v want 0.5", row["override_rate"])
	}
	if row["mean_abs_diff"] != "0.5" {
		t.Errorf("mean_abs_diff: got %v want 0.5", row["mean_abs_diff"])
	}
}

// TestAnalysis_OverrideRateByMethod_NoOfficialsExcludesAnswer pins the query's
// documented "only answers with BOTH a model record from this method AND an
// official record set are counted" rule: an assessment with AI grades but no
// official set anywhere yields an empty override_rate list, not a divide-by-zero
// or a misleading 0%.
func TestAnalysis_OverrideRateByMethod_NoOfficialsExcludesAnswer(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/analysis", env.ts.URL, aid), http.StatusOK)
	rows, ok := got["override_rate"].([]any)
	if !ok || len(rows) != 0 {
		t.Fatalf("override_rate: got %v want empty", got["override_rate"])
	}
}

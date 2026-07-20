package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// distFixtureAnswer inserts a bare answer (EnsureAnswer skips the upload/mask
// pipeline entirely — the distribution queries only join answers/grading_records,
// so a fixture doesn't need real pages) plus one grading_record with an exact
// total, and returns the answer id and record id.
func distFixtureAnswer(t *testing.T, env *testEnv, aid, pid, rvID int64, studentSuffix string, source string, criterionScores string, total string) (answerID, recordID int64) {
	t.Helper()
	sid := "dist-" + studentSuffix
	seedStudent(t, env.st, sid, "Student "+sid, sid+"@x.edu")
	student, err := env.st.Q.GetStudentByExternalID(t.Context(), sid)
	if err != nil {
		t.Fatalf("GetStudentByExternalID: %v", err)
	}
	ans, err := env.st.Q.EnsureAnswer(t.Context(), db.EnsureAnswerParams{AssessmentID: aid, StudentID: student.ID, ProblemID: pid})
	if err != nil {
		t.Fatalf("EnsureAnswer: %v", err)
	}
	createdBy := "NULL"
	if source == "human" {
		// grading_records has CHECK (source <> 'human' OR created_by IS NOT NULL); any
		// existing user id works, phase4Setup always seeds the lecturer as user 1.
		createdBy = "1"
	}
	var recID int64
	err = env.st.Pool.QueryRow(t.Context(), fmt.Sprintf(`
		INSERT INTO grading_records (answer_id, source, model_id, rubric_version_id, graded_image_shas, criterion_scores, total, comment, adjustments, created_by)
		VALUES ($1, $2, 'fake-vision-1', $3, '{}', $4, $5, '', '[]', %s)
		RETURNING id`, createdBy),
		ans.ID, source, rvID, criterionScores, total).Scan(&recID)
	if err != nil {
		t.Fatalf("insert grading_record: %v", err)
	}
	return ans.ID, recID
}

func setOfficial(t *testing.T, env *testEnv, answerID, recordID int64) {
	t.Helper()
	mustExec(t, env.st, `UPDATE answers SET official_record_id = $1 WHERE id = $2`, recordID, answerID)
}

// TestScoreDistribution_OfficialExactStats hand-builds 3 official grades on a
// problem with max_points=10 (rubric: Recurrence 6pts, Complexity 4pts, per
// phase4Setup) and checks the endpoint's mean/stddev/%zero/%max/histogram against
// values computed by hand.
//
// Totals: 0 (all-zero), 6, 10 (all-max).
//
//	mean   = (0+6+10)/3 = 16/3 = 5.3333
//	stddev (sample, N-1) = sqrt( ((0-16/3)^2+(6-16/3)^2+(10-16/3)^2) / 2 )
//	                     = sqrt( (28.4444+0.4444+25.0000...) / 2 ) ≈ 5.1316
//	zeros = 1/3 = 33.3%, maxes = 1/3 = 33.3%
//
// Criterion "Recurrence" (6pts) scores: 0, 4, 6 → mean=10/3=3.3333, zeros=1/3, maxes=1/3
// Criterion "Complexity" (4pts) scores: 0, 2, 4 → mean=2, zeros=1/3, maxes=1/3
// Histogram (10 buckets over [0,10]): 0 -> bucket 1, 6 -> bucket 7 ([60%,70%)), 10 -> bucket 10.
func TestScoreDistribution_OfficialExactStats(t *testing.T) {
	env, c, aid, pid, rvID := phase4Setup(t)

	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	current := rubric["current"].(map[string]any)
	criteria := current["criteria"].([]any)
	recurrenceID := int64(criteria[0].(map[string]any)["id"].(float64)) // 6 pts
	complexityID := int64(criteria[1].(map[string]any)["id"].(float64)) // 4 pts

	scoresJSON := func(rec, comp string) string {
		return fmt.Sprintf(`[{"criterion_id":%d,"score":"%s"},{"criterion_id":%d,"score":"%s"}]`, recurrenceID, rec, complexityID, comp)
	}

	a1, r1 := distFixtureAnswer(t, env, aid, pid, rvID, "1", "human", scoresJSON("0", "0"), "0")
	a2, r2 := distFixtureAnswer(t, env, aid, pid, rvID, "2", "human", scoresJSON("4", "2"), "6")
	a3, r3 := distFixtureAnswer(t, env, aid, pid, rvID, "3", "human", scoresJSON("6", "4"), "10")
	setOfficial(t, env, a1, r1)
	setOfficial(t, env, a2, r2)
	setOfficial(t, env, a3, r3)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/score-distribution", env.ts.URL, pid), http.StatusOK)

	if got["source"] != "official" {
		t.Fatalf("source: got %v want official", got["source"])
	}
	if got["max_points"] != "10" {
		t.Fatalf("max_points: got %v", got["max_points"])
	}

	total := got["total"].(map[string]any)
	if total["n"].(float64) != 3 {
		t.Fatalf("total.n: %v", total)
	}
	if total["mean"] != "5.3333" {
		t.Errorf("total.mean: got %v want 5.3333", total["mean"])
	}
	if total["stddev"] != "5.0332" {
		t.Errorf("total.stddev: got %v want 5.0332", total["stddev"])
	}
	if total["zeros"].(float64) != 1 || total["maxes"].(float64) != 1 {
		t.Errorf("total zeros/maxes: %v", total)
	}
	if total["zero_pct"] != "33.3" || total["max_pct"] != "33.3" {
		t.Errorf("total zero_pct/max_pct: %v", total)
	}

	hist, ok := got["histogram"].([]any)
	if !ok || len(hist) != 10 {
		t.Fatalf("histogram shape: %v", got["histogram"])
	}
	wantHist := [10]float64{1, 0, 0, 0, 0, 0, 1, 0, 0, 1}
	for i, v := range hist {
		if v.(float64) != wantHist[i] {
			t.Errorf("histogram[%d]: got %v want %v (full: %v)", i, v, wantHist[i], hist)
		}
	}

	crit := got["criteria"].([]any)
	if len(crit) != 2 {
		t.Fatalf("criteria: %v", crit)
	}
	c0 := crit[0].(map[string]any)
	if c0["description"] != "Recurrence" || c0["points"] != "6" {
		t.Fatalf("criteria[0]: %v", c0)
	}
	// Recurrence scores 0,4,6: mean=10/3=3.3333, stddev_samp≈3.0551.
	if c0["mean"] != "3.3333" || c0["stddev"] != "3.0551" || c0["zeros"].(float64) != 1 || c0["maxes"].(float64) != 1 {
		t.Errorf("criteria[0] stats: %v", c0)
	}
	c1 := crit[1].(map[string]any)
	if c1["description"] != "Complexity" || c1["points"] != "4" {
		t.Fatalf("criteria[1]: %v", c1)
	}
	// Complexity scores 0,2,4: mean=2, stddev_samp=2 exactly — NumStr trims
	// trailing fractional zeros ("2.0000" -> "2").
	if c1["mean"] != "2" || c1["stddev"] != "2" || c1["zeros"].(float64) != 1 || c1["maxes"].(float64) != 1 {
		t.Errorf("criteria[1] stats: %v", c1)
	}
}

// TestScoreDistribution_SparseOfficialsFallsBackToLatestRunAI pins the D38
// fallback: when a problem has zero official grades, the endpoint falls back to
// the latest run's AI records and labels source "ai_fallback".
func TestScoreDistribution_SparseOfficialsFallsBackToLatestRunAI(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	driveRun(t, env, int64(run["id"].(float64)), false)

	// No official grades set anywhere on this problem yet.
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/score-distribution", env.ts.URL, pid), http.StatusOK)
	if got["source"] != "ai_fallback" {
		t.Fatalf("source: got %v want ai_fallback", got["source"])
	}
	total := got["total"].(map[string]any)
	// The fake provider grades every criterion 1 point (see TestAnalysis_StatsAndAgreement);
	// phase4Setup has 2 students both AI-graded total=2.
	if total["n"].(float64) != 2 {
		t.Fatalf("total.n: %v", total)
	}
	if total["mean"] != "2" {
		t.Errorf("total.mean: got %v want 2", total["mean"])
	}

	// Once ANY official is set, the endpoint switches back to "official" even
	// though it's just one record (v0 sparse threshold: >0 officials is enough).
	// Officials are derived since 0027; poke the pointer directly (fixture-only).
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	recID := int64(detail["records"].([]any)[0].(map[string]any)["id"].(float64))
	setOfficial(t, env, answerID, recID)

	got2 := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/score-distribution", env.ts.URL, pid), http.StatusOK)
	if got2["source"] != "official" {
		t.Fatalf("source after setting one official: got %v want official", got2["source"])
	}
	total2 := got2["total"].(map[string]any)
	if total2["n"].(float64) != 1 {
		t.Fatalf("total.n after one official: %v", total2)
	}
}

// TestScoreDistribution_NoDataIsEmptyNotCrash pins the brief's constraint: zero
// data ⇒ empty histogram + null stats, not a crash or fake zeros.
func TestScoreDistribution_NoDataIsEmptyNotCrash(t *testing.T) {
	env, c, _, pid, _ := phase4Setup(t)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/score-distribution", env.ts.URL, pid), http.StatusOK)
	if got["source"] != "none" {
		t.Fatalf("source: got %v want none", got["source"])
	}
	if got["total"] != nil {
		t.Errorf("total: got %v want null", got["total"])
	}
	hist, ok := got["histogram"].([]any)
	if !ok || len(hist) != 10 {
		t.Fatalf("histogram: %v", got["histogram"])
	}
	for i, v := range hist {
		if v.(float64) != 0 {
			t.Errorf("histogram[%d]: got %v want 0", i, v)
		}
	}
	crit, ok := got["criteria"].([]any)
	if !ok || len(crit) != 0 {
		t.Errorf("criteria: got %v want empty", got["criteria"])
	}
}

// TestScoreDistribution_UnknownProblem404 is a smoke test for the id-not-found path.
func TestScoreDistribution_UnknownProblem404(t *testing.T) {
	env, c, _, _, _ := phase4Setup(t)
	getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/999999/score-distribution", env.ts.URL), http.StatusNotFound)
}

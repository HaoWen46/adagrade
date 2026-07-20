package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/llm/fake"
)

// officialCount reads the problem-students drilldown and returns how many rows
// carry an official grade plus a per-student map of official totals (external
// student id -> total string, absent when no official).
func officialTotals(t *testing.T, c *http.Client, baseURL string, pid int64) map[string]string {
	t.Helper()
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", baseURL, pid), http.StatusOK)
	out := map[string]string{}
	for _, raw := range got["students"].([]any) {
		row := raw.(map[string]any)
		if total, ok := row["official_total"].(string); ok {
			out[row["student_id"].(string)] = total
		}
	}
	return out
}

// TestFinalSource_DerivesOfficials drives the 0027 round-0 lifecycle end to
// end: nothing is official until the exam chooses its source; choosing a
// method derives officials from that method's records; flags open holes that
// human fallbacks fill; removing the flag makes the source win again (the
// fallback is IGNORED once the source decides); switching to consensus with no
// aggregates falls back to human records only; a new rubric version reopens
// every hole.
func TestFinalSource_DerivesOfficials(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// A completed run alone must set nothing — no source is chosen yet.
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true)
	if got := officialTotals(t, c, env.ts.URL, pid); len(got) != 0 {
		t.Fatalf("no source chosen: expected zero officials, got %v", got)
	}

	// Choosing the method derives an official for every graded answer.
	res := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	if moved := int(res["officials_moved"].(float64)); moved != 2 {
		t.Fatalf("choosing the method should derive 2 officials, moved %d", moved)
	}
	totals := officialTotals(t, c, env.ts.URL, pid)
	if len(totals) != 2 {
		t.Fatalf("expected officials for both students, got %v", totals)
	}

	// A flag blocks the AI source: the answer becomes a hole again.
	answers := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	row0 := answers["students"].([]any)[0].(map[string]any)
	answerID := int64(row0["answer_id"].(float64))
	student0 := row0["student_id"].(string)
	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/flags", env.ts.URL, answerID),
		map[string]any{"flag": "needs_review", "add": true}, http.StatusNoContent)
	if totals := officialTotals(t, c, env.ts.URL, pid); len(totals) != 1 {
		t.Fatalf("flag should open a hole (1 official left), got %v", totals)
	}

	// A human record fills the hole — the fallback path. Scored 0.5/criterion
	// (total 1) so it is distinguishable from the method's records (the fake
	// provider scores 1/criterion, total 2).
	rubric := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), http.StatusOK)
	current := rubric["current"].(map[string]any)
	rubricID := int64(current["id"].(float64))
	var scores []map[string]any
	for _, cr := range current["criteria"].([]any) {
		scores = append(scores, map[string]any{
			"criterion_id": int64(cr.(map[string]any)["id"].(float64)), "score": "0.5",
		})
	}
	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/records", env.ts.URL, answerID), map[string]any{
		"rubric_version_id": rubricID, "comment": "fallback", "scores": scores,
	}, http.StatusCreated)
	totals = officialTotals(t, c, env.ts.URL, pid)
	if totals[student0] != "1" {
		t.Fatalf("human fallback should be official (total 1) for %s, got %v", student0, totals)
	}

	// Removing the flag lets the source decide again — the human record is now
	// ignored ("it can never be official on its own").
	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/flags", env.ts.URL, answerID),
		map[string]any{"flag": "needs_review", "add": false}, http.StatusNoContent)
	totals = officialTotals(t, c, env.ts.URL, pid)
	if totals[student0] != "2" {
		t.Fatalf("unflagged answer should re-derive from the method (total 2, not the human 1), got %v", totals)
	}

	// Switching to consensus (no aggregates exist): only the human fallback
	// survives; the other student becomes a hole.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	totals = officialTotals(t, c, env.ts.URL, pid)
	if len(totals) != 1 || totals[student0] != "1" {
		t.Fatalf("consensus with no aggregates: only the human fallback should hold, got %v", totals)
	}

	// A new rubric version reopens every hole (freshness rule).
	postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", env.ts.URL, pid), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Recurrence", "points": "6"},
			{"description": "Complexity", "points": "4"},
		},
	}, http.StatusCreated)
	if totals := officialTotals(t, c, env.ts.URL, pid); len(totals) != 0 {
		t.Fatalf("new rubric version should reopen all holes, got %v", totals)
	}

	// Un-choosing the source clears unpublished officials entirely.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": nil}, http.StatusOK)
	if totals := officialTotals(t, c, env.ts.URL, pid); len(totals) != 0 {
		t.Fatalf("unset source should leave zero officials, got %v", totals)
	}

	// Validation: kind=method requires a real completed run.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method"}, http.StatusBadRequest)
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "bogus"}, http.StatusBadRequest)
}

// TestPublish_RequiresFinalSourceAndSpotCheck: publishing is gated on (a) a
// chosen source and (b), for method sources, the selected completed run's
// spot-check sample (trust spec §4, relocated in 0027 from accept-official).
func TestPublish_RequiresFinalSourceAndSpotCheck(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true)

	// No source chosen: preview says not publishable + publish 409s.
	pv := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", env.ts.URL, aid), http.StatusOK)
	if pv["publishable"] == true {
		t.Fatalf("no source chosen should not be publishable")
	}
	if pv["final_source"] != nil {
		t.Fatalf("final_source should be null before choosing, got %v", pv["final_source"])
	}
	resp := postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/publish", env.ts.URL, aid), map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("publish without a source: got %d want 409", resp.StatusCode)
	}

	// Choose the method: coverage is satisfied (derived officials), but the
	// spot-check gate is still closed — preview shows it, publish 409s.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	pv = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", env.ts.URL, aid), http.StatusOK)
	fs := pv["final_source"].(map[string]any)
	if fs["kind"] != "method" || fs["spot_check_open"] == true {
		t.Fatalf("expected method source with closed spot-check gate, got %v", fs)
	}
	if pv["publishable"] == true {
		t.Fatalf("closed spot-check gate must block publishable")
	}
	resp = postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/publish", env.ts.URL, aid), map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("publish with closed gate: got %d want 409", resp.StatusCode)
	}

	// Clear the sample; publish goes through.
	spotCheckAgreeAll(t, c, env.ts.URL, runID)
	pv = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", env.ts.URL, aid), http.StatusOK)
	if pv["publishable"] != true {
		t.Fatalf("gate cleared: expected publishable, got %v (final_source %v)", pv["publishable"], pv["final_source"])
	}
	postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/publish", env.ts.URL, aid), map[string]any{}, http.StatusCreated)

	// A live batch freezes the grading state that its snapshots represent. The
	// source must not be changed underneath that batch: doing so used to leave
	// published answers pointing at stale officials, then expose those stale
	// records again after unpublish/re-publish. The operator must unpublish first.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusConflict)
	pv = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", env.ts.URL, aid), http.StatusOK)
	fs = pv["final_source"].(map[string]any)
	if fs["kind"] != "method" || int64(fs["method_id"].(float64)) != methodID {
		t.Fatalf("rejected source change must preserve the published source, got %v", fs)
	}
	_ = pid
}

func TestFinalSource_LivePublishBatchLocksSource(t *testing.T) {
	env := harnessEnv(t)
	c := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")
	a := postExpect(t, c, env.ts.URL+"/api/assessments",
		map[string]string{"kind": "exam", "name": "Published source lock"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	// A non-superseded batch is the publication lock. An item is not needed to
	// exercise that invariant: even an empty/changed-only live batch represents a
	// snapshot whose grading source may not move underneath it.
	mustExec(t, env.st, `INSERT INTO publish_batches (assessment_id) VALUES ($1)`, aid)
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": nil}, http.StatusConflict)

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d", env.ts.URL, aid), http.StatusOK)
	assessment := got["assessment"].(map[string]any)
	if assessment["final_source_kind"] != "consensus" {
		t.Fatalf("rejected source change must leave consensus selected, got %v", assessment)
	}
}

func TestFinalSource_MethodRequiresCompletedRunFromAssessment(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))

	// A pending execution is mutable and therefore cannot be final authority.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusConflict)
	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d", env.ts.URL, aid), http.StatusOK)
	if assessment := got["assessment"].(map[string]any); assessment["final_source_kind"] != nil {
		t.Fatalf("rejected pending run changed source: %v", assessment)
	}

	driveRun(t, env, runID, false)
	other := postExpect(t, c, env.ts.URL+"/api/assessments",
		map[string]string{"kind": "exam", "name": "Other assessment"}, http.StatusCreated)
	otherID := int64(other["id"].(float64))
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, otherID),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusBadRequest)

	selected := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	assessment := selected["assessment"].(map[string]any)
	if int64(assessment["final_run_id"].(float64)) != runID || int64(assessment["final_method_id"].(float64)) != methodID {
		t.Fatalf("server did not pin run and derive its method: %v", assessment)
	}
}

// TestFinalSource_RejectsZeroSucceededRun (audit A3): a completed run whose
// leaves are ALL failed never produces a spot-check sample, so pinning it
// wedges publish behind an unreachable "review spot-check" call to action.
// The API must refuse with 422 + a machine-readable code before the pin (and
// its RecomputeOfficials side effect) lands.
func TestFinalSource_RejectsZeroSucceededRun(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// phase4Setup seeds 2 students/answers; script both Grade calls to fail so
	// the run completes with zero succeeded leaves.
	env.fakeProv.Script = []fake.Step{{Err: errors.New("boom")}, {Err: errors.New("boom")}}
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, true) // finalAttempt → terminal failures, run completes

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	r := got["run"].(map[string]any)
	succeeded, _ := r["counts"].(map[string]any)["succeeded"].(float64) // absent key = 0 succeeded
	if r["status"] != "completed" || succeeded != 0 {
		t.Fatalf("setup: want a completed run with zero succeeded leaves, got %v", r)
	}

	resp := putJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("pin a zero-succeeded run: got %d want 422", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "final_run_no_succeeded" {
		t.Errorf("422 code = %q, want %q", body.Code, "final_run_no_succeeded")
	}
	wantMsg := fmt.Sprintf("run #%d graded nothing — pick a run that produced grades", runID)
	if body.Error != wantMsg {
		t.Errorf("422 error = %q, want %q", body.Error, wantMsg)
	}

	// The rejected pin must be a complete no-op.
	assessment := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d", env.ts.URL, aid), http.StatusOK)["assessment"].(map[string]any)
	if assessment["final_source_kind"] != nil {
		t.Fatalf("rejected pin changed final_source_kind: %v", assessment)
	}
}

// TestFinalSource_RejectsNonAssessmentScopeRun (audit A4-minimal):
// RecomputeOfficials joins strictly on run_id = final_run_id, so pinning a
// problem- or answer-scoped run as the final source would silently
// un-officialize every answer outside that scope. The API refuses any run
// whose scope isn't the whole assessment.
func TestFinalSource_RejectsNonAssessmentScopeRun(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": answerID, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false) // succeeds — a real, spot-checkable sample

	resp := putJSONRaw(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("pin an answer-scoped run: got %d want 422", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "final_run_scope_not_assessment" {
		t.Errorf("422 code = %q, want %q", body.Code, "final_run_scope_not_assessment")
	}

	assessment := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d", env.ts.URL, aid), http.StatusOK)["assessment"].(map[string]any)
	if assessment["final_source_kind"] != nil {
		t.Fatalf("rejected pin changed final_source_kind: %v", assessment)
	}
}

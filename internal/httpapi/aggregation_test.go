package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// putPolicy PUTs the aggregation policy, asserting the expected status.
func putPolicy(t *testing.T, env *testEnv, c *http.Client, aid int64, body map[string]any, want int) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/assessments/%d/aggregation", env.ts.URL, aid), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		var eb bytes.Buffer
		_, _ = eb.ReadFrom(resp.Body)
		t.Fatalf("put policy: got %d want %d — %s", resp.StatusCode, want, eb.String())
	}
}

// TestAggregation_ConsensusOverTwoModels drives the whole D17 loop: two methods on
// providers that score 1 vs 2 per criterion, majority (disagree → flag, so the
// consensus source derives no official), then mean (agree within tolerance →
// flags cleared, the fresh aggregates derive official — 0027).
func TestAggregation_ConsensusOverTwoModels(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	m1 := createFakeMethod(t, env, c) // provider "fake", scores 1 per criterion
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2raw := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake method 2",
		"config": map[string]any{
			"provider": "fake2", "model": "fake-vision-2",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	m2 := int64(m2raw["id"].(float64))
	acceptAllMasks(t, env, c, aid)

	// Two runs, one per method (real records via the run pipeline). Spot-check
	// verdicts are irrelevant here: consensus officials are DERIVED (0027) and
	// the relocated spot-check gate only guards publishing method-sourced exams.
	var mvIDs []int64
	for _, mid := range []int64{m1, m2} {
		run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
			"assessment_id": aid, "scope_kind": "assessment", "method_id": mid,
		}, http.StatusCreated)
		driveRun(t, env, int64(run["id"].(float64)), false)
		mvIDs = append(mvIDs, int64(run["method_version_id"].(float64)))
	}

	// Round-based grading (0027): officials only ever derive from the exam's
	// chosen source. This exam grades by consensus.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	// Policy validation: f too large for n=2 rejected.
	putPolicy(t, env, c, aid, map[string]any{
		"method_version_ids": mvIDs, "combiner": "majority", "fault_tolerance": 1,
		"flag_triggers": []string{"agg_disagreement", "agg_missing"},
	}, http.StatusBadRequest)

	// Majority, f=0: models score 1 vs 2 → every criterion contested → flagged,
	// aggregate written with mean fallback (1.5); flagged answers are holes for
	// the derivation (no human fallback exists yet), so no officials move.
	putPolicy(t, env, c, aid, map[string]any{
		"method_version_ids": mvIDs, "combiner": "majority", "fault_tolerance": 0,
		"flag_triggers": []string{"agg_disagreement", "agg_missing"},
	}, http.StatusOK)
	rep := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/aggregate", env.ts.URL, aid), nil, http.StatusOK)
	if rep["aggregates_written"].(float64) != 2 || rep["officials_set"].(float64) != 0 {
		t.Fatalf("majority report: %v", rep)
	}
	if rep["flagged"].(map[string]any)["agg_disagreement"].(float64) != 2 {
		t.Fatalf("flagged: %v", rep["flagged"])
	}

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	agg := detail["records"].([]any)[0].(map[string]any)
	if agg["source"] != "aggregate" || agg["total"] != "3" { // 1.5 + 1.5
		t.Fatalf("aggregate record: source=%v total=%v", agg["source"], agg["total"])
	}

	// Mean, f=0: 1 vs 2 → mean 1.5, both within one increment (0.5) → clean:
	// flags cleared, and the tail derivation makes the (new) aggregates official.
	putPolicy(t, env, c, aid, map[string]any{
		"method_version_ids": mvIDs, "combiner": "mean", "fault_tolerance": 0,
		"flag_triggers": []string{"agg_disagreement", "agg_missing"},
	}, http.StatusOK)
	rep = postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/aggregate", env.ts.URL, aid), nil, http.StatusOK)
	if rep["officials_set"].(float64) != 2 || len(rep["flagged"].(map[string]any)) != 0 {
		t.Fatalf("mean report: %v", rep)
	}
	detail = getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	if flags := detail["answer"].(map[string]any)["flags"].([]any); len(flags) != 0 {
		t.Fatalf("flags should be cleared: %v", flags)
	}
	official := detail["answer"].(map[string]any)["official_record_id"]
	latest := detail["records"].([]any)[0].(map[string]any)
	if official == nil || int64(official.(float64)) != int64(latest["id"].(float64)) || latest["source"] != "aggregate" {
		t.Fatalf("official should be the latest aggregate: official=%v latest=%v", official, latest["id"])
	}
}

// TestAggregation_ReRunOverUnchangedInputsWritesNoFlagRows pins F11's no-op
// guard: when a re-aggregation raises exactly the same agg_* flag set as the
// last pass (here: none, both models agree), RemoveAnswerFlag's UPDATE must
// match zero rows rather than unconditionally rewriting every answer. The
// answers table stamps updated_at on every flag-column write (see
// AddAnswerFlag/RemoveAnswerFlag in ingestion.sql), so an unchanged updated_at
// after the second pass is the observable proof the no-op guard is in place.
func TestAggregation_ReRunOverUnchangedInputsWritesNoFlagRows(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	m1 := createFakeMethod(t, env, c) // provider "fake", scores 1 per criterion
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	// A second method on the SAME "fake" provider (fake2 defaults to a different
	// score and isn't exposed on testEnv to override) — both panelists then score
	// 1 per criterion, so the panel agrees and no agg_* flag is ever raised.
	m2raw := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake method 2",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-2",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	m2 := int64(m2raw["id"].(float64))
	acceptAllMasks(t, env, c, aid)

	var mvIDs []int64
	for _, mid := range []int64{m1, m2} {
		run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
			"assessment_id": aid, "scope_kind": "assessment", "method_id": mid,
		}, http.StatusCreated)
		driveRun(t, env, int64(run["id"].(float64)), false)
		mvIDs = append(mvIDs, int64(run["method_version_id"].(float64)))
	}

	// No final source is chosen in this fixture, so each pass's tail derivation
	// (0027) finds nothing to clear or set and leaves answers untouched — a
	// chosen consensus source would legitimately re-derive (a fresh aggregate
	// record is written each run: a real state change, not the bug F11 targets).
	// This test isolates the agg_* flag UPDATEs, which SHOULD be no-ops when the
	// flag outcome hasn't changed.
	putPolicy(t, env, c, aid, map[string]any{
		"method_version_ids": mvIDs, "combiner": "mean", "fault_tolerance": 0,
		"flag_triggers": []string{"agg_disagreement", "agg_missing", "agg_low_confidence"},
	}, http.StatusOK)

	// First pass: agreeing panel, nothing flagged.
	rep := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/aggregate", env.ts.URL, aid), nil, http.StatusOK)
	if len(rep["flagged"].(map[string]any)) != 0 {
		t.Fatalf("setup: first pass should raise no flags: %v", rep["flagged"])
	}

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	var answerIDs []int64
	for _, st := range students["students"] {
		answerIDs = append(answerIDs, int64(st["answer_id"].(float64)))
	}
	if len(answerIDs) != 2 {
		t.Fatalf("expected 2 answers, got %d", len(answerIDs))
	}

	before := map[int64]string{}
	for _, id := range answerIDs {
		a, err := env.st.Q.GetAnswer(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAnswer(%d): %v", id, err)
		}
		before[id] = a.UpdatedAt.Time.String()
	}

	// Second pass over identical inputs: same flag outcome (none), so the
	// RemoveAnswerFlag no-op guard must leave updated_at untouched.
	rep = postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/aggregate", env.ts.URL, aid), nil, http.StatusOK)
	if len(rep["flagged"].(map[string]any)) != 0 {
		t.Fatalf("re-run should still raise no flags: %v", rep["flagged"])
	}

	for _, id := range answerIDs {
		a, err := env.st.Q.GetAnswer(context.Background(), id)
		if err != nil {
			t.Fatalf("GetAnswer(%d): %v", id, err)
		}
		if got := a.UpdatedAt.Time.String(); got != before[id] {
			t.Errorf("answer %d updated_at changed on a no-op re-aggregation: before=%s after=%s", id, before[id], got)
		}
	}
}

// TestAggregation_WithoutChosenSource_DerivesNothing: aggregation is pure
// derived math (D17) and the official pointer only ever follows the exam's
// chosen final source (0027) — a clean consensus pass over an exam that has
// NOT picked consensus (or anything) writes its aggregate records but moves
// zero officials. (The old aggregation-time spot-check gate is gone: the
// sample now gates publish for method-sourced exams only, covered in
// final_source_test.go.)
func TestAggregation_WithoutChosenSource_DerivesNothing(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	m1 := createFakeMethod(t, env, c)
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2raw := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake method 2",
		"config": map[string]any{
			"provider": "fake", "model": "fake-vision-2",
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	m2 := int64(m2raw["id"].(float64))
	acceptAllMasks(t, env, c, aid)

	var mvIDs []int64
	for _, mid := range []int64{m1, m2} {
		run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
			"assessment_id": aid, "scope_kind": "assessment", "method_id": mid,
		}, http.StatusCreated)
		driveRun(t, env, int64(run["id"].(float64)), false)
		mvIDs = append(mvIDs, int64(run["method_version_id"].(float64)))
	}

	// Both methods run on the SAME fake provider → identical scores → clean pass.
	putPolicy(t, env, c, aid, map[string]any{
		"method_version_ids": mvIDs, "combiner": "mean", "fault_tolerance": 0,
		"flag_triggers": []string{"agg_disagreement", "agg_missing"},
	}, http.StatusOK)

	rep := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/aggregate", env.ts.URL, aid), nil, http.StatusOK)
	if rep["aggregates_written"].(float64) != 2 {
		t.Fatalf("expected aggregate records written for both answers: %v", rep)
	}
	if rep["officials_set"].(float64) != 0 || len(rep["flagged"].(map[string]any)) != 0 {
		t.Fatalf("no chosen source: expected officials_set=0 and no flags, got %v", rep)
	}

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	if official := detail["answer"].(map[string]any)["official_record_id"]; official != nil {
		t.Fatalf("official should stay a hole until the exam chooses its source, got %v", official)
	}

	// Choosing consensus NOW derives from the aggregates this pass already wrote.
	res := putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)
	if moved := int(res["officials_moved"].(float64)); moved != 2 {
		t.Fatalf("choosing consensus should derive both aggregates official, moved %d", moved)
	}
}

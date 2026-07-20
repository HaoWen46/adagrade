package httpapi

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"testing"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// createFakeProviderRow creates an llm_providers row named "fake" (or "fake2")
// through the CRUD API so cost lookup (which resolves the provider by name via
// the DB, independent of the runner's llm.StaticSource) can find it. Grading
// itself still goes through env.runner.Providers (the StaticSource) — this row
// only backs pricing/cost resolution, exactly as it would in production where
// GetProviderByName IS the provider registry.
func createFakeProviderRow(t *testing.T, env *testEnv, lect *http.Client, name string) int64 {
	t.Helper()
	created := postExpect(t, lect, env.ts.URL+"/api/providers", map[string]any{
		"name": name, "base_url": "https://example.test/" + name, "api_key": "sk-test-00000",
	}, http.StatusCreated)
	return int64(created["id"].(float64))
}

// setPricing PUTs a pricing row via the API.
func setPricing(t *testing.T, env *testEnv, lect *http.Client, providerID int64, model, in, out string) {
	t.Helper()
	res := putJSON(t, lect, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, providerID), map[string]any{
		"model": model, "input_usd_per_mtok": in, "output_usd_per_mtok": out,
	}, http.StatusOK)
	if res["model"] != model {
		t.Fatalf("setPricing: %v", res)
	}
}

func TestPricingCRUD_RoleGateAndRoundTrip(t *testing.T) {
	env := harnessEnv(t)
	lect := loginAs(t, env.ts, env.st, "lect@ntu.edu.tw", "lecturer")
	ta := loginAs(t, env.ts, env.st, "ta@ntu.edu.tw", "ta")
	pid := createFakeProviderRow(t, env, lect, "pricing-prov")

	// TA cannot write pricing.
	resp := putJSONRaw(t, ta, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, pid), map[string]any{
		"model": "m1", "input_usd_per_mtok": "3.00", "output_usd_per_mtok": "15.00",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ta put pricing: got %d want 403", resp.StatusCode)
	}

	// Lecturer sets pricing.
	setPricing(t, env, lect, pid, "m1", "3.00", "15.00")

	// Any signed-in role (TA) can list.
	listed := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, pid), http.StatusOK)
	rows := listed["pricing"].([]any)
	if len(rows) != 1 {
		t.Fatalf("pricing list: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["model"] != "m1" || row["input_usd_per_mtok"] != "3" || row["output_usd_per_mtok"] != "15" {
		t.Errorf("pricing row: %v", row)
	}

	// Editing is an upsert, not a new row (D35).
	setPricing(t, env, lect, pid, "m1", "2.50", "12.00")
	listed = getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, pid), http.StatusOK)
	rows = listed["pricing"].([]any)
	if len(rows) != 1 {
		t.Fatalf("pricing after edit: got %d rows want 1: %v", len(rows), rows)
	}
	if rows[0].(map[string]any)["input_usd_per_mtok"] != "2.5" {
		t.Errorf("edit did not take effect: %v", rows[0])
	}

	// Unknown provider -> 404.
	resp = putJSONRaw(t, lect, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, pid+99999), map[string]any{
		"model": "m1", "input_usd_per_mtok": "1", "output_usd_per_mtok": "1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("pricing for unknown provider: got %d want 404", resp.StatusCode)
	}

	// Invalid decimal -> 400.
	resp = putJSONRaw(t, lect, fmt.Sprintf("%s/api/providers/%d/pricing", env.ts.URL, pid), map[string]any{
		"model": "m1", "input_usd_per_mtok": "not-a-number", "output_usd_per_mtok": "1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid pricing decimal: got %d want 400", resp.StatusCode)
	}
}

// TestGradeLeaf_ComputesCostFromPricing_NullWithoutPricing pins that cost_usd is
// computed at insert time from token counts x pricing (trust spec §2), and stays
// NULL — never a fake $0 — for a model with no pricing row. Checked at the store
// layer (grading_records.cost_usd) since cost_usd is not yet surfaced on the
// answer-detail JSON contract (that's the spec §7/D40 cost-reporting deliverable,
// out of scope here).
func TestGradeLeaf_ComputesCostFromPricing_NullWithoutPricing(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	// Provider row + pricing for the fake provider's model (fake.Provider records
	// 100 input / 50 output tokens per call, see internal/llm/fake).
	provID := createFakeProviderRow(t, env, c, "fake")
	setPricing(t, env, c, provID, "fake-vision-1", "3.00", "15.00")

	// Scope to ONE answer so the run's total cost is exactly one leaf's cost.
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))
	otherAnswer := int64(students["students"][1]["answer_id"].(float64))

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": answerID, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	recs := detail["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	recID := int64(recs[0].(map[string]any)["id"].(float64))
	dbRec, err := env.st.Q.GetRecord(t.Context(), recID)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	// 100 input * 3.00/Mtok + 50 output * 15.00/Mtok = 0.0003 + 0.00075 = 0.00105
	if !dbRec.CostUsd.Valid || store.NumStr(dbRec.CostUsd) != "0.00105" {
		t.Errorf("cost_usd with pricing present: got %+v want 0.00105", dbRec.CostUsd)
	}

	costRow, err := env.st.RunCost(t.Context(), runID)
	if err != nil {
		t.Fatalf("RunCost: %v", err)
	}
	if costRow.TotalUSD != "0.00105" || costRow.InputTokens != 100 || costRow.OutputTokens != 50 {
		t.Errorf("RunCost: got %+v want total=0.00105 in=100 out=50", costRow)
	}

	// Second run on "fake2" — no pricing row for it — cost_usd must stay NULL.
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2 := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake2 method",
		"config": map[string]any{
			"provider": "fake2", "model": "fake2-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	run2 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": otherAnswer, "method_id": int64(m2["id"].(float64)),
	}, http.StatusCreated)
	driveRun(t, env, int64(run2["id"].(float64)), false)

	detail2 := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, otherAnswer), http.StatusOK)
	recs2 := detail2["records"].([]any)
	if len(recs2) != 1 {
		t.Fatalf("records2: %v", recs2)
	}
	recID2 := int64(recs2[0].(map[string]any)["id"].(float64))
	dbRec2, err := env.st.Q.GetRecord(t.Context(), recID2)
	if err != nil {
		t.Fatalf("GetRecord: %v", err)
	}
	if dbRec2.CostUsd.Valid {
		t.Errorf("cost_usd without a pricing row: got %+v, want NULL (absence must stay visible, D35)", dbRec2.CostUsd)
	}
}

// TestRunCostCap_StopsRemainingLeavesAsBudgetExceeded_RetryableAfterRaise pins
// D36's per-run cap: once accumulated spend >= cost_cap_usd, the leaf executor
// stops calling the provider and marks remaining leaves terminally failed with
// reason "budget_exceeded" — never silently skipped, never retried forever. Once
// the cap is raised, retry-failed-only must pick those leaves back up (they are
// ordinary terminal failures like any other, per ResetFailedItems).
func TestRunCostCap_StopsRemainingLeavesAsBudgetExceeded_RetryableAfterRaise(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	provID := createFakeProviderRow(t, env, c, "fake")
	// 100 input * 300.00/Mtok + 50 output * 1500.00/Mtok = 0.03 + 0.075 = 0.105 per
	// leaf. Priced high (rather than realistic $/Mtok) so the per-leaf cost lands
	// on real cents — runs.cost_cap_usd is NUMERIC(10,2), so a cap finer than
	// $0.01 would round away to nothing and this test needs a cap that actually
	// distinguishes "before leaf 1" from "after leaf 1".
	setPricing(t, env, c, provID, "fake-vision-1", "300.00", "1500.00")

	// Cap set so that exactly ONE leaf can afford to run: the FIRST leaf always
	// checks accumulated spend=0 < any positive cap and proceeds; after it posts
	// its 0.105 cost, the SECOND leaf's check sees spend(0.105) >= cap and stops.
	// So the cap must be in (0, 0.105] at NUMERIC(10,2) precision — pick 0.10.
	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
		"cost_cap_usd": "0.10",
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))

	// Sanity: the cap round-tripped onto the run row.
	runRow, err := env.st.Q.GetRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !runRow.CostCapUsd.Valid {
		t.Fatalf("cost_cap_usd not set on run: %+v", runRow)
	}

	driveRun(t, env, runID, true) // finalAttempt: convert any retryable into terminal

	got := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	counts := got["run"].(map[string]any)["counts"].(map[string]any)
	// 2 answers in this assessment (phase4Setup): one succeeds, one is stopped by
	// the cap before it ever calls the provider.
	if counts["succeeded"].(float64) != 1 || counts["failed"].(float64) != 1 {
		t.Fatalf("counts after cap stop: %v", counts)
	}

	items := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d?all=1", env.ts.URL, runID), http.StatusOK)["items"].([]any)
	var sawBudgetExceeded bool
	for _, it := range items {
		m := it.(map[string]any)
		if m["state"] == "failed" {
			if m["error"] != grading.BudgetExceededReason {
				t.Errorf("failed item error reason: got %v want %q", m["error"], grading.BudgetExceededReason)
			}
			sawBudgetExceeded = true
		}
	}
	if !sawBudgetExceeded {
		t.Fatalf("expected one failed item with budget_exceeded, items: %v", items)
	}

	// The fake provider must NOT have been called for the stopped leaf: only 1
	// Grade call total (the one that succeeded before the cap tripped).
	if len(env.fakeProv.Calls) != 1 {
		t.Errorf("fake provider Grade calls: got %d want 1 (cap must stop BEFORE calling the provider)", len(env.fakeProv.Calls))
	}

	// Raising the cap + retry-failed-only must work exactly like any other
	// terminal failure (F17 rule: budget_exceeded is a real terminal verdict, not
	// a shutdown-cancel, so it's eligible for the normal retry path).
	raisedCap, err := store.Num("5.00")
	if err != nil {
		t.Fatalf("Num: %v", err)
	}
	if _, err := env.st.Q.SetRunCostCap(t.Context(), db.SetRunCostCapParams{ID: runID, CostCapUsd: raisedCap}); err != nil {
		t.Fatalf("raise cap: %v", err)
	}
	retried := postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/retry-failed", env.ts.URL, runID), nil, http.StatusOK)
	if retried["retried"].(float64) != 1 {
		t.Fatalf("retry-failed after cap raise: %v", retried)
	}
	driveRun(t, env, runID, false)

	final := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	finalCounts := final["run"].(map[string]any)["counts"].(map[string]any)
	if final["run"].(map[string]any)["status"] != "completed" || finalCounts["succeeded"].(float64) != 2 {
		t.Fatalf("after cap raise + retry: %v", final)
	}
}

// TestRunCreation_MonthlyBudget409 pins D36's monthly global cap: run creation
// compares month-to-date spend + this run's pre-flight estimate against
// ADAMARKER_MONTHLY_BUDGET_USD and refuses with 409 + {month_to_date, estimate,
// budget} when it would be exceeded. An unconfigured budget never blocks.
func TestRunCreation_MonthlyBudget409(t *testing.T) {
	env := harnessEnvWithEnv(t, map[string]string{"ADAMARKER_MONTHLY_BUDGET_USD": "0.001"}) // a tiny budget, easy to exceed
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	seedRole(t, env.st, "lect@ntu.edu.tw", "lecturer")
	resp := devLogin(t, env.ts, c, "lect@ntu.edu.tw")
	resp.Body.Close()

	aid, _, _ := phase4SetupOn(t, env, c)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	provID := createFakeProviderRow(t, env, c, "fake")
	// Expensive pricing so 2 answers' estimate blows the $0.001 budget:
	// estimate = 2 * (1500*3 + 400*15)/1e6 = 2*0.0105 = 0.021 >> 0.001.
	setPricing(t, env, c, provID, "fake-vision-1", "3.00", "15.00")

	resp2 := postJSON(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("run creation over monthly budget: got %d want 409", resp2.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, resp2, &body)
	if body["month_to_date"] != "0" {
		t.Errorf("month_to_date: got %v want \"0\"", body["month_to_date"])
	}
	if body["estimate"] != "0.021" {
		t.Errorf("estimate: got %v want \"0.021\"", body["estimate"])
	}
	if body["budget"] != "0.001" {
		t.Errorf("budget: got %v want \"0.001\"", body["budget"])
	}
}

// TestRunCreation_MonthlyBudget_UnknownEstimateDoesNotBlock pins "unknown, never
// a fake $0" (trust spec §3): when pricing is missing, the estimate can't be
// computed, so the monthly-budget comparison has nothing sound to check against
// and must not block the run.
func TestRunCreation_MonthlyBudget_UnknownEstimateDoesNotBlock(t *testing.T) {
	env := harnessEnvWithEnv(t, map[string]string{"ADAMARKER_MONTHLY_BUDGET_USD": "0.001"})
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	seedRole(t, env.st, "lect@ntu.edu.tw", "lecturer")
	resp := devLogin(t, env.ts, c, "lect@ntu.edu.tw")
	resp.Body.Close()

	aid, _, _ := phase4SetupOn(t, env, c)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)
	// No provider row / no pricing set for "fake" — estimate is unknown.

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	if run["id"] == nil {
		t.Fatalf("run should launch when the estimate is unknown: %v", run)
	}
}

// TestCostExposure_RecordAndRunJSON pins the R1-deferred, R3-owned deliverable
// (trust spec §7, D40; see "recordJSON" note in n2-R1-report.md): cost_usd/tokens
// on the answer-detail record JSON, and cost_usd/input_tokens/output_tokens on the
// run list/detail JSON, using the exact same fixture math as
// TestGradeLeaf_ComputesCostFromPricing_NullWithoutPricing (100in/50out tokens per
// fake-provider call, $3.00/$15.00 per Mtok -> $0.00105/leaf).
func TestCostExposure_RecordAndRunJSON(t *testing.T) {
	env, c, aid, pid, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	provID := createFakeProviderRow(t, env, c, "fake")
	setPricing(t, env, c, provID, "fake-vision-1", "3.00", "15.00")

	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", env.ts.URL, pid), http.StatusOK)
	answerID := int64(students["students"][0]["answer_id"].(float64))

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": answerID, "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	// recordJSON: cost_usd/input_tokens/output_tokens on the answer-detail record.
	detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/answers/%d", env.ts.URL, answerID), http.StatusOK)
	recs := detail["records"].([]any)
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	rec := recs[0].(map[string]any)
	if rec["cost_usd"] != "0.00105" {
		t.Errorf("record cost_usd: got %v want 0.00105", rec["cost_usd"])
	}
	if rec["input_tokens"].(float64) != 100 || rec["output_tokens"].(float64) != 50 {
		t.Errorf("record tokens: got in=%v out=%v want 100/50", rec["input_tokens"], rec["output_tokens"])
	}

	// runJSON (detail): same figures, summed over the run's records.
	runDetail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, runID), http.StatusOK)
	runObj := runDetail["run"].(map[string]any)
	if runObj["cost_usd"] != "0.00105" {
		t.Errorf("run detail cost_usd: got %v want 0.00105", runObj["cost_usd"])
	}
	if runObj["input_tokens"].(float64) != 100 || runObj["output_tokens"].(float64) != 50 {
		t.Errorf("run detail tokens: got in=%v out=%v want 100/50", runObj["input_tokens"], runObj["output_tokens"])
	}

	// runJSON (list): same figures via GET /api/runs.
	runList := getJSON[map[string]any](t, c, env.ts.URL+"/api/runs", http.StatusOK)
	runs := runList["runs"].([]any)
	var found map[string]any
	for _, r := range runs {
		rm := r.(map[string]any)
		if int64(rm["id"].(float64)) == runID {
			found = rm
		}
	}
	if found == nil {
		t.Fatalf("run %d not found in list: %v", runID, runs)
	}
	if found["cost_usd"] != "0.00105" {
		t.Errorf("run list cost_usd: got %v want 0.00105", found["cost_usd"])
	}
	if found["input_tokens"].(float64) != 100 || found["output_tokens"].(float64) != 50 {
		t.Errorf("run list tokens: got in=%v out=%v want 100/50", found["input_tokens"], found["output_tokens"])
	}

	// A run with zero priced records reports cost_usd "0", never a missing/null field.
	provID2 := createFakeProviderRow(t, env, c, "fake-nopricing")
	_ = provID2
	tpl := getJSON[map[string]any](t, c, env.ts.URL+"/api/prompt-templates/transcribe-then-grade", http.StatusOK)
	m2 := postExpect(t, c, env.ts.URL+"/api/methods", map[string]any{
		"name": "Fake2 method (no pricing)",
		"config": map[string]any{
			"provider": "fake2", "model": "fake2-vision-1",
			"temperature": 0, "ref_solutions": 0, "reask_cap": 2,
			"prompt_template_version_id": int64(tpl["id"].(float64)),
		},
	}, http.StatusCreated)
	otherAnswer := int64(students["students"][1]["answer_id"].(float64))
	run2 := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "answer", "scope_id": otherAnswer, "method_id": int64(m2["id"].(float64)),
	}, http.StatusCreated)
	run2ID := int64(run2["id"].(float64))
	driveRun(t, env, run2ID, false)

	run2Detail := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d", env.ts.URL, run2ID), http.StatusOK)
	run2Obj := run2Detail["run"].(map[string]any)
	if run2Obj["cost_usd"] != "0" {
		t.Errorf("run2 cost_usd (no pricing): got %v want \"0\" (not null)", run2Obj["cost_usd"])
	}
}

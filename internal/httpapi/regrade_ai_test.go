package httpapi

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// --- AI re-grade assist endpoints (spec §8, per-sub-item re-scope) -------------------
//
// These exercise POST /api/regrades/{id}/ai-regrade {problem_id} and POST
// /api/regrades/ai-regrade-all: eligibility gating (per sub-item), enqueue, the batch
// estimate/skip counts, the monthly-budget 409, cross-assessment isolation, and audit.
// The runner-side job behavior (source/policy/linkage/redaction/context-isolation/F17) is
// covered in internal/grading.

func TestAIRegrade_EligibleSubItem_Enqueues(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-ai-1")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-ai@ntu.edu.tw", "ta")
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id),
		map[string]any{"problem_id": f.pid}, http.StatusAccepted)
	if int(got["enqueued"].(float64)) != 1 {
		t.Errorf("enqueued: got %v want 1", got["enqueued"])
	}
	again := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id),
		map[string]any{"problem_id": f.pid}, http.StatusAccepted)
	if int(again["enqueued"].(float64)) != 0 {
		t.Errorf("duplicate enqueue: got %v want 0", again["enqueued"])
	}
	if jobs := countRegradeAIJobs(t, f); jobs != 1 {
		t.Errorf("duplicate enqueue created %d jobs, want 1 total", jobs)
	}

	admin := loginAs(t, f.re.ts, f.re.st, "admin-ai@ntu.edu.tw", "admin")
	entries := getJSON[map[string]any](t, admin,
		fmt.Sprintf("%s/api/audit?target_kind=regrade_request&target_id=%d", f.re.ts.URL, id), http.StatusOK)
	var found bool
	for _, row := range entries["entries"].([]any) {
		if row.(map[string]any)["action"] == "regrade.ai_regrade" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a regrade.ai_regrade audit row, got %v", entries["entries"])
	}
}

func TestAIRegrade_UnknownProblem_404(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-ai-unknown")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-aiu@ntu.edu.tw", "ta")
	// A problem number not contested on this request.
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id), map[string]any{"problem_id": 999999})
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("ai-regrade for a non-contested problem: got %d want 404", r.StatusCode)
	}
}

func TestAIRegrade_ResolvedRequest_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-ai-res")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-res@ntu.edu.tw", "ta")

	// Verdict + send the result so the request resolves, then AI-regrade must 409.
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)
	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id), map[string]any{"problem_id": f.pid})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("resolved ai-regrade: got %d want 409", r.StatusCode)
	}
}

func TestAIRegrade_AlreadyHasRecord_409UnlessRerun(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-ai-rec")
	rec := seedGradingRecordForSubItem(t, f, id, subID)
	if _, err := f.re.st.SetProblemAIRecord(t.Context(), subID, rec); err != nil {
		t.Fatal(err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-rec@ntu.edu.tw", "ta")
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id), map[string]any{"problem_id": f.pid})
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("already-graded ai-regrade without rerun: got %d want 409", r.StatusCode)
	}
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade?rerun=1", f.re.ts.URL, id),
		map[string]any{"problem_id": f.pid}, http.StatusAccepted)
	if int(got["enqueued"].(float64)) != 1 {
		t.Errorf("rerun enqueued: got %v want 1", got["enqueued"])
	}
}

func TestAIRegrade_MonthlyBudget409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_MONTHLY_BUDGET_USD":     "0.01",
	})
	id, _ := fileRequest(t, f, f.token, "pm-single-budget-1")
	priceContestedRecord(t, f, id, "single-budget-prov", "expensive-model", "1000", "1000")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-single-budget@ntu.edu.tw", "ta")
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id), map[string]any{"problem_id": f.pid})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("over-budget single ai-regrade: got %d want 409", r.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, r, &body)
	for _, k := range []string{"month_to_date", "estimate", "budget"} {
		if _, ok := body[k]; !ok {
			t.Errorf("409 body missing %q: %v", k, body)
		}
	}
	if n := countRegradeAIJobs(t, f); n != 0 {
		t.Errorf("over-budget single ai-regrade should enqueue nothing, got %d jobs", n)
	}
}

func TestAIRegrade_UnderBudget_Enqueues(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_MONTHLY_BUDGET_USD":     "100000",
	})
	id, _ := fileRequest(t, f, f.token, "pm-single-underbudget-1")
	priceContestedRecord(t, f, id, "single-under-prov", "cheap-model", "3.00", "15.00")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-single-under@ntu.edu.tw", "ta")
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id),
		map[string]any{"problem_id": f.pid}, http.StatusAccepted)
	if int(got["enqueued"].(float64)) != 1 {
		t.Errorf("under-budget single ai-regrade enqueued: got %v want 1", got["enqueued"])
	}
}

func TestAIRegrade_RerunOverBudget_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_MONTHLY_BUDGET_USD":     "0.01",
	})
	id, subID := fileRequest(t, f, f.token, "pm-rerun-budget-1")
	priceContestedRecord(t, f, id, "rerun-budget-prov", "expensive-model", "1000", "1000")
	rec := seedGradingRecordForSubItem(t, f, id, subID)
	if _, err := f.re.st.SetProblemAIRecord(t.Context(), subID, rec); err != nil {
		t.Fatal(err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-rerun-budget@ntu.edu.tw", "ta")
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/ai-regrade?rerun=1", f.re.ts.URL, id), map[string]any{"problem_id": f.pid})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("over-budget rerun ai-regrade: got %d want 409 (must not re-spend ungated)", r.StatusCode)
	}
}

func TestAIRegrade_NotFound_404(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	ta := loginAs(t, f.re.ts, f.re.st, "ta-404@ntu.edu.tw", "ta")
	r := postJSON(t, ta, f.re.ts.URL+"/api/regrades/999999/ai-regrade", map[string]any{"problem_id": f.pid})
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("ai-regrade nonexistent: got %d want 404", r.StatusCode)
	}
}

func TestAIRegrade_RequiresSession(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-ai-auth")
	unauth := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/regrades/%d/ai-regrade", f.re.ts.URL, id), nil)
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := unauth.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ai-regrade: got %d want 401", resp.StatusCode)
	}
}

// TestAIRegradeAll_EnqueuesEligibleSkipsGradedAndCounts: two filed requests each with one
// sub-item, one sub-item already carrying an AI record. The batch enqueues the eligible
// sub-item and reports the graded one as skipped.
func TestAIRegradeAll_EnqueuesEligibleSkipsGradedAndCounts(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id1, _ := fileRequest(t, f, f.token, "pm-all-1")
	tok2 := mintV2Token(t, f.re, f.itemID, 2)
	id2, sub2 := fileRequest(t, f, tok2, "pm-all-2")

	// id2's sub-item already graded → skipped.
	rec := seedGradingRecordForSubItem(t, f, id2, sub2)
	if _, err := f.re.st.SetProblemAIRecord(t.Context(), sub2, rec); err != nil {
		t.Fatal(err)
	}
	_ = id1

	ta := loginAs(t, f.re.ts, f.re.st, "ta-all@ntu.edu.tw", "ta")
	got := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)
	if int(got["enqueued"].(float64)) != 1 {
		t.Errorf("enqueued: got %v want 1 (only the un-graded eligible sub-item)", got["enqueued"])
	}
	if int(got["skipped"].(float64)) != 1 {
		t.Errorf("skipped: got %v want 1 (the already-graded sub-item)", got["skipped"])
	}

	admin := loginAs(t, f.re.ts, f.re.st, "admin-all@ntu.edu.tw", "admin")
	entries := getJSON[map[string]any](t, admin,
		fmt.Sprintf("%s/api/audit?target_kind=assessment&target_id=%d", f.re.ts.URL, f.aid), http.StatusOK)
	var found bool
	for _, row := range entries["entries"].([]any) {
		if row.(map[string]any)["action"] == "regrade.ai_regrade_all" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a regrade.ai_regrade_all audit row, got %v", entries["entries"])
	}
}

// TestAIRegradeAll_CrossAssessmentIsolation: another assessment's eligible sub-items are
// NOT enqueued by a batch scoped to the first assessment.
func TestAIRegradeAll_CrossAssessmentIsolation(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, _ = fileRequest(t, f, f.token, "pm-iso-1")

	// A second, unrelated assessment with an eligible sub-item (inserted directly).
	other := postExpect(t, f.c, f.re.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Other"}, http.StatusCreated)
	otherAid := int64(other["id"].(float64))
	otherProblem := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/problems", f.re.ts.URL, otherAid),
		map[string]any{"number": 1, "title": "P", "max_points": "10"}, http.StatusCreated)
	otherPid := int64(otherProblem["id"].(float64))
	seedStudent(t, f.re.st, "b02", "Other Student", "other@example.edu")
	otherStudent, err := f.re.st.Q.GetStudentByExternalID(t.Context(), "b02")
	if err != nil {
		t.Fatal(err)
	}
	// A filed request for the other assessment, needing a publish item — mint one directly.
	otherItemID := seedPublishItemDirect(t, f, otherAid, otherStudent.ID)
	rr, err := f.re.st.InsertRegradeRequestV2(t.Context(), store.InsertRegradeRequestV2Params{
		PublishItemID: otherItemID, StudentID: otherStudent.ID, AssessmentID: otherAid,
		FromEmail: "other@example.edu", Subject: "Re", Body: "recheck",
		Status: "received", Kind: "filed", Turn: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.re.st.InsertRequestProblems(t.Context(), rr.ID, []store.RequestProblemInput{
		{ProblemID: otherPid, ComplaintText: "recheck"},
	}); err != nil {
		t.Fatal(err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-iso@ntu.edu.tw", "ta")
	otherRes := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": otherAid}, http.StatusAccepted)
	if int(otherRes["enqueued"].(float64)) != 1 {
		t.Errorf("other-assessment batch enqueued %v, want 1 (its own sub-item only)", otherRes["enqueued"])
	}

	firstRes := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)
	if int(firstRes["enqueued"].(float64)) != 1 {
		t.Errorf("first-assessment batch enqueued %v, want 1 (its own sub-item only)", firstRes["enqueued"])
	}
}

func TestAIRegradeAll_MonthlyBudget409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_MONTHLY_BUDGET_USD":     "0.01",
	})
	id, _ := fileRequest(t, f, f.token, "pm-budget-1")
	priceContestedRecord(t, f, id, "budget-prov", "expensive-model", "1000", "1000")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-budget@ntu.edu.tw", "ta")
	r := postJSON(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all", map[string]any{"assessment_id": f.aid})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("over-budget batch: got %d want 409", r.StatusCode)
	}
	if n := countRegradeAIJobs(t, f); n != 0 {
		t.Errorf("over-budget batch should enqueue nothing, got %d jobs", n)
	}
}

func TestAIRegradeAll_EstimateKnownWithPricing(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-est-1")
	priceContestedRecord(t, f, id, "est-prov", "est-model", "3.00", "15.00")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-est@ntu.edu.tw", "ta")
	got := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)
	est, ok := got["estimated_cost"].(string)
	if !ok || est == "" {
		t.Fatalf("estimated_cost should be a non-empty decimal string, got %v", got["estimated_cost"])
	}
}

// --- ai-regrade-all dry-run ----------------------------------------------------------

func countRegradeAIJobs(t *testing.T, f regradeFixture) int {
	t.Helper()
	var n int
	if err := f.re.st.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM river_job WHERE kind = 'regrade.ai'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAIRegradeAll_DryRun_EnqueuesNothing(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, _ = fileRequest(t, f, f.token, "pm-dry-1")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-dry@ntu.edu.tw", "ta")
	got := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid, "dry_run": true}, http.StatusAccepted)

	if int(got["enqueued"].(float64)) != 1 {
		t.Errorf("dry-run enqueued (would-be count): got %v want 1", got["enqueued"])
	}
	if dr, _ := got["dry_run"].(bool); !dr {
		t.Errorf("dry_run: got %v want true", got["dry_run"])
	}
	if n := countRegradeAIJobs(t, f); n != 0 {
		t.Errorf("river_job regrade.ai rows after dry-run: got %d want 0", n)
	}
}

func TestAIRegradeAll_DryRun_NoAuditRow(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, _ = fileRequest(t, f, f.token, "pm-dry-audit")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-dry-audit@ntu.edu.tw", "ta")
	postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid, "dry_run": true}, http.StatusAccepted)

	admin := loginAs(t, f.re.ts, f.re.st, "admin-dry-audit@ntu.edu.tw", "admin")
	entries := getJSON[map[string]any](t, admin,
		fmt.Sprintf("%s/api/audit?target_kind=assessment&target_id=%d", f.re.ts.URL, f.aid), http.StatusOK)
	for _, row := range entries["entries"].([]any) {
		if row.(map[string]any)["action"] == "regrade.ai_regrade_all" {
			t.Errorf("dry-run must not write a regrade.ai_regrade_all audit row, got %v", row)
		}
	}
}

func TestAIRegradeAll_DryRun_MatchesSubsequentRealCall(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id1, _ := fileRequest(t, f, f.token, "pm-dry-match-1")
	priceContestedRecord(t, f, id1, "dry-match-prov", "dry-match-model", "3.00", "15.00")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-dry-match@ntu.edu.tw", "ta")
	dry := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid, "dry_run": true}, http.StatusAccepted)
	real := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)

	if dry["enqueued"] != real["enqueued"] {
		t.Errorf("enqueued mismatch: dry-run %v, real %v", dry["enqueued"], real["enqueued"])
	}
	if dry["skipped"] != real["skipped"] {
		t.Errorf("skipped mismatch: dry-run %v, real %v", dry["skipped"], real["skipped"])
	}
	if dry["estimated_cost"] != real["estimated_cost"] {
		t.Errorf("estimated_cost mismatch: dry-run %v, real %v", dry["estimated_cost"], real["estimated_cost"])
	}
	if n := countRegradeAIJobs(t, f); n != 1 {
		t.Errorf("river_job regrade.ai rows after the real call: got %d want 1", n)
	}
}

func TestAIRegradeAll_DryRun_QueryParamAlternative(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, _ = fileRequest(t, f, f.token, "pm-dry-qp-1")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-dry-qp@ntu.edu.tw", "ta")
	got := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all?dry_run=1",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)
	if dr, _ := got["dry_run"].(bool); !dr {
		t.Errorf("dry_run via query param: got %v want true", got["dry_run"])
	}
	if n := countRegradeAIJobs(t, f); n != 0 {
		t.Errorf("river_job regrade.ai rows after query-param dry-run: got %d want 0", n)
	}
}

func TestAIRegradeAll_RealPath_Unchanged(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, _ = fileRequest(t, f, f.token, "pm-real-unchanged")

	ta := loginAs(t, f.re.ts, f.re.st, "ta-real-unchanged@ntu.edu.tw", "ta")
	got := postExpect(t, ta, f.re.ts.URL+"/api/regrades/ai-regrade-all",
		map[string]any{"assessment_id": f.aid}, http.StatusAccepted)
	if v, ok := got["dry_run"]; ok {
		t.Errorf("real call should not include dry_run in the response, got %v", v)
	}
	if n := countRegradeAIJobs(t, f); n != 1 {
		t.Errorf("river_job regrade.ai rows after real call: got %d want 1", n)
	}
}

// --- test-only DB helpers ------------------------------------------------------------

// seedPublishItemDirect mints a throwaway publish batch + item for a (student, assessment)
// so a directly-inserted filed request has a valid publish_item_id. Invented data only.
func seedPublishItemDirect(t *testing.T, f regradeFixture, assessmentID, studentID int64) int64 {
	t.Helper()
	ctx := t.Context()
	batch, err := f.re.st.Q.CreatePublishBatch(ctx, db.CreatePublishBatchParams{AssessmentID: assessmentID, Attachment: "none"})
	if err != nil {
		t.Fatalf("create publish batch: %v", err)
	}
	item, err := f.re.st.Q.CreatePublishItem(ctx, db.CreatePublishItemParams{
		BatchID: batch.ID, StudentID: studentID, Snapshot: []byte("{}"),
		RecipientEmail: "x@example.edu", EmailStatus: "sent",
	})
	if err != nil {
		t.Fatalf("create publish item: %v", err)
	}
	return item.ID
}

// priceContestedRecord makes a request's contested official record a priced MODEL record
// so the estimate resolves a known per-answer cost.
func priceContestedRecord(t *testing.T, f regradeFixture, requestID int64, provider, model, inPrice, outPrice string) {
	t.Helper()
	ctx := t.Context()
	rr, err := f.re.st.GetRegradeRequest(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.re.st.Pool.Exec(ctx,
		`UPDATE grading_records SET source = 'model', provider = $2, model_id = $3
		 WHERE id = (SELECT official_record_id FROM answers
		             WHERE assessment_id = $1 AND student_id = $4 AND official_record_id IS NOT NULL LIMIT 1)`,
		rr.AssessmentID.Int64, provider, model, rr.StudentID.Int64); err != nil {
		t.Fatalf("repoint official record to model: %v", err)
	}
	var providerID int64
	if err := f.re.st.Pool.QueryRow(ctx,
		`INSERT INTO llm_providers (name, kind, base_url, api_key_ciphertext, api_key_hint, models, requests_per_second, burst, enabled)
		 VALUES ($1, 'openai-compat', 'https://example.test', '\x00', 'hint', ARRAY[$2], 1, 1, true)
		 RETURNING id`, provider, model).Scan(&providerID); err != nil {
		t.Fatalf("create provider row: %v", err)
	}
	inNum, _ := store.Num(inPrice)
	outNum, _ := store.Num(outPrice)
	if _, err := f.re.st.Q.UpsertModelPricing(ctx, db.UpsertModelPricingParams{
		ProviderID: providerID, Model: model, InputUsdPerMtok: inNum, OutputUsdPerMtok: outNum,
	}); err != nil {
		t.Fatalf("upsert pricing: %v", err)
	}
}

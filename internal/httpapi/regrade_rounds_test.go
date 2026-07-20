package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/store"
)

// seedRoundMethod inserts a bare method+version (SQL — no provider row needed;
// round-config validation only requires an existing version and, for the batch
// endpoint, a real prompt template) and returns the method id.
func seedRoundMethod(t *testing.T, f regradeFixture, name string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := grading.EnsureSeeds(ctx, f.re.st, discardLogger()); err != nil {
		t.Fatal(err)
	}
	var tplID int64
	if err := f.re.st.Pool.QueryRow(ctx,
		`SELECT id FROM prompt_template_versions ORDER BY id LIMIT 1`).Scan(&tplID); err != nil {
		t.Fatalf("no prompt template seeded: %v", err)
	}
	var methodID int64
	if err := f.re.st.Pool.QueryRow(ctx,
		`INSERT INTO grading_methods (name) VALUES ($1) RETURNING id`, name).Scan(&methodID); err != nil {
		t.Fatalf("insert method: %v", err)
	}
	cfg := fmt.Sprintf(`{"provider":"fake","model":"fake-vision-1","temperature":0,"ref_solutions":0,"reask_cap":2,"prompt_template_version_id":%d}`, tplID)
	if _, err := f.re.st.Pool.Exec(ctx,
		`INSERT INTO grading_method_versions (method_id, version, config) VALUES ($1, 1, $2::jsonb)`,
		methodID, cfg); err != nil {
		t.Fatalf("insert method version: %v", err)
	}
	return methodID
}

// TestRegradeRounds_DeadlineGate: replies after the exam's regrade deadline are
// recorded-but-rejected (rung 3.5); clearing the deadline reopens the window.
func TestRegradeRounds_DeadlineGate(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	lect := loginAs(t, f.re.ts, f.re.st, "lect-deadline@ntu.edu.tw", "lecturer")

	past := time.Now().Add(-time.Hour)
	putJSON(t, lect, fmt.Sprintf("%s/api/assessments/%d/regrade-deadline", f.re.ts.URL, f.aid),
		map[string]any{"deadline": past.Format(time.RFC3339)}, http.StatusOK)

	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	r.Body.Close()
	rows, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 recorded row, got %d err=%v", len(rows), err)
	}
	if rows[0].Status != "rejected_deadline" || rows[0].Kind != "unparsed" {
		t.Fatalf("past-deadline reply: status=%s kind=%s, want rejected_deadline/unparsed", rows[0].Status, rows[0].Kind)
	}

	// Clearing the deadline reopens the window: the same token now files.
	putJSON(t, lect, fmt.Sprintf("%s/api/assessments/%d/regrade-deadline", f.re.ts.URL, f.aid),
		map[string]any{"deadline": nil}, http.StatusOK)
	r2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-after-clear"))
	r2.Body.Close()
	filed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: "filed"})
	if err != nil || len(filed) != 1 {
		t.Fatalf("expected the reply to file after clearing the deadline, got %d err=%v", len(filed), err)
	}
}

// TestRegradeRounds_ConfigLockAndBatch: per-round method config (lecturer+),
// frozen once the round graded anything; the batch endpoint refuses without a
// configured method and dry-runs the pending count with one.
func TestRegradeRounds_ConfigLockAndBatch(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	_, subID := fileRequest(t, f, f.token, "pm-rounds-1")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-rounds@ntu.edu.tw", "lecturer")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-rounds@ntu.edu.tw", "ta")
	methodID := seedRoundMethod(t, f, "Round strict")

	// Batch without config → 409.
	resp := postJSON(t, ta, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds/1/grade", f.re.ts.URL, f.aid), map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("batch without round method: got %d want 409", resp.StatusCode)
	}

	// Configure round 1; GET reflects config + pending count.
	putJSON(t, lect, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds/1", f.re.ts.URL, f.aid),
		map[string]any{"method_id": methodID}, http.StatusOK)
	rounds := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds", f.re.ts.URL, f.aid), http.StatusOK)
	r1 := rounds["rounds"].([]any)[0].(map[string]any)
	if int64(r1["method_id"].(float64)) != methodID || r1["locked"] == true {
		t.Fatalf("round 1 config: %v", r1)
	}
	if int(r1["pending"].(float64)) != 1 {
		t.Fatalf("round 1 pending: got %v want 1", r1["pending"])
	}

	// Out-of-range turn → 400.
	putJSON(t, lect, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds/99", f.re.ts.URL, f.aid),
		map[string]any{"method_id": methodID}, http.StatusBadRequest)

	// Dry-run batch: 1 pending enqueued, nothing actually queued.
	got := postExpect(t, ta, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds/1/grade", f.re.ts.URL, f.aid),
		map[string]any{"dry_run": true}, http.StatusAccepted)
	if int(got["enqueued"].(float64)) != 1 {
		t.Fatalf("dry-run enqueued: got %v want 1", got["enqueued"])
	}

	// Simulate the round having graded: seed a regrade_ai record AND link it to
	// the sub-item (seedGradingRecordForSubItem leaves linking to the caller) —
	// config locks.
	recID := seedGradingRecordForSubItem(t, f, mustRequestID(t, f), subID)
	mustExec(t, f.re.st, `UPDATE regrade_request_problems SET ai_record_id = $2 WHERE id = $1`, subID, recID)
	putResp := putJSONRaw(t, lect, fmt.Sprintf("%s/api/assessments/%d/regrade-rounds/1", f.re.ts.URL, f.aid),
		map[string]any{"method_id": methodID})
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusConflict {
		t.Fatalf("locked round method edit: got %d want 409", putResp.StatusCode)
	}
}

// mustRequestID returns the single filed request's id.
func mustRequestID(t *testing.T, f regradeFixture) int64 {
	t.Helper()
	rows, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: "filed"})
	if err != nil || len(rows) == 0 {
		t.Fatalf("no filed request: %v", err)
	}
	return rows[0].ID
}

// TestRegradeRounds_VerdictAdoptionAndEffectiveTotals: "regraded" adopts the
// round record as the overlay layer; upheld adopts nothing; totals/export
// report the effective (top-of-stack) score while round 0 stays untouched.
func TestRegradeRounds_VerdictAdoptionAndEffectiveTotals(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	reqID, subID := fileRequest(t, f, f.token, "pm-adopt-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-adopt@ntu.edu.tw", "ta")

	// Regraded with no AI record and no explicit adoption → 409.
	patch := func(body map[string]any) *http.Response {
		return patchJSONRaw(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, reqID, subID), body)
	}
	r1 := patch(map[string]any{"outcome": "regraded", "note": "should fail"})
	r1.Body.Close()
	if r1.StatusCode != http.StatusConflict {
		t.Fatalf("regraded without adoptable record: got %d want 409", r1.StatusCode)
	}

	// Seed an AI-style record on the sub-item (the round's grade), link it, and
	// give it a total distinct from the 8.5 round-0 official, then adopt it.
	aiRecID := seedGradingRecordForSubItem(t, f, reqID, subID)
	mustExec(t, f.re.st, `UPDATE grading_records SET total = 10 WHERE id = $1`, aiRecID)
	mustExec(t, f.re.st, `UPDATE regrade_request_problems SET ai_record_id = $2 WHERE id = $1`, subID, aiRecID)
	r2 := patch(map[string]any{"outcome": "regraded", "note": "adopting the round grade"})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("regraded with AI record: got %d want 200", r2.StatusCode)
	}

	// Effective totals: the adopted record's total overrides the round-0 official
	// (fixture official = 8.5; seeded record differs), while the answer's
	// official pointer itself is untouched.
	totals := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/assessments/%d/totals", f.re.ts.URL, f.aid), http.StatusOK)
	row := totals["students"].([]any)[0].(map[string]any)
	if row["total"] == nil || row["total"].(string) != "10" {
		t.Fatalf("effective total should be the adopted record's 10, got %v", row["total"])
	}

	// The answer's layer feed (GET /api/answers/{id}) carries the overlay.
	var answerID int64
	if err := f.re.st.Pool.QueryRow(t.Context(),
		`SELECT a.id FROM answers a JOIN regrade_requests rr ON rr.student_id = a.student_id
		 WHERE rr.id = $1 AND a.problem_id = $2`, reqID, f.pid).Scan(&answerID); err != nil {
		t.Fatal(err)
	}
	ansResp := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/answers/%d", f.re.ts.URL, answerID), http.StatusOK)
	layers := ansResp["regrade_layers"].([]any)
	if len(layers) != 1 {
		t.Fatalf("expected 1 regrade layer, got %d", len(layers))
	}
	l0 := layers[0].(map[string]any)
	if l0["verdict"] != "regraded" || l0["adopted_total"] != "10" || int(l0["turn"].(float64)) != 1 {
		t.Fatalf("layer shape wrong: %v", l0)
	}

	// Flipping to upheld clears the adoption — totals fall back to round 0.
	r3 := patch(map[string]any{"outcome": "upheld", "note": "never mind"})
	r3.Body.Close()
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("upheld flip: got %d", r3.StatusCode)
	}
	totals = getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/assessments/%d/totals", f.re.ts.URL, f.aid), http.StatusOK)
	row = totals["students"].([]any)[0].(map[string]any)
	if row["total"] == nil || row["total"].(string) != "8.5" {
		t.Fatalf("upheld should fall back to the round-0 official 8.5, got %v", row["total"])
	}
}

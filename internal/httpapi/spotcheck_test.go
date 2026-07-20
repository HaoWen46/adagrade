package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/HaoWen46/adagrade/internal/store/db"
)

// previewGate reads the publish preview and returns its final_source object
// (nil-safe map) plus the publishable flag — the spot-check gate's home since
// its 0027 relocation from accept-official to publish.
func previewGate(t *testing.T, c *http.Client, baseURL string, aid int64) (map[string]any, bool) {
	t.Helper()
	pv := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", baseURL, aid), http.StatusOK)
	fs, _ := pv["final_source"].(map[string]any)
	publishable, _ := pv["publishable"].(bool)
	return fs, publishable
}

// TestSpotCheck_GateBlocksThenOpensOnAllVerdicted drives a full run to
// completion, confirms the publish preview reports the gate closed (trust spec
// §4, D37, relocated in 0027 from accept-official to the publish gate),
// verdicts every sample, and confirms the gate then opens.
func TestSpotCheck_GateBlocksThenOpensOnAllVerdicted(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	// A fresh completed run (2 graded leaves, below the floor) samples all of
	// them: total=2, done=0, not waived.
	sc := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", env.ts.URL, runID), http.StatusOK)
	state := sc["state"].(map[string]any)
	if state["total"].(float64) != 2 || state["done"].(float64) != 0 || state["waived"].(bool) {
		t.Fatalf("spot-check state: %v", state)
	}
	samples, _ := sc["samples"].([]any)
	if len(samples) != 2 {
		t.Fatalf("expected 2 sampled records, got %d", len(samples))
	}

	// Choose this method as the final source: coverage derives (both answers
	// official), so the ONLY thing holding publish is the spot-check gate.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	fs, publishable := previewGate(t, c, env.ts.URL, aid)
	if publishable || fs == nil || fs["spot_check_open"] == true {
		t.Fatalf("gate should block publish before any verdict: publishable=%v final_source=%v", publishable, fs)
	}
	if fs["spot_check_total"].(float64) != 2 || fs["spot_check_done"] != nil {
		t.Fatalf("preview gate state: want total=2 done=0(omitted), got %v", fs)
	}

	// Verdict one of two: gate still blocked (done < total).
	first := samples[0].(map[string]any)
	postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/%d", env.ts.URL, runID, int64(first["id"].(float64))),
		map[string]any{"verdict": "agree", "note": "matches my read"}, http.StatusOK)
	fs, publishable = previewGate(t, c, env.ts.URL, aid)
	if publishable || fs["spot_check_open"] == true || fs["spot_check_done"].(float64) != 1 {
		t.Fatalf("gate should still block after 1/2 verdicted: publishable=%v final_source=%v", publishable, fs)
	}

	// Verdict the second (as "adjusted", the other allowed verdict): gate opens.
	second := samples[1].(map[string]any)
	postExpect(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/%d", env.ts.URL, runID, int64(second["id"].(float64))),
		map[string]any{"verdict": "adjusted", "note": "score was off by one"}, http.StatusOK)

	scAfter := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", env.ts.URL, runID), http.StatusOK)
	stateAfter := scAfter["state"].(map[string]any)
	if stateAfter["done"].(float64) != 2 {
		t.Fatalf("expected done=2 after verdicting both, got %v", stateAfter)
	}

	fs, publishable = previewGate(t, c, env.ts.URL, aid)
	if !publishable || fs["spot_check_open"] != true || fs["spot_check_done"].(float64) != 2 {
		t.Fatalf("gate should open once all samples are verdicted: publishable=%v final_source=%v", publishable, fs)
	}
}

// TestSpotCheck_RejectsInvalidVerdict pins the verdict enum: only agree|adjusted.
func TestSpotCheck_RejectsInvalidVerdict(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	sc := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", env.ts.URL, runID), http.StatusOK)
	samples := sc["samples"].([]any)
	id := int64(samples[0].(map[string]any)["id"].(float64))

	resp := postJSON(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/%d", env.ts.URL, runID, id), map[string]any{"verdict": "disagree"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid verdict: got %d want 400", resp.StatusCode)
	}
}

// TestSpotCheck_WaiveOpensGateAndAudits: an admin can waive without any sample
// being checked; the reason is required and the action is audited
// (run.spotcheck.waive, trust spec §4).
func TestSpotCheck_WaiveOpensGateAndAudits(t *testing.T) {
	env, c, aid, _, _ := phase4Setup(t)
	admin := loginAs(t, env.ts, env.st, "admin@ntu.edu.tw", "admin")
	methodID := createFakeMethod(t, env, c)
	acceptAllMasks(t, env, c, aid)

	run := postExpect(t, c, env.ts.URL+"/api/runs", map[string]any{
		"assessment_id": aid, "scope_kind": "assessment", "method_id": methodID,
	}, http.StatusCreated)
	runID := int64(run["id"].(float64))
	driveRun(t, env, runID, false)

	// Anchor the gate: this method is the exam's final source, so the unreviewed
	// sample is what blocks publish until the waive below (0027 relocation).
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", env.ts.URL, aid),
		map[string]any{"kind": "method", "run_id": runID}, http.StatusOK)
	if _, publishable := previewGate(t, c, env.ts.URL, aid); publishable {
		t.Fatal("unreviewed sample should block publish before the waive")
	}

	// Non-admin (lecturer, the default phase4Setup login) cannot waive.
	resp := postJSON(t, c, fmt.Sprintf("%s/api/runs/%d/spot-check/waive", env.ts.URL, runID), map[string]any{"reason": "trust the process"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lecturer waive: got %d want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin without a reason is rejected.
	resp2 := postJSON(t, admin, fmt.Sprintf("%s/api/runs/%d/spot-check/waive", env.ts.URL, runID), map[string]any{"reason": ""})
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("waive without reason: got %d want 400", resp2.StatusCode)
	}
	resp2.Body.Close()

	// Admin with a reason opens the gate immediately, without any verdicts.
	postExpect(t, admin, fmt.Sprintf("%s/api/runs/%d/spot-check/waive", env.ts.URL, runID), map[string]any{"reason": "spot-checked out of band"}, http.StatusOK)

	sc := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/runs/%d/spot-check", env.ts.URL, runID), http.StatusOK)
	state := sc["state"].(map[string]any)
	if !state["waived"].(bool) {
		t.Fatalf("expected waived=true, got %v", state)
	}

	fs, publishable := previewGate(t, c, env.ts.URL, aid)
	if !publishable || fs["spot_check_waived"] != true || fs["spot_check_open"] != true {
		t.Fatalf("waive should open the publish gate without verdicts: publishable=%v final_source=%v", publishable, fs)
	}

	// Audited: an audit row exists for the waive action (trust spec §4:
	// run.spotcheck.waive).
	audits, err := env.st.Q.ListAuditForTarget(context.Background(), db.ListAuditForTargetParams{
		TargetKind: "run", TargetID: strconv.FormatInt(runID, 10), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "run.spotcheck.waive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a run.spotcheck.waive audit row, got actions: %v", auditActions(audits))
	}
}

func auditActions(rows []db.AuditLog) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}

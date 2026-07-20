package httpapi

import (
	"fmt"
	"net/http"
	"testing"
)

// TestVerdict_LockedAfterResultSent is the BUG 4(a) regression: handlePatchRegradeProblem
// never checked request status, so a TA could flip a verdict (or swap the adopted record)
// AFTER the result email went out — silently changing the student's exported grade to a
// value they were never told, with no correction path (send/resend both 409). After the
// fix, a verdict PATCH on a resolved request 409s and the stored verdict is untouched.
func TestVerdict_LockedAfterResultSent(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-lock-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-lock@ntu.edu.tw", "ta")

	// Adjudicate upheld, then send the result → the request resolves.
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld", "note": "reviewed"}, http.StatusOK)
	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	// A verdict change AFTER the result email must 409 — the student was already told.
	resp := patchJSONRaw(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "regraded"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("verdict change after result sent: got %d want 409", resp.StatusCode)
	}

	// And the persisted verdict is untouched: still upheld, no adopted record leaked in.
	sub, err := f.re.st.GetRequestProblem(t.Context(), subID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Verdict.String != "upheld" || sub.AdoptedRecordID.Valid {
		t.Fatalf("locked sub-item mutated after result sent: verdict=%q adopted=%+v", sub.Verdict.String, sub.AdoptedRecordID)
	}
}

// TestVerdict_RegradedPersistsVerdictAndAdoptionTogether is the BUG 4(b) regression: the
// handler used to write the verdict and the adopted record as two separate statements
// (a failure between them could strand a verdict without its adoption, or an upheld flip
// with a stale adopted record). The atomic single-statement write must persist BOTH the
// 'regraded' verdict and the adopted record id from one PATCH.
func TestVerdict_RegradedPersistsVerdictAndAdoptionTogether(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-atomic-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-atomic@ntu.edu.tw", "ta")

	recID := seedGradingRecordForSubItem(t, f, id, subID)
	mustExec(t, f.re.st, `UPDATE grading_records SET total = 9 WHERE id = $1`, recID)
	mustExec(t, f.re.st, `UPDATE regrade_request_problems SET ai_record_id = $2 WHERE id = $1`, subID, recID)

	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "regraded", "note": "point restored"}, http.StatusOK)

	sub, err := f.re.st.GetRequestProblem(t.Context(), subID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Verdict.String != "regraded" {
		t.Fatalf("verdict = %q, want regraded", sub.Verdict.String)
	}
	if !sub.AdoptedRecordID.Valid || sub.AdoptedRecordID.Int64 != recID {
		t.Fatalf("adopted record not persisted atomically with the verdict: got %+v, want %d", sub.AdoptedRecordID, recID)
	}
}

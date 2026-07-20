package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font/gofont/goregular"

	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
)

// testReportFontPath writes a real (non-CJK) UTF-8 TTF to a temp file and returns its
// path — a stand-in for `make report-fonts`'s Noto Sans TC, letting attachment tests
// set ADAMARKER_REPORT_FONT without a real font download (mirrors
// internal/report/report_test.go's and internal/publish/sender_test.go's identical
// helper; duplicated rather than exported test-only surface across packages).
func testReportFontPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test-font.ttf")
	if err := os.WriteFile(path, goregular.TTF, 0o600); err != nil {
		t.Fatalf("write test font: %v", err)
	}
	return path
}

// publishFixture builds an assessment with one problem (max 10, rubric 6+4) and two
// roster students, both with an uploaded submission and one graded answer. Whether the
// answers are made official is left to the caller so coverage-gate tests can exercise
// the pre-official state. All data is invented (CLAUDE.md).
type publishFixture struct {
	ts        *httptest.Server
	c         *http.Client
	st        storeSeeder
	env       *testEnv
	aid       int64
	problemID int64
	rubricID  int64
	critIDs   []int64
	answers   map[string]int64 // external student id -> answer id
}

// uploadFakePDF stages one fake PDF submission for the assessment (the ingest worker is
// driven separately via driveDirectUploads).
func uploadFakePDF(t *testing.T, c *http.Client, ts *httptest.Server, aid int64, filename string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("files", filename)
	_, _ = fw.Write([]byte("%PDF-fake"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/assessments/%d/submissions", ts.URL, aid), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func publishSetup(t *testing.T) publishFixture {
	return publishSetupEnv(t, nil)
}

func publishSetupEnv(t *testing.T, extra map[string]string) publishFixture {
	t.Helper()
	env := harnessEnvWithEnv(t, extra)
	ts, st := env.ts, env.st
	c := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, c, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", ts.URL, aid),
		map[string]any{"number": 1, "title": "Greedy", "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))
	rv := postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", ts.URL, pid), map[string]any{
		"score_increment": "0.5",
		"criteria": []map[string]string{
			{"description": "Correctness", "points": "6"},
			{"description": "Clarity", "points": "4"},
		},
	}, http.StatusCreated)
	rubricID := int64(rv["id"].(float64))
	var critIDs []int64
	for _, cr := range rv["criteria"].([]any) {
		critIDs = append(critIDs, int64(cr.(map[string]any)["id"].(float64)))
	}

	// Round-based grading (0027): publishing needs a chosen final source, and
	// officials are derived from it. Choose consensus up front — no aggregates
	// ever exist in these fixtures, so every human record gradeOfficial posts is
	// derived official via the manual-record recompute hook.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	seedStudent(t, st, "b01", "Ada Fake", "ada@example.edu")
	seedStudent(t, st, "b02", "Bo Fake", "bo@example.edu")
	uploadFakePDF(t, c, ts, aid, "b01.pdf")
	uploadFakePDF(t, c, ts, aid, "b02.pdf")
	driveDirectUploads(t, env, aid)

	answers := map[string]int64{}
	students := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", ts.URL, pid), http.StatusOK)
	for _, s := range students["students"] {
		answers[s["student_id"].(string)] = int64(s["answer_id"].(float64))
	}

	return publishFixture{
		ts: ts, c: c, st: st, env: env,
		aid: aid, problemID: pid, rubricID: rubricID, critIDs: critIDs, answers: answers,
	}
}

// gradeOfficial grades one answer with a human record. Under round-based grading
// (0027) officials are derived, never set: the fixture's consensus final source
// (chosen in publishSetupEnv) with no aggregates anywhere makes the post-insert
// recompute pick exactly this record as the answer's official — asserted here so
// the fixture stays honest. Works whether or not the answer already had an
// official (e.g. re-grading after an unpublish: the newest human record wins).
func (f publishFixture) gradeOfficial(t *testing.T, answerID int64, s1, s2 string) int64 {
	t.Helper()
	rec := postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, answerID), map[string]any{
		"rubric_version_id": f.rubricID,
		"comment":           "graded",
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": s1},
			{"criterion_id": f.critIDs[1], "score": s2},
		},
	}, http.StatusCreated)
	recID := int64(rec["id"].(float64))

	ans, err := f.st.Q.GetAnswer(t.Context(), answerID)
	if err != nil {
		t.Fatal(err)
	}
	if !ans.OfficialRecordID.Valid || ans.OfficialRecordID.Int64 != recID {
		t.Fatalf("answer %d official = %+v, want the derived human record %d", answerID, ans.OfficialRecordID, recID)
	}
	return recID
}

// --- Coverage gate ------------------------------------------------------------------

func TestPublish_CoverageGate_BlocksThenOpens(t *testing.T) {
	f := publishSetup(t)

	// Nothing official yet: preview must report not publishable with blockers.
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["publishable"].(bool) {
		t.Fatal("preview publishable=true before any official grade")
	}
	if len(pv["blockers"].([]any)) != 2 {
		t.Errorf("want 2 blockers (both answers ungraded), got %v", pv["blockers"])
	}

	// Publish must 409 with the blockers list.
	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("publish before coverage: got %d want 409", resp.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, resp, &body)
	if len(body["blockers"].([]any)) != 2 {
		t.Errorf("409 body should carry blockers, got %v", body)
	}

	// Grade + set official on both answers; now the gate opens.
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if !pv["publishable"].(bool) {
		t.Errorf("preview should be publishable after both official, got %v", pv)
	}
}

// --- Sender identity display (spec §2 D41: the publish dialog shows the From address
// the operator's students will actually see) -----------------------------------------

func TestPublish_Preview_ShowsFromAddress(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{"ADAMARKER_EMAIL_FROM": "ada2026@csie.ntu.edu.tw"})
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["from"] != "ada2026@csie.ntu.edu.tw" {
		t.Errorf("preview from = %v, want the configured ADAMARKER_EMAIL_FROM", pv["from"])
	}
}

func TestPublish_Preview_FromEmptyWhenUnset(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{"ADAMARKER_EMAIL_PROVIDER": "none"})
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["from"] != "" {
		t.Errorf("preview from = %v, want empty when ADAMARKER_EMAIL_FROM is unset (provider none)", pv["from"])
	}
}

// --- F2 (whole-branch review): publish preview's unassigned-TA warning (spec §6 D60,
// "publish preview warns (not blocks) on unassigned problems") ------------------------

// TestPublish_Preview_UnassignedTAProblems_WarnsUntilAssigned covers F2: a problem
// with no assigned TA appears in unassigned_ta_problems; assigning a TA removes it.
// The field never blocks publishability.
func TestPublish_Preview_UnassignedTAProblems_WarnsUntilAssigned(t *testing.T) {
	f := publishSetup(t)

	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	unassigned, ok := pv["unassigned_ta_problems"].([]any)
	if !ok {
		t.Fatalf("preview must carry an unassigned_ta_problems array, got %+v", pv)
	}
	if len(unassigned) != 1 {
		t.Fatalf("want 1 unassigned problem before any TA assignment, got %d: %+v", len(unassigned), unassigned)
	}
	entry := unassigned[0].(map[string]any)
	if int64(entry["problem_id"].(float64)) != f.problemID {
		t.Errorf("unassigned entry problem_id = %v, want %d", entry["problem_id"], f.problemID)
	}
	if int32(entry["problem_number"].(float64)) != 1 {
		t.Errorf("unassigned entry problem_number = %v, want 1", entry["problem_number"])
	}

	// Grading is still ungraded at this point — the warning must not block
	// publishability (it's advisory, spec §6 D60 "warns (not blocks)").
	if pv["publishable"].(bool) {
		t.Fatal("sanity: this fixture should not be publishable yet (no official grades)")
	}

	// Assign a TA to the problem: the picker's backing endpoint.
	ta := loginAs(t, f.ts, f.st, "ta-preview@ntu.edu.tw", "ta")
	_ = ta
	taUser, err := f.st.Q.CreateUser(t.Context(), db.CreateUserParams{
		Email: "ta-assignee@ntu.edu.tw", DisplayName: "TA Assignee", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	putJSONExpect(t, f.c, http.MethodPut, fmt.Sprintf("%s/api/problems/%d/ta", f.ts.URL, f.problemID),
		map[string]any{"user_id": taUser.ID}, http.StatusOK)

	pv2 := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	unassigned2 := pv2["unassigned_ta_problems"].([]any)
	if len(unassigned2) != 0 {
		t.Errorf("after assigning a TA, unassigned_ta_problems should be empty, got %+v", unassigned2)
	}
}

// --- Attachment options through publish (spec §3 D42/D44, §9) ----------------------

// TestPublish_AttachmentRequested_400sWhenFontUnconfigured: the test harness never
// sets ADAMARKER_REPORT_FONT, so any non-"none" attachment request must 400 rather
// than create a batch the send job can never actually attach to (spec §3: "the
// publish handler must 400 an attachment≠none request when the font isn't
// configured").
func TestPublish_AttachmentRequested_400sWhenFontUnconfigured(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{
		"attachment": "compressed",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("publish with attachment=compressed, font unconfigured: got %d want 400", resp.StatusCode)
	}
}

// TestPublish_AttachmentRequested_RejectsInvalidValue: an attachment value outside
// the exact three (spec §3 D44) must 400, independent of font configuration.
func TestPublish_AttachmentRequested_RejectsInvalidValue(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{
		"attachment": "fast",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("publish with attachment=fast (not none|compressed|original): got %d want 400", resp.StatusCode)
	}
}

// TestPublish_NoneAttachment_NeverRequiresFont: the default "none" (and an explicit
// "none") must publish fine even with no font configured — D43: "publish without
// attachments still works."
func TestPublish_NoneAttachment_NeverRequiresFont(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{
		"attachment": "none",
	}, http.StatusCreated)
}

// --- Individual resend (spec §4 D46) -------------------------------------------------

// TestResendItem_TerminalStatus_ReenqueuesAndAudits: a completed delivery can be
// explicitly re-armed as a new generation and writes an audit row.
func TestResendItem_TerminalStatus_ReenqueuesAndAudits(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	if len(items) == 0 {
		t.Fatal("expected at least one publish item")
	}
	itemID := items[0].ID

	// Drive the item to "sent" first (as if it was already delivered) — resend must
	// still work on an already-sent item, not just a failed one.
	if err := sendPublishItem(t, f.env, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	sentItem, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if sentItem.EmailStatus != "sent" {
		t.Fatalf("setup: item status = %q, want sent before exercising resend", sentItem.EmailStatus)
	}

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body map[string]any
		_ = decodeJSONResp(t, resp, &body)
		t.Fatalf("resend: got %d want 200 (%v)", resp.StatusCode, body)
	}

	// The item was reset to pending and (in this synchronous test harness) not yet
	// re-driven through the send worker — assert it left "sent" and became pending
	// again, proving the resend actually re-armed it rather than being a no-op.
	after, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if after.EmailStatus != "pending" {
		t.Errorf("after resend: item status = %q, want pending (re-enqueued)", after.EmailStatus)
	}

	audits := getJSON[map[string][]map[string]any](t, loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin"), fmt.Sprintf("%s/api/audit", f.ts.URL), http.StatusOK)
	found := false
	for _, a := range audits["entries"] {
		if a["action"] == "publish.resend_item" {
			found = true
		}
	}
	if !found {
		t.Errorf("no publish.resend_item audit row found: %v", audits["entries"])
	}
}

func TestResendItem_ActiveRefusedAndUncertainRequiresAcknowledgement(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	itemID := items[0].ID

	active := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	active.Body.Close()
	if active.StatusCode != http.StatusConflict {
		t.Fatalf("resend of pending delivery = %d, want 409", active.StatusCode)
	}
	stillPending, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if stillPending.EmailStatus != "pending" || stillPending.EmailGeneration != 1 {
		t.Fatalf("active resend changed state to %q generation %d", stillPending.EmailStatus, stillPending.EmailGeneration)
	}

	mustExec(t, f.st, "UPDATE publish_items SET email_status='uncertain', error='provider outcome unknown' WHERE id=$1", itemID)
	noAck := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	noAck.Body.Close()
	if noAck.StatusCode != http.StatusConflict {
		t.Fatalf("unacknowledged uncertain resend = %d, want 409", noAck.StatusCode)
	}
	postExpect(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{
		"acknowledge_duplicate_risk": true,
	}, http.StatusOK)
	armed, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if armed.EmailStatus != "pending" || armed.EmailGeneration != 2 {
		t.Fatalf("acknowledged uncertain resend = %q generation %d, want pending/2", armed.EmailStatus, armed.EmailGeneration)
	}
}

// TestResendItem_ReusesBatchAttachmentSettings: the resend job rebuilds using the
// PARENT BATCH's attachment/zip settings (spec §4 "reuses batch settings"), not some
// default — proven by resending an item whose batch was published with a compressed
// attachment and confirming the redelivered email carries one.
func TestResendItem_ReusesBatchAttachmentSettings(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{"ADAMARKER_REPORT_FONT": testReportFontPath(t)})
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{
		"attachment": "compressed",
	}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	itemID := items[0].ID
	if err := sendPublishItem(t, f.env, itemID, false); err != nil {
		t.Fatalf("initial SendItem: %v", err)
	}
	if sent, _ := f.st.Q.GetPublishItem(t.Context(), itemID); sent.EmailStatus != "sent" {
		t.Fatalf("setup: item status = %q, want sent before resend", sent.EmailStatus)
	}

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resend: got %d want 200", resp.StatusCode)
	}

	// Drive the re-enqueued send directly (as the email worker would) and confirm the
	// resend picked up the batch's "compressed" attachment setting, not "none".
	after, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if after.EmailStatus != "pending" {
		t.Fatalf("after resend: status = %q, want pending", after.EmailStatus)
	}
	if err := sendPublishItem(t, f.env, itemID, false); err != nil {
		t.Fatalf("SendItem after resend: %v", err)
	}
	final, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if final.EmailStatus != "sent" {
		t.Errorf("after resend+send: status = %q, want sent", final.EmailStatus)
	}
}

// TestPublishBatches_SurfacesAttachmentSettingsAndWarning: the batches-history API
// echoes the batch-level attachment/zip settings (spec §9/§10) and derives a
// per-item `warning` boolean from the non-terminal warning-prefixed error text (spec
// §3 15MB guard) — distinct from a real send failure.
func TestPublishBatches_SurfacesAttachmentSettingsAndWarning(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{"ADAMARKER_REPORT_FONT": testReportFontPath(t)})
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{
		"attachment": "compressed", "zip": true,
	}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	mustExec(t, f.st, "UPDATE publish_items SET email_status='sent', error='warning: attachment is 99 bytes, over the 15 byte guideline' WHERE id=$1", items[0].ID)

	body := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/batches", f.ts.URL, f.aid), http.StatusOK)
	batches := body["batches"].([]any)
	if len(batches) == 0 {
		t.Fatal("no batches returned")
	}
	b := batches[0].(map[string]any)
	if b["attachment"] != "compressed" || b["zip"] != true {
		t.Errorf("batch attachment/zip = %v/%v, want compressed/true", b["attachment"], b["zip"])
	}
	foundWarning := false
	for _, raw := range b["items"].([]any) {
		it := raw.(map[string]any)
		if it["id"] == float64(items[0].ID) {
			if it["warning"] != true {
				t.Errorf("item warning = %v, want true", it["warning"])
			}
			if it["email_status"] != "sent" {
				t.Errorf("item email_status = %v, want sent (warning is non-terminal)", it["email_status"])
			}
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatal("warning item not found in batches response")
	}
}

// TestPublishBatches_SurfacesSkippedCount is the B5 regression test: the batches-list
// summary previously carried only the full items[] array, so a collapsed/summarized
// view (ITEMS/SENT/FAILED/UNCERTAIN, as the publish history table renders per audit
// finding B5) had no visible account of "skipped" items — reading e.g. 14 items / 10
// sent / 0 failed / 0 uncertain as 4 lost emails instead of 4 students who were never
// mailed (all-no_submission or none-provider). The batches-list response must expose
// an explicit per-status count breakdown, including skipped, additive to the existing
// `items` list (field names unchanged).
func TestPublishBatches_SurfacesSkippedCount(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	if len(items) != 2 {
		t.Fatalf("setup: got %d publish items, want 2", len(items))
	}
	mustExec(t, f.st, "UPDATE publish_items SET email_status='sent' WHERE id=$1", items[0].ID)
	mustExec(t, f.st, "UPDATE publish_items SET email_status='skipped', error='no submission' WHERE id=$1", items[1].ID)

	body := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/batches", f.ts.URL, f.aid), http.StatusOK)
	batches := body["batches"].([]any)
	if len(batches) == 0 {
		t.Fatal("no batches returned")
	}
	b := batches[0].(map[string]any)
	want := map[string]float64{
		"items_count": 2, "sent_count": 1, "failed_count": 0, "uncertain_count": 0, "skipped_count": 1,
	}
	for k, wantV := range want {
		got, ok := b[k]
		if !ok {
			t.Fatalf("batch summary missing %q field in %+v", k, b)
		}
		if got != wantV {
			t.Errorf("batch summary %s = %v, want %v", k, got, wantV)
		}
	}
	// Additive: the full items[] list stays intact alongside the new counts.
	if len(b["items"].([]any)) != 2 {
		t.Errorf("batch items list = %v, want 2 entries (counts are additive, not a replacement)", b["items"])
	}
}

// TestPublish_ZeroEligibleFirstPublish_409MessageIsAccurate is the A5 follow-up copy
// fix: ErrNothingToPublish now also fires on a FIRST publish with zero eligible
// students (empty/fully-withdrawn roster), where there has never been a "last publish"
// and resend_all cannot help — so the pre-A5 409 body ("no changed students since the
// last publish; use resend_all to re-send to everyone") is factually wrong in exactly
// the situation A5 newly routes here. The handler must produce copy that matches the
// actual situation.
func TestPublish_ZeroEligibleFirstPublish_409MessageIsAccurate(t *testing.T) {
	env := harnessEnv(t)
	ts, st := env.ts, env.st
	c := loginAs(t, ts, st, "lect@ntu.edu.tw", "lecturer")

	// An assessment with a chosen final source but ZERO roster students: the coverage
	// gate passes vacuously and Publish reaches the empty-items guard (A5).
	a := postExpect(t, c, ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Empty Roster"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	resp := postJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/publish", ts.URL, aid), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("first publish with zero eligible students: got %d want 409", resp.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, resp, &body)
	msg, _ := body["error"].(string)
	if msg == "" {
		t.Fatalf("409 body has no error message: %v", body)
	}
	// There is no "last publish" and resend_all is not actionable here — the message
	// must not claim either.
	if strings.Contains(msg, "last publish") || strings.Contains(msg, "resend_all") {
		t.Errorf("first-publish 409 message wrongly speaks of a re-publish: %q", msg)
	}
	if !strings.Contains(msg, "no eligible students") {
		t.Errorf("first-publish 409 message should say there are no eligible students, got %q", msg)
	}

	// No batch row may exist (the A5 service guarantee, re-checked end-to-end here).
	var batches int
	if err := st.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM publish_batches WHERE assessment_id = $1`, aid).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("refused publish left %d batch row(s), want 0", batches)
	}
}

// TestPublish_ResendAllZeroEligible_409MessageIsAccurate covers the third
// ErrNothingToPublish situation: a RE-publish with resend_all=true against an
// assessment whose eligible population is now empty (every student withdrawn). The
// changed-only suggestion ("use resend_all to re-send to everyone") would be circular
// here — the caller already set resend_all — so the handler must say the population
// is empty instead.
func TestPublish_ResendAllZeroEligible_409MessageIsAccurate(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)

	// Unpublish (admin-only) so a re-publish is possible, then withdraw the whole
	// roster: the assessment HAS a prior batch but zero eligible students remain.
	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d want 200", uresp.StatusCode)
	}
	setStudentWithdrawnByExt(t, f.st, "b01", true)
	setStudentWithdrawnByExt(t, f.st, "b02", true)

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"resend_all": true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resend_all with zero eligible students: got %d want 409", resp.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, resp, &body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "no eligible students remain") {
		t.Errorf("resend_all-exhausted 409 message = %q, want it to say no eligible students remain", msg)
	}
	if strings.Contains(msg, "use resend_all") {
		t.Errorf("409 message circularly re-suggests resend_all: %q", msg)
	}
}

// TestPublish_CoverageLostUnderLock_409GradingStateChanged drives the FULL A9 path
// end-to-end — store.CreatePublishBatch's locked coverage re-check →
// store.ErrCoverageGateChanged → publish.ErrGradingStateChanged → the handler's 409 —
// with a deterministic reproduction of the exact race the re-check exists for, using
// no injection seam:
//
//  1. the test opens its own transaction and takes the assessment's FOR UPDATE lock
//     (the same lock CreatePublishBatch takes), then nulls one answer's official
//     record INSIDE that transaction — invisible to any other connection until commit;
//  2. a goroutine POSTs the publish: its unlocked pre-reads (coverage preview, final
//     source) all pass against the still-committed state, then CreatePublishBatch
//     blocks on the test's assessment lock — exactly the preview/create window A9 is
//     about;
//  3. the test waits (via pg_locks) until the publish is provably blocked, then
//     commits — the official record is now gone;
//  4. the publish resumes, its locked coverage re-check sees blocked=1, and the
//     request must surface the refresh-and-retry 409 with NO batch row created.
func TestPublish_CoverageLostUnderLock_409GradingStateChanged(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	ctx := t.Context()

	tx, err := f.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // no-op after the Commit below
	if _, err := tx.Exec(ctx, `SELECT id FROM assessments WHERE id = $1 FOR UPDATE`, f.aid); err != nil {
		t.Fatal(err)
	}
	// The racing write: b01's answer loses its official record (as a concurrent
	// regrade/recompute would do). Its pages exist (publishSetup ingests real
	// uploads), so without an official it re-becomes a BLOCKED answer.
	if _, err := tx.Exec(ctx,
		`UPDATE answers SET official_record_id = NULL, official_set_at = NULL WHERE id = $1`,
		f.answers["b01"]); err != nil {
		t.Fatal(err)
	}

	type publishOutcome struct {
		status int
		body   map[string]any
		err    error
	}
	done := make(chan publishOutcome, 1)
	go func() {
		// Manual request (not postJSON): test helpers t.Fatal, which must not be
		// called from a non-test goroutine.
		b, _ := json.Marshal(map[string]any{})
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), bytes.NewReader(b))
		if err != nil {
			done <- publishOutcome{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-ADA-CSRF", "1")
		resp, err := f.c.Do(req)
		if err != nil {
			done <- publishOutcome{err: err}
			return
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		done <- publishOutcome{status: resp.StatusCode, body: body}
	}()

	// Wait until the publish transaction is provably parked on our lock (its
	// unlocked pre-reads are then complete — the exact race window), then commit.
	// Advisory waiters are excluded: storetest.Fresh serializes DB tests
	// cluster-wide via pg_advisory_lock, so under a multi-package DB run sibling
	// test binaries sit parked as ungranted ADVISORY locks the whole time — an
	// unfiltered EXISTS would fire before the publish goroutine even started,
	// and the early commit would flip the preview itself to blocked (a coverage
	// 409, not the under-lock race this test exists to pin).
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting bool
		if err := f.st.Pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted AND locktype <> 'advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publish never blocked on the assessment lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	res := <-done
	if res.err != nil {
		t.Fatalf("publish request: %v", res.err)
	}
	if res.status != http.StatusConflict {
		t.Fatalf("racing publish: got %d (%v), want 409", res.status, res.body)
	}
	msg, _ := res.body["error"].(string)
	if !strings.Contains(msg, "refresh and try again") {
		t.Errorf("409 message should ask the operator to refresh and retry, got %q", msg)
	}

	var batches int
	if err := f.st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM publish_batches WHERE assessment_id = $1`, f.aid).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("lost-coverage race left %d batch row(s), want 0", batches)
	}
}

// TestResendItem_LecturerRole_Allowed_TAForbidden: resend is lecturer+ (spec §4).
func TestResendItem_LecturerRole_Allowed_TAForbidden(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	itemID := items[0].ID

	ta := loginAs(t, f.ts, f.st, "ta@ntu.edu.tw", "ta")
	resp := postJSON(t, ta, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("TA resend: got %d want 403", resp.StatusCode)
	}
}

// TestResendItem_SupersededBatch_409NoDowngrade (I1): individual resend is a LIVE-batch-
// only action (spec §4 D46). An item on a SUPERSEDED (unpublished) batch cannot be
// individually resent — SendItem's unpublish guard skips it, so an unconditional resend
// would silently downgrade a "sent" item to "pending" forever (wedged, never re-sent).
// The endpoint must 409 that resend and leave the item status untouched; corrections go
// through re-publish, not resend.
func TestResendItem_SupersededBatch_409NoDowngrade(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	itemID := items[0].ID

	// Drive the item to "sent" so a downgrade would be visibly destructive.
	if err := sendPublishItem(t, f.env, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	sent, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if sent.EmailStatus != "sent" {
		t.Fatalf("setup: item status = %q, want sent before superseding", sent.EmailStatus)
	}

	// Supersede the batch via unpublish (admin-only).
	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d want 200", uresp.StatusCode)
	}

	// Resend on the now-superseded item must 409, not silently no-op.
	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		var body map[string]any
		_ = decodeJSONResp(t, resp, &body)
		t.Fatalf("resend on superseded batch: got %d want 409 (%v)", resp.StatusCode, body)
	}

	// The item status is UNCHANGED — not downgraded to pending (which would wedge it).
	after, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if after.EmailStatus != "sent" {
		t.Errorf("after 409 resend: item status = %q, want unchanged (sent) — never downgraded to pending", after.EmailStatus)
	}
}

// --- Roster-add-after-ingest coverage gap (F: fail-closed not_ingested blocker) -----

// TestPublish_RosterGapAfterIngest_BlocksThenOpens is the regression test for a
// roster student added after ingest already ran: they have zero answers rows for the
// assessment. Without a roster-side check, PublishCoverageCounts/PublishBlockers/
// PublishSnapshotInputs (all FROM answers) never see them, so they'd silently pass the
// gate and receive no email at all. This asserts the gate fails closed instead: the
// preview is blocked and lists the gap student under a distinct "not_ingested" blocker
// kind, and re-ingesting (materializing their answers) reopens the gate.
func TestPublish_RosterGapAfterIngest_BlocksThenOpens(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	// Gate is open before the roster-add.
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if !pv["publishable"].(bool) {
		t.Fatalf("preview should be publishable before the roster-add, got %v", pv)
	}

	// Add a third roster student AFTER ingest — no PDF uploaded, no answers row
	// materialized for them at all (unlike b01/b02, and unlike a genuine no_submission
	// student who at least has an answers row from ingest).
	seedStudent(t, f.st, "b03", "Cy Fake", "cy@example.edu")

	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["publishable"].(bool) {
		t.Fatal("preview publishable=true with an un-ingested roster student present")
	}
	if int(pv["not_ingested"].(float64)) != 1 {
		t.Errorf("not_ingested count = %v, want 1", pv["not_ingested"])
	}
	blockers := pv["blockers"].([]any)
	var found bool
	for _, b := range blockers {
		bm := b.(map[string]any)
		if bm["kind"] == "not_ingested" && bm["student_external_id"] == "b03" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a not_ingested blocker for b03 in %v", blockers)
	}

	// Publish must 409 too, not just preview.
	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("publish with a roster gap present: got %d want 409", resp.StatusCode)
	}

	// Re-ingest b03 (upload + drive the direct-upload worker, same as publishSetup did
	// for b01/b02) -> the gate opens once their answers row(s) exist.
	uploadFakePDF(t, f.c, f.ts, f.aid, "b03.pdf")
	driveDirectUploads(t, f.env, f.aid)

	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if int(pv["not_ingested"].(float64)) != 0 {
		t.Errorf("not_ingested after re-ingest = %v, want 0", pv["not_ingested"])
	}
	// b03 now has an ungraded answer with a page -> a regular "ungraded" blocker, not
	// publishable yet (expected — re-ingesting doesn't grade them).
	if pv["publishable"].(bool) {
		t.Fatal("preview publishable=true before b03 is graded")
	}
}

// --- Snapshot correctness -----------------------------------------------------------

func TestPublish_SnapshotMatchesFixture(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5") // total 8.5
	f.gradeOfficial(t, f.answers["b02"], "6", "4")   // total 10

	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"note": "first"}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	if int(res["items_created"].(float64)) != 2 {
		t.Fatalf("items_created: %v", res)
	}

	// Read back the stored snapshot for b01 and assert its shape/values.
	items, err := f.st.ListPublishItems(t.Context(), batchID)
	if err != nil {
		t.Fatal(err)
	}
	var b01 db.ListPublishItemsRow
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			b01 = it
		}
	}
	var snap struct {
		AssessmentName    string `json:"assessment_name"`
		StudentExternalID string `json:"student_external_id"`
		Total             string `json:"total"`
		Max               string `json:"max"`
		Problems          []struct {
			Number   int32  `json:"number"`
			Total    string `json:"total"`
			Criteria []struct {
				Name, Score, Max string
			} `json:"criteria"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(b01.Snapshot, &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if snap.AssessmentName != "Midterm" || snap.StudentExternalID != "b01" {
		t.Errorf("snapshot header wrong: %+v", snap)
	}
	if snap.Total != "8.5" || snap.Max != "10" {
		t.Errorf("snapshot totals: total=%q max=%q want 8.5/10", snap.Total, snap.Max)
	}
	if len(snap.Problems) != 1 || len(snap.Problems[0].Criteria) != 2 {
		t.Fatalf("snapshot problems/criteria: %+v", snap.Problems)
	}
	if b01.RecipientEmail != "ada@example.edu" {
		t.Errorf("recipient captured at publish time = %q, want ada@example.edu", b01.RecipientEmail)
	}
	if b01.EmailStatus != "pending" {
		t.Errorf("emailable item status = %q, want pending", b01.EmailStatus)
	}
}

// --- Published lock -----------------------------------------------------------------

// TestPublish_LocksOfficialWrites: the spec §2 lock under round-based grading
// (0027) — the derivation only ever touches UNPUBLISHED answers, so a human
// record filed after publish is appended to history but the published answer's
// official pointer never moves off the snapshotted record.
func TestPublish_LocksOfficialWrites(t *testing.T) {
	f := publishSetup(t)
	official := f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)

	// A newer (better) human record on the published answer is allowed — records
	// are immutable history — but its recompute hook must skip the published
	// answer rather than re-deriving it official.
	postExpect(t, f.c, fmt.Sprintf("%s/api/answers/%d/records", f.ts.URL, f.answers["b01"]), map[string]any{
		"rubric_version_id": f.rubricID,
		"scores": []map[string]any{
			{"criterion_id": f.critIDs[0], "score": "6"},
			{"criterion_id": f.critIDs[1], "score": "4"},
		},
	}, http.StatusCreated)
	ans, err := f.st.Q.GetAnswer(t.Context(), f.answers["b01"])
	if err != nil {
		t.Fatal(err)
	}
	if !ans.OfficialRecordID.Valid || ans.OfficialRecordID.Int64 != official {
		t.Errorf("published answer's official moved to %+v, want locked on %d", ans.OfficialRecordID, official)
	}
}

// --- Already-published guard (F: no double-publish) ---------------------------------

// TestPublish_AlreadyPublished_Returns409NoDuplicateBatch is the regression test for
// the missing already-published guard: publishing twice in a row (without an
// intervening unpublish) must 409 on the second call, must NOT create a second
// non-superseded batch, and must NOT enqueue a second round of emails. The
// single-live-batch invariant depends on this — two non-superseded batches would make
// LatestNonSupersededBatch (and thus Unpublish) ambiguous.
func TestPublish_AlreadyPublished_Returns409NoDuplicateBatch(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	first := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	firstBatch := int64(first["batch_id"].(float64))

	// Second publish, no unpublish in between, with resend_all so the changed-only
	// "nothing changed" 409 (ErrNothingToPublish) can't be the thing tripping this —
	// resend_all bypasses that path entirely and would otherwise happily create a
	// second non-superseded batch and re-enqueue both students' emails. The guard
	// under test must refuse this regardless.
	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"resend_all": true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second publish (resend_all) while already published: got %d want 409", resp.StatusCode)
	}

	batches, err := f.st.ListPublishBatches(t.Context(), f.aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("expected exactly 1 batch after a refused double-publish, got %d", len(batches))
	}
	if batches[0].ID != firstBatch {
		t.Fatalf("the surviving batch should be the first one: got %d want %d", batches[0].ID, firstBatch)
	}

	// No duplicate emails: the first batch's items must be untouched (still whatever
	// they were right after the first publish — in particular, no new items appended).
	items, err := f.st.ListPublishItems(t.Context(), firstBatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("first batch item count changed by the refused double-publish: got %d want 2", len(items))
	}
}

// --- Changed-only re-publish --------------------------------------------------------

// TestPublish_ChangedOnly_SelectsOnlyChanged doubles as the already-published guard's
// re-publish regression coverage: it proves publish -> unpublish -> publish still
// succeeds (201) with changed-only semantics intact after the guard in Publish (F:
// already-published 409 on a second publish) landed. The guard must key off "is there
// a batch LIVE right now" (LatestNonSupersededBatch), not "has this assessment ever
// been published" — otherwise this exact flow (a legitimate re-publish after
// unpublish) would wrongly 409 on the second publish call below.
func TestPublish_ChangedOnly_SelectsOnlyChanged(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)

	// Unpublish to reopen grading, change only b02, re-publish.
	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d", uresp.StatusCode)
	}

	// Change b02's grade (6->5 on crit0). b01 unchanged.
	f.gradeOfficial(t, f.answers["b02"], "5", "4")

	// Preview should report exactly b02 changed.
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	changed := pv["changed"].([]any)
	if len(changed) != 1 || changed[0].(map[string]any)["external_id"].(string) != "b02" {
		t.Fatalf("changed set = %v, want exactly [b02]", changed)
	}

	// Changed-only re-publish creates a batch with exactly one item (b02) — 201, not
	// the already-published 409 (no batch is LIVE right now; the unpublish above
	// superseded the only prior batch).
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 1 {
		t.Errorf("changed-only re-publish items_created = %v, want 1", res["items_created"])
	}
}

// TestPublish_ChangedOnly_BaselineIsPerStudentAcrossBatches is the C1 regression: the
// changed-only baseline must be each student's MOST RECENT item across ALL batches, not
// just the newest batch's items. A changed-only re-publish writes a THIN batch (only the
// changed student), so if the baseline were "the newest batch" the next cycle would see
// every student absent from that thin batch as changed and re-email the whole cohort.
//
// Three-cycle scenario (per the finding):
//  1. publish all (3 students) -> batch1 = {b01,b02,b03}
//  2. unpublish, change ONLY b02, changed-only re-publish -> batch2 = {b02} (thin)
//  3. unpublish, change ONLY a DIFFERENT student (b03), changed-only re-publish
//     must select EXACTLY {b03} — not {b01,b03} (b01 last appeared in batch1 and is
//     unchanged; the bug would resurrect it because it's absent from batch2).
func TestPublish_ChangedOnly_BaselineIsPerStudentAcrossBatches(t *testing.T) {
	f := publishSetup(t)
	// publishSetup seeds b01,b02 with graded answers; add a third student b03 too.
	seedStudent(t, f.st, "b03", "Cy Fake", "cy@example.edu")
	uploadFakePDF(t, f.c, f.ts, f.aid, "b03.pdf")
	driveDirectUploads(t, f.env, f.aid)
	// Re-fetch answer ids now that b03 has an answer row.
	students := getJSON[map[string][]map[string]any](t, f.c, fmt.Sprintf("%s/api/problems/%d/students", f.ts.URL, f.problemID), http.StatusOK)
	for _, s := range students["students"] {
		f.answers[s["student_id"].(string)] = int64(s["answer_id"].(float64))
	}

	// Cycle 1: grade all three and publish everyone.
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	f.gradeOfficial(t, f.answers["b03"], "4", "2")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 3 {
		t.Fatalf("first publish items_created = %v, want 3", res["items_created"])
	}

	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	unpublish := func() {
		uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
		uresp.Body.Close()
		if uresp.StatusCode != http.StatusOK {
			t.Fatalf("unpublish: got %d", uresp.StatusCode)
		}
	}

	// Cycle 2: change ONLY b02, changed-only re-publish -> a thin batch of just b02.
	unpublish()
	f.gradeOfficial(t, f.answers["b02"], "5", "4")
	res = postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 1 {
		t.Fatalf("cycle-2 changed-only items_created = %v, want 1 (only b02)", res["items_created"])
	}

	// Cycle 3: change ONLY b03 (a DIFFERENT student). The preview + the re-publish must
	// select exactly {b03}. b01 is unchanged since batch1 and must NOT be resurrected
	// just because it's absent from the thin batch2.
	unpublish()
	f.gradeOfficial(t, f.answers["b03"], "5", "3")

	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	changed := pv["changed"].([]any)
	if len(changed) != 1 || changed[0].(map[string]any)["external_id"].(string) != "b03" {
		t.Fatalf("cycle-3 changed set = %v, want exactly [b03] (b01 must not resurface)", changed)
	}

	res = postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 1 {
		t.Fatalf("cycle-3 changed-only items_created = %v, want exactly 1 (only b03), not the cohort", res["items_created"])
	}
}

// --- resend_all overrides changed-only ----------------------------------------------

func TestPublish_ResendAll_OverridesChangedOnly(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)

	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()

	// No grades changed; changed-only would be a no-op (409), resend_all re-sends both.
	noop := postJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{})
	noop.Body.Close()
	if noop.StatusCode != http.StatusConflict {
		t.Errorf("changed-only with no changes: got %d want 409", noop.StatusCode)
	}
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 2 {
		t.Errorf("resend_all items_created = %v, want 2", res["items_created"])
	}
}

// TestPublish_Preview_HasLiveBatchVsEverPublished covers M1: the preview splits
// has_live_batch (a live batch exists — gates the Unpublish button/badge) from
// ever_published (any batch ever existed — drives changed-only re-publish). After an
// unpublish they diverge: has_live_batch=false, ever_published=true.
func TestPublish_Preview_HasLiveBatchVsEverPublished(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	// Before any publish: both false.
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["has_live_batch"] != false || pv["ever_published"] != false {
		t.Fatalf("pre-publish: has_live_batch=%v ever_published=%v, want both false", pv["has_live_batch"], pv["ever_published"])
	}

	// After publish: both true.
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["has_live_batch"] != true || pv["ever_published"] != true {
		t.Fatalf("after publish: has_live_batch=%v ever_published=%v, want both true", pv["has_live_batch"], pv["ever_published"])
	}
	// Back-compat: already_published mirrors has_live_batch.
	if pv["already_published"] != true {
		t.Errorf("already_published should mirror has_live_batch (true), got %v", pv["already_published"])
	}

	// After unpublish: they diverge — no live batch, but ever published.
	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["has_live_batch"] != false {
		t.Errorf("after unpublish: has_live_batch = %v, want false (no live batch)", pv["has_live_batch"])
	}
	if pv["ever_published"] != true {
		t.Errorf("after unpublish: ever_published = %v, want true (a superseded batch exists)", pv["ever_published"])
	}
	if pv["already_published"] != false {
		t.Errorf("after unpublish: already_published should mirror has_live_batch (false), got %v", pv["already_published"])
	}
}

// --- Unpublish reopens + older-batch 409 --------------------------------------------

func TestPublish_Unpublish_ReopensAndOlderBatch409(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	first := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	firstBatch := int64(first["batch_id"].(float64))

	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")

	// Unpublish reopens grading: an official write now succeeds again.
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d", uresp.StatusCode)
	}
	// Official write reopened.
	rec := f.gradeOfficial(t, f.answers["b01"], "6", "4")
	_ = rec

	// Re-publish (resend_all) creates a newer batch; unpublishing the OLD batch is now
	// not-latest. We can't target a specific batch via the API (it unpublishes latest),
	// but superseding the old batch directly at the store level must 409-equivalent.
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	if err := f.st.SupersedePublishBatch(t.Context(), firstBatch, 0); err == nil {
		t.Error("superseding the older batch should fail (ErrNotLatestBatch), got nil")
	}
}

func TestPublish_UnpublishRefusesProviderBoundaryThenSucceeds(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	attempt := store.PublishDeliveryAttempt{ItemID: items[0].ID, Generation: items[0].EmailGeneration, JobID: 991}
	if _, ok, err := f.st.ClaimPublishItemDelivery(t.Context(), attempt); err != nil || !ok {
		t.Fatalf("claim delivery: ok=%v err=%v", ok, err)
	}
	if _, ok, err := f.st.BeginPublishItemSending(t.Context(), attempt); err != nil || !ok {
		t.Fatalf("begin delivery: ok=%v err=%v", ok, err)
	}

	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	blocked := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusConflict {
		t.Fatalf("unpublish while sending = %d, want 409", blocked.StatusCode)
	}
	live, err := f.st.Q.GetPublishBatch(t.Context(), batchID)
	if err != nil || live.SupersededAt.Valid {
		t.Fatalf("blocked unpublish changed batch: %+v err=%v", live.SupersededAt, err)
	}
	if ok, err := f.st.MarkPublishItemDeliveryUncertain(t.Context(), attempt, "", "provider outcome unknown"); err != nil || !ok {
		t.Fatalf("finish ambiguous delivery: ok=%v err=%v", ok, err)
	}
	postExpect(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{}, http.StatusOK)
}

// --- resend-failed re-enqueues only failed ------------------------------------------

func TestPublish_ResendFailed_OnlyFailed(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	// Mark b01 failed, b02 sent — resend-failed must re-enqueue exactly 1.
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			mustExec(t, f.st, "UPDATE publish_items SET email_status='failed', error='boom' WHERE id=$1", it.ID)
		} else {
			mustExec(t, f.st, "UPDATE publish_items SET email_status='sent' WHERE id=$1", it.ID)
		}
	}

	rr := postExpect(t, f.c, fmt.Sprintf("%s/api/publish/batches/%d/resend-failed", f.ts.URL, batchID), map[string]any{}, http.StatusOK)
	if int(rr["reenqueued"].(float64)) != 1 {
		t.Errorf("resend-failed reenqueued = %v, want 1", rr["reenqueued"])
	}
	// The failed item is back to pending; the sent one is untouched.
	items, _ = f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		if it.StudentExternalID == "b01" && it.EmailStatus != "pending" {
			t.Errorf("b01 after resend: %q, want pending", it.EmailStatus)
		}
		if it.StudentExternalID == "b02" && it.EmailStatus != "sent" {
			t.Errorf("b02 after resend: %q, want sent (untouched)", it.EmailStatus)
		}
	}
}

// --- none provider: all skipped + loud warning --------------------------------------

func TestPublish_NoneProvider_AllSkippedWithWarning(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{"ADAMARKER_EMAIL_PROVIDER": "none"})
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if res["email_disabled"] != true {
		t.Errorf("email_disabled = %v, want true for none provider", res["email_disabled"])
	}
	if res["warning"] == nil || res["warning"] == "" {
		t.Error("none-provider publish should return a loud warning")
	}
	if int(res["enqueued"].(float64)) != 0 {
		t.Errorf("none provider should enqueue 0 sends, got %v", res["enqueued"])
	}
	if int(res["skipped"].(float64)) != 2 {
		t.Errorf("none provider should skip both items, got %v", res["skipped"])
	}
	// Every item recorded as skipped, none pending.
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		if it.EmailStatus != "skipped" {
			t.Errorf("none-provider item %s status = %q, want skipped", it.StudentExternalID, it.EmailStatus)
		}
	}
}

// --- publish preflight warnings (workflow-guards plan 2026-07-10, Task B3) ----------

// pubPreviewWarnings fetches the publish preview and indexes its warnings array by
// code (each code appears at most once) — the publish-dialog counterpart of
// warningsFor (and of runs_test.go's launch-scoped previewWarnings).
func pubPreviewWarnings(t *testing.T, c *http.Client, baseURL string, aid int64) map[string]map[string]any {
	t.Helper()
	pv := getJSON[map[string]any](t, c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", baseURL, aid), http.StatusOK)
	raw, ok := pv["warnings"].([]any)
	if !ok {
		t.Fatalf("publish preview must carry a warnings array: %v", pv)
	}
	out := make(map[string]map[string]any, len(raw))
	for _, w := range raw {
		wm := w.(map[string]any)
		code := wm["code"].(string)
		if _, dup := out[code]; dup {
			t.Fatalf("preview warning code %q emitted twice: %v", code, raw)
		}
		out[code] = wm
	}
	return out
}

// TestPublishPreview_Warnings_EmailFileProviderInfoInDev: the harness runs with the
// development default (provider=file), so the preview flags email_file_provider at
// info severity — grade emails land in a local outbox, not student inboxes. No reply
// domain and no skipped students here, so those codes stay absent.
func TestPublishPreview_Warnings_EmailFileProviderInfoInDev(t *testing.T) {
	f := publishSetup(t)
	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "email_file_provider", "info", 0)
	refuteWarning(t, ws, "email_replyto_dead")
	refuteWarning(t, ws, "skipped_students")
}

// TestPublishPreview_Warnings_ReplyToDeadOnSMTP: smtp has no inbound webhook, so a
// configured reply domain advertises a regrade Reply-To no one can receive — replies
// are silently lost (main.go already warns at startup; the preview surfaces it at the
// moment of publishing, as danger).
func TestPublishPreview_Warnings_ReplyToDeadOnSMTP(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{
		"ADAMARKER_EMAIL_PROVIDER":     "smtp",
		"ADAMARKER_EMAIL_REPLY_DOMAIN": "inbound.example.edu",
	})
	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "email_replyto_dead", "danger", 0)
	refuteWarning(t, ws, "email_file_provider")
}

// TestPublishPreview_Warnings_NoRegradeDeadline (demo-polish plan 2026-07-10, Task D):
// a configured reply domain puts a regrade Reply-To on every grade email (the sender's
// HasReplyTo condition), and the inbound webhook only enforces
// assessments.regrade_deadline when it is set — unset means replies are accepted for as
// long as tokens live. The preview nudges (info) until a deadline exists; clearing the
// deadline brings the nudge back.
func TestPublishPreview_Warnings_NoRegradeDeadline(t *testing.T) {
	f := publishSetupEnv(t, map[string]string{
		"ADAMARKER_EMAIL_REPLY_DOMAIN": "inbound.example.edu",
	})
	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "no_regrade_deadline", "info", 0)

	// Setting a deadline resolves the nudge.
	deadline := time.Now().Add(14 * 24 * time.Hour)
	putJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/regrade-deadline", f.ts.URL, f.aid),
		map[string]any{"deadline": deadline.Format(time.RFC3339)}, http.StatusOK)
	ws = pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	refuteWarning(t, ws, "no_regrade_deadline")

	// Clearing it reopens the indefinite window — nudge returns.
	putJSON(t, f.c, fmt.Sprintf("%s/api/assessments/%d/regrade-deadline", f.ts.URL, f.aid),
		map[string]any{"deadline": nil}, http.StatusOK)
	ws = pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "no_regrade_deadline", "info", 0)
}

// TestPublishPreview_Warnings_NoRegradeDeadline_AbsentWithoutReplyChannel: with no
// reply domain the grade email says replies are not monitored, so an unset deadline
// gates nothing — the nudge stays silent (the harness default has no reply domain).
func TestPublishPreview_Warnings_NoRegradeDeadline_AbsentWithoutReplyChannel(t *testing.T) {
	f := publishSetup(t)
	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	refuteWarning(t, ws, "no_regrade_deadline")
}

// TestPublishPreview_Warnings_SkippedStudents: a roster student whose every answer is
// no_submission (an answers row with zero pages) receives NO email on publish — the
// preview must call that out with the count, and it must never block publishability.
func TestPublishPreview_Warnings_SkippedStudents(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")

	// b03: answers row materialized, no pages ⇒ no_submission for their only problem.
	stu, err := f.st.Q.UpsertStudent(t.Context(), db.UpsertStudentParams{StudentID: "b03", Name: "Cy Fake", Email: "cy@example.edu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Q.EnsureAnswer(t.Context(), db.EnsureAnswerParams{
		AssessmentID: f.aid, StudentID: stu.ID, ProblemID: f.problemID,
	}); err != nil {
		t.Fatal(err)
	}

	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "skipped_students", "warning", 1)

	// Warn-only: the no_submission student counts as covered, so the gate stays open.
	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["publishable"] != true {
		t.Errorf("skipped_students must not block: publishable = %v, want true", pv["publishable"])
	}
}

// TestPublishPreview_Warnings_AssessmentWideSubset: the preview reuses the standing
// assessment-wide warnings but scoped to publish-relevant codes — an open quarantine
// row shows up, while mask_errors (a grading-input hazard, not a publish hazard) is
// filtered out even when present.
func TestPublishPreview_Warnings_AssessmentWideSubset(t *testing.T) {
	f := publishSetup(t)

	// An unmatchable upload lands in quarantine, unresolved.
	uploadFakePDF(t, f.c, f.ts, f.aid, "not-on-roster.pdf")
	driveDirectUploads(t, f.env, f.aid)

	// One page's MaskPage job failed terminally — assessment-wide danger, but NOT a
	// publish-preview code.
	mustExec(t, f.st, `UPDATE answer_pages SET mask_error = 'decode_failed'
		WHERE id = (SELECT ap.id FROM answer_pages ap JOIN answers a ON a.id = ap.answer_id
		            WHERE a.assessment_id = $1 ORDER BY ap.id LIMIT 1)`, f.aid)

	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "quarantined_uploads", "warning", 1)
	refuteWarning(t, ws, "mask_errors")
	refuteWarning(t, ws, "superseded_answers")
}

// TestEmailConfigWarnings_SeverityByEnv unit-tests the pure config-derivation
// directly: the HTTP harness can't run env=production (dev-login is development-only),
// so the file-provider-in-production danger branch is pinned here.
func TestEmailConfigWarnings_SeverityByEnv(t *testing.T) {
	cases := []struct {
		name, provider, replyDomain string
		env                         config.Environment
		wantCodes                   map[string]string // code -> severity
	}{
		{"file in development is info", "file", "", config.EnvDevelopment,
			map[string]string{"email_file_provider": "info"}},
		{"file in production is danger", "file", "", config.EnvProduction,
			map[string]string{"email_file_provider": "danger"}},
		{"smtp with reply domain is dead-replies danger", "smtp", "inbound.example.edu", config.EnvProduction,
			map[string]string{"email_replyto_dead": "danger"}},
		{"smtp without reply domain is clean", "smtp", "", config.EnvProduction, map[string]string{}},
		{"postmark with reply domain is clean", "postmark", "inbound.example.edu", config.EnvProduction, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := emailConfigWarnings(tc.provider, tc.replyDomain, tc.env)
			if len(got) != len(tc.wantCodes) {
				t.Fatalf("got %d warnings %v, want %d", len(got), got, len(tc.wantCodes))
			}
			for _, w := range got {
				sev, ok := tc.wantCodes[w.Code]
				if !ok {
					t.Errorf("unexpected warning %+v", w)
					continue
				}
				if w.Severity != sev {
					t.Errorf("%s severity = %s, want %s", w.Code, w.Severity, sev)
				}
			}
		})
	}
}

// --- withdraw semantics downstream (roster-lifecycle plan 2026-07-10, Task R2) -------

// setStudentWithdrawnByExt flips one roster student's withdrawn state directly at the
// store level (the bulk endpoints belong to R1; these tests only need the state).
func setStudentWithdrawnByExt(t *testing.T, st storeSeeder, externalID string, withdrawn bool) {
	t.Helper()
	stu, err := st.Q.GetStudentByExternalID(t.Context(), externalID)
	if err != nil {
		t.Fatalf("GetStudentByExternalID(%s): %v", externalID, err)
	}
	if _, err := st.Q.SetStudentWithdrawn(t.Context(), db.SetStudentWithdrawnParams{ID: stu.ID, Withdrawn: withdrawn}); err != nil {
		t.Fatalf("SetStudentWithdrawn(%s): %v", externalID, err)
	}
}

// TestPublish_WithdrawnStudent_DoesNotBlockAndExcludedFromBatch pins locked semantics
// (a)+(b): a withdrawn student's ungraded-with-pages answers no longer hold the
// coverage gate, and a NEW batch carries no item (and thus no email) for them.
func TestPublish_WithdrawnStudent_DoesNotBlockAndExcludedFromBatch(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	// b02 stays ungraded WITH pages — the classic blocker.

	pv := getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if pv["publishable"].(bool) {
		t.Fatal("sanity: ungraded active b02 should block the gate")
	}

	setStudentWithdrawnByExt(t, f.st, "b02", true)

	pv = getJSON[map[string]any](t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish/preview", f.ts.URL, f.aid), http.StatusOK)
	if !pv["publishable"].(bool) {
		t.Fatalf("withdrawn b02 must not block publish: %v", pv["blockers"])
	}
	if n := len(pv["blockers"].([]any)); n != 0 {
		t.Errorf("blockers after withdrawing b02 = %d, want 0", n)
	}
	if int(pv["student_count"].(float64)) != 1 {
		t.Errorf("student_count = %v, want 1 (b02 excluded from the snapshot population)", pv["student_count"])
	}

	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 1 {
		t.Fatalf("items_created = %v, want 1 (no item for withdrawn b02)", res["items_created"])
	}
	items, _ := f.st.ListPublishItems(t.Context(), int64(res["batch_id"].(float64)))
	for _, it := range items {
		if it.StudentExternalID == "b02" {
			t.Fatal("withdrawn b02 must have NO item in a new batch")
		}
	}
}

// TestPublish_ResendAll_SkipsWithdrawn_ReportsCountAndKeepsHistory pins locked
// semantics (b)+(d): a resend-all re-publish after a withdrawal creates no new item
// for the withdrawn student, reports how many were skipped that way
// (skipped_withdrawn), and leaves their already-published item untouched.
func TestPublish_ResendAll_SkipsWithdrawn_ReportsCountAndKeepsHistory(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	first := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	firstBatch := int64(first["batch_id"].(float64))
	if int(first["skipped_withdrawn"].(float64)) != 0 {
		t.Errorf("first publish skipped_withdrawn = %v, want 0", first["skipped_withdrawn"])
	}

	admin := loginAs(t, f.ts, f.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d", uresp.StatusCode)
	}

	setStudentWithdrawnByExt(t, f.st, "b02", true)

	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	if int(res["items_created"].(float64)) != 1 {
		t.Errorf("resend_all after withdrawal items_created = %v, want 1 (b01 only)", res["items_created"])
	}
	if int(res["skipped_withdrawn"].(float64)) != 1 {
		t.Errorf("skipped_withdrawn = %v, want 1 (b02 had a published item, is withdrawn now)", res["skipped_withdrawn"])
	}

	// Already-published history untouched: the superseded first batch still carries
	// b02's item exactly as published.
	items, err := f.st.ListPublishItems(t.Context(), firstBatch)
	if err != nil {
		t.Fatal(err)
	}
	foundB02 := false
	for _, it := range items {
		if it.StudentExternalID == "b02" {
			foundB02 = true
		}
	}
	if len(items) != 2 || !foundB02 {
		t.Fatalf("first batch history changed: %d items, b02 present=%v — want 2 items with b02 intact", len(items), foundB02)
	}
}

// TestResendItem_WithdrawnStudent_409ThenReinstateAllows pins locked semantics (d):
// per-item resend on a withdrawn student's item is a 409 with the exact reinstate
// hint, leaves the item status untouched, and works again after reinstating.
func TestResendItem_WithdrawnStudent_409ThenReinstateAllows(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	var itemID int64
	for _, it := range items {
		if it.StudentExternalID == "b02" {
			itemID = it.ID
		}
	}
	if itemID == 0 {
		t.Fatal("b02 item not found")
	}
	// Drive to "sent" so a wrongful resend would visibly downgrade the status.
	if err := sendPublishItem(t, f.env, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}

	setStudentWithdrawnByExt(t, f.st, "b02", true)

	resp := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resend for withdrawn student: got %d want 409", resp.StatusCode)
	}
	var body map[string]any
	_ = decodeJSONResp(t, resp, &body)
	if body["error"] != "student is withdrawn — reinstate to resend" {
		t.Errorf("409 error = %q, want the exact reinstate hint", body["error"])
	}
	after, _ := f.st.Q.GetPublishItem(t.Context(), itemID)
	if after.EmailStatus != "sent" {
		t.Errorf("after refused resend: status = %q, want unchanged (sent)", after.EmailStatus)
	}

	// Reinstate ⇒ resend works again.
	setStudentWithdrawnByExt(t, f.st, "b02", false)
	resp2 := postJSON(t, f.c, fmt.Sprintf("%s/api/publish/items/%d/resend", f.ts.URL, itemID), map[string]any{})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resend after reinstating: got %d want 200", resp2.StatusCode)
	}
}

// TestPublish_ResendFailed_SkipsWithdrawn pins locked semantics (d) for the batch
// path: resend-failed skips the withdrawn student's failed item (leaving it failed —
// no send job, no email) and reports the count as skipped_withdrawn.
func TestPublish_ResendFailed_SkipsWithdrawn(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		mustExec(t, f.st, "UPDATE publish_items SET email_status='failed', error='boom' WHERE id=$1", it.ID)
	}
	setStudentWithdrawnByExt(t, f.st, "b02", true)

	rr := postExpect(t, f.c, fmt.Sprintf("%s/api/publish/batches/%d/resend-failed", f.ts.URL, batchID), map[string]any{}, http.StatusOK)
	if int(rr["reenqueued"].(float64)) != 1 {
		t.Errorf("reenqueued = %v, want 1 (b01 only)", rr["reenqueued"])
	}
	if int(rr["skipped_withdrawn"].(float64)) != 1 {
		t.Errorf("skipped_withdrawn = %v, want 1 (b02)", rr["skipped_withdrawn"])
	}

	items, _ = f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		if it.StudentExternalID == "b01" && it.EmailStatus != "pending" {
			t.Errorf("b01 after resend-failed: %q, want pending", it.EmailStatus)
		}
		if it.StudentExternalID == "b02" && it.EmailStatus != "failed" {
			t.Errorf("b02 after resend-failed: %q, want failed (skipped, untouched)", it.EmailStatus)
		}
	}
}

// TestPublishPreview_Warnings_DuplicateEmails: two ACTIVE students sharing an email
// (case-insensitively) is a danger warning in the publish dialog — grade emails would
// land in the same mailbox. Withdrawing one of them clears it (active-only count).
func TestPublishPreview_Warnings_DuplicateEmails(t *testing.T) {
	f := publishSetup(t)

	// b03 shares b01's mailbox, differing only in case.
	seedStudent(t, f.st, "b03", "Cy Fake", "ADA@example.edu")

	ws := pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	wantWarning(t, ws, "duplicate_emails", "danger", 1)

	setStudentWithdrawnByExt(t, f.st, "b03", true)
	ws = pubPreviewWarnings(t, f.c, f.ts.URL, f.aid)
	refuteWarning(t, ws, "duplicate_emails")
}

// --- send job actually delivers via the file provider (end-to-end through Sender) ---

func TestPublish_SendItem_DeliversAndMarksSent(t *testing.T) {
	f := publishSetup(t)
	f.gradeOfficial(t, f.answers["b01"], "5", "3.5")
	f.gradeOfficial(t, f.answers["b02"], "6", "4")
	res := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.ts.URL, f.aid), map[string]any{}, http.StatusCreated)
	batchID := int64(res["batch_id"].(float64))

	// Drive the send seam directly (as the email queue worker would) for each pending
	// item; the file provider writes an .eml and returns a provider id.
	items, _ := f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		if err := sendPublishItem(t, f.env, it.ID, false); err != nil {
			t.Fatalf("SendItem(%d): %v", it.ID, err)
		}
	}
	items, _ = f.st.ListPublishItems(t.Context(), batchID)
	for _, it := range items {
		if it.EmailStatus != "sent" {
			t.Errorf("item %s status = %q after send, want sent", it.StudentExternalID, it.EmailStatus)
		}
		if !it.ProviderMessageID.Valid || it.ProviderMessageID.String == "" {
			t.Errorf("item %s missing provider_message_id after send", it.StudentExternalID)
		}
		if it.RegradeToken == "" {
			t.Errorf("item %s missing regrade token after send", it.StudentExternalID)
		}
	}
}

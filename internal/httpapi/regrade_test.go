package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/email"
	"github.com/HaoWen46/adagrade/internal/grading"
	"github.com/HaoWen46/adagrade/internal/ingest"
	"github.com/HaoWen46/adagrade/internal/llm"
	"github.com/HaoWen46/adagrade/internal/llm/fake"
	"github.com/HaoWen46/adagrade/internal/llm/registry"
	"github.com/HaoWen46/adagrade/internal/publish"
	"github.com/HaoWen46/adagrade/internal/queue"
	"github.com/HaoWen46/adagrade/internal/render"
	"github.com/HaoWen46/adagrade/internal/scan"
	"github.com/HaoWen46/adagrade/internal/secrets"
	"github.com/HaoWen46/adagrade/internal/store"
	"github.com/HaoWen46/adagrade/internal/store/db"
	"github.com/HaoWen46/adagrade/internal/store/storetest"
)

// --- regrade v2 test harness ---------------------------------------------------------
//
// Regrade v2 (spec 2026-07-03-regrade-v2-design.md §2-§8): the single-use per-turn token
// chain, multi-problem filing, per-problem adjudication, TA-clicked gated result email,
// unparsed reminder, and final-turn handoff. The webhook needs a PostmarkProvider (only
// it implements ParseInbound); outbound sends are captured via a local httptest server.

// sentEmail is one outbound send captured by the fake Postmark endpoint.
type sentEmail struct {
	To      string
	Subject string
	Body    string
}

type regradeEnv struct {
	ts      *httptest.Server
	st      *store.Store
	ing     *ingest.Service
	sender  *publish.Sender
	cfg     config.Config
	keyFile string

	mu            sync.Mutex
	sent          []sentEmail
	failNext      int           // fake outbound endpoint fails (Postmark ErrorCode!=0) this many more sends
	reminderDelay time.Duration // widens reminder races without slowing unrelated sends
}

func (e *regradeEnv) sentEmails() []sentEmail {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]sentEmail, len(e.sent))
	copy(out, e.sent)
	return out
}

// failNextSend arms the fake outbound endpoint to fail (Postmark-shaped ErrorCode!=0, no
// send recorded) the next n sends — for driving Finding 1's provider-failure-after-the-
// atomic-flip path without a second EmailProvider seam.
func (e *regradeEnv) failNextSend(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failNext = n
}

func (e *regradeEnv) delayReminderSends(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reminderDelay = d
}

// regradeSetup builds a full API test server whose EmailProvider is Postmark-backed (so
// the inbound webhook's ParseInbound works). Invented data only (CLAUDE.md).
func regradeSetup(t *testing.T, extra map[string]string) *regradeEnv {
	t.Helper()
	s := storetest.Fresh(t)

	env := map[string]string{"ADAMARKER_DEV_LOGIN": "1"}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Regrade fixtures define a single-problem assessment, so their synthetic
	// whole-assessment upload must contain one page.
	ing := &ingest.Service{Store: s, Blobs: blobs, Renderer: render.NewFake(1)}
	fakeProv := &fake.Provider{}
	staticSource := llm.StaticSource{"fake": fakeProv}
	runner := &grading.Runner{Store: s, Blobs: blobs, Providers: staticSource}
	scans := &scan.Service{Store: s, Blobs: blobs, Renderer: render.NewFake(2), Providers: staticSource, Ingest: ing}

	keyFile := filepath.Join(t.TempDir(), "secret.key")
	key, err := secrets.LoadOrCreateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	re := &regradeEnv{st: s, cfg: cfg, keyFile: keyFile}
	outboundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			To       string `json:"To"`
			Subject  string `json:"Subject"`
			TextBody string `json:"TextBody"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")

		re.mu.Lock()
		if re.failNext > 0 {
			re.failNext--
			re.mu.Unlock()
			// Postmark-shaped provider failure: non-zero ErrorCode, nothing recorded as sent.
			_ = json.NewEncoder(w).Encode(map[string]any{"ErrorCode": 500, "Message": "simulated provider failure"})
			return
		}
		reminderDelay := re.reminderDelay
		re.mu.Unlock()
		if reminderDelay > 0 && strings.Contains(strings.ToLower(body.Subject), "reminder") {
			time.Sleep(reminderDelay)
		}
		re.mu.Lock()
		re.sent = append(re.sent, sentEmail{To: body.To, Subject: body.Subject, Body: body.TextBody})
		re.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ErrorCode": 0, "Message": "OK", "MessageID": "pm-test-id"})
	}))
	t.Cleanup(outboundSrv.Close)

	emailProv := email.NewPostmarkProvider(email.Config{
		From: "grades@example.edu", PostmarkToken: "test-token", PostmarkBaseURL: outboundSrv.URL,
		ReplyDomain: "inbound.example.edu",
	})

	tokenKey := secrets.Derive(key, "regrade-token-v1")
	emailSender := publish.NewSender(s, emailProv, tokenKey, cfg.RegradeWindow, "inbound.example.edu", nil, blobs, cfg.ReportFontPath)
	qc, err := queue.New(s.Pool, queue.Deps{
		Runner: runner, Scans: scans, Ingest: ing,
		Email: emailSender, EmailRate: 1000,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, Deps{
		Store: s, Ingest: ing, Scans: scans, Queue: qc,
		Providers: registry.NewDBSource(s, key), SecretKey: key,
		EmailProvider: emailProv,
	})
	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	t.Cleanup(ts.Close)

	re.ts, re.sender, re.ing = ts, emailSender, ing
	return re
}

// postWebhook POSTs a raw Postmark-shaped inbound payload to the webhook path.
func postWebhook(t *testing.T, ts *httptest.Server, secret string, payload map[string]any) *http.Response {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/webhooks/email/inbound/%s", ts.URL, secret)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// p1Reply is a valid <p1>...</p1> reply body — the fixture's single problem is number 1,
// so this parses to one contested block that files a request. distinctive lets a test
// make the complaint text unique for a leak/anchor assertion.
func p1Reply(distinctive string) string {
	return fmt.Sprintf("Hi, please look again.\n<p1>\n%s\n</p1>\nThanks!", distinctive)
}

// inboundPayload builds a Postmark-shaped inbound JSON body with a valid <p1> reply body
// (so it FILES) plus a real MailboxHash token, from-address, and SPF/DKIM headers.
func inboundPayload(from, mailboxHash string, spfPass, dkimPass bool) map[string]any {
	return inboundPayloadWithBody(from, mailboxHash, spfPass, dkimPass, "Re: results", p1Reply("my base case n=1 is correct"), "")
}

// inboundPayloadWithMessageID is inboundPayload plus an explicit MessageID (F1).
func inboundPayloadWithMessageID(from, mailboxHash string, spfPass, dkimPass bool, messageID string) map[string]any {
	return inboundPayloadWithBody(from, mailboxHash, spfPass, dkimPass, "Re: results", p1Reply("my base case n=1 is correct"), messageID)
}

// inboundPayloadWithBody controls the subject/body (and optional MessageID) exactly.
func inboundPayloadWithBody(from, mailboxHash string, spfPass, dkimPass bool, subject, body, messageID string) map[string]any {
	spf := "fail (no matching record)"
	if spfPass {
		spf = "pass (domain designates sender)"
	}
	dkim := "dkim=fail"
	if dkimPass {
		dkim = "dkim=pass"
	}
	return map[string]any{
		"From":        from,
		"MailboxHash": mailboxHash,
		"MessageID":   messageID,
		"Date":        time.Now().UTC().Format(time.RFC1123Z),
		"Subject":     subject,
		"TextBody":    body,
		"Headers": []map[string]string{
			{"Name": "Received-SPF", "Value": spf},
			{"Name": "Authentication-Results", "Value": dkim},
		},
	}
}

// --- regrade fixture: assessment -> problem -> rubric -> student -> published item ----

type regradeFixture struct {
	re           *regradeEnv
	c            *http.Client
	aid          int64
	pid          int64
	studentEmail string
	studentID    int64
	itemID       int64
	token        string // valid, unexpired v2 turn-1 token for itemID
	batchID      int64
}

func regradeFixtureSetup(t *testing.T, extra map[string]string) regradeFixture {
	t.Helper()
	re := regradeSetup(t, extra)
	c := loginAs(t, re.ts, re.st, "lect@ntu.edu.tw", "lecturer")

	a := postExpect(t, c, re.ts.URL+"/api/assessments", map[string]string{"kind": "exam", "name": "Midterm"}, http.StatusCreated)
	aid := int64(a["id"].(float64))
	p := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/problems", re.ts.URL, aid),
		map[string]any{"number": 1, "title": "Greedy", "max_points": "10"}, http.StatusCreated)
	pid := int64(p["id"].(float64))
	rv := postExpect(t, c, fmt.Sprintf("%s/api/problems/%d/rubric", re.ts.URL, pid), map[string]any{
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

	studentEmail := "ada@example.edu"
	seedStudent(t, re.st, "b01", "Ada Fake", studentEmail)
	uploadFakePDF(t, c, re.ts, aid, "b01.pdf")
	driveDirectUploadsRegrade(t, re, aid)

	answers := getJSON[map[string][]map[string]any](t, c, fmt.Sprintf("%s/api/problems/%d/students", re.ts.URL, pid), http.StatusOK)
	var answerID int64
	for _, s := range answers["students"] {
		if s["student_id"].(string) == "b01" {
			answerID = int64(s["answer_id"].(float64))
		}
	}

	postExpect(t, c, fmt.Sprintf("%s/api/answers/%d/records", re.ts.URL, answerID), map[string]any{
		"rubric_version_id": rubricID,
		"comment":           "graded",
		"scores": []map[string]any{
			{"criterion_id": critIDs[0], "score": "5"},
			{"criterion_id": critIDs[1], "score": "3.5"},
		},
	}, http.StatusCreated)
	// Round-based grading (0027): choose consensus as the final source — with no
	// aggregates in this fixture the recompute derives the human record official,
	// and publishing (below and after later re-grades) stays unblocked.
	putJSON(t, c, fmt.Sprintf("%s/api/assessments/%d/final-source", re.ts.URL, aid),
		map[string]any{"kind": "consensus"}, http.StatusOK)

	pub := postExpect(t, c, fmt.Sprintf("%s/api/assessments/%d/publish", re.ts.URL, aid), map[string]any{}, http.StatusCreated)
	batchID := int64(pub["batch_id"].(float64))

	items, err := re.st.ListPublishItems(t.Context(), batchID)
	if err != nil {
		t.Fatal(err)
	}
	var itemID int64
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			itemID = it.ID
		}
	}
	if itemID == 0 {
		t.Fatalf("b01 publish item not found among %+v", items)
	}

	// Drive the send seam directly (as the email.send worker would) so the item gets a
	// real, persisted v2 turn-1 regrade token.
	if err := sendPublishItemWith(t, re.st, re.sender, itemID, false); err != nil {
		t.Fatalf("SendItem: %v", err)
	}
	item, err := re.st.Q.GetPublishItem(t.Context(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := re.st.Q.GetPublishItemForSend(t.Context(), itemID)
	if err != nil {
		t.Fatal(err)
	}

	return regradeFixture{
		re: re, c: c, aid: aid, pid: pid, studentEmail: studentEmail, studentID: row.StudentID,
		itemID: itemID, token: item.RegradeToken, batchID: batchID,
	}
}

func driveDirectUploadsRegrade(t *testing.T, re *regradeEnv, assessmentID int64) {
	t.Helper()
	ctx := t.Context()
	rows, err := re.st.Q.ListDirectUploadsForAssessment(ctx, db.ListDirectUploadsForAssessmentParams{
		AssessmentID: assessmentID, Limit: 200,
	})
	if err != nil {
		t.Fatalf("ListDirectUploadsForAssessment: %v", err)
	}
	for _, row := range rows {
		if row.FinishedAt.Valid {
			continue
		}
		if err := re.ing.IngestDirectUpload(ctx, row.ID, false); err != nil {
			t.Fatalf("IngestDirectUpload(%d): %v", row.ID, err)
		}
	}
}

func loadMasterKeyForRegradeEnv(t *testing.T, re *regradeEnv) [32]byte {
	t.Helper()
	key, err := secrets.LoadOrCreateKey(re.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// mintV2Token mints a valid v2 token for the fixture's item at the given turn, using the
// same key the server derives — for driving deeper turns / handoff directly.
func mintV2Token(t *testing.T, re *regradeEnv, itemID int64, turn int) string {
	t.Helper()
	key := loadMasterKeyForRegradeEnv(t, re)
	tokenKey := secrets.Derive(key, "regrade-token-v1")
	return email.MintTokenV2(tokenKey, itemID, turn, time.Now().Add(14*24*time.Hour))
}

// onlyRow returns the single regrade row, failing if there isn't exactly one.
func onlyRow(t *testing.T, re *regradeEnv) db.ListRegradeRequestsRow {
	t.Helper()
	listed, err := re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected exactly 1 regrade row, got %d: %+v", len(listed), listed)
	}
	return listed[0]
}

// --- webhook: path secret ------------------------------------------------------------

func TestWebhook_WrongSecret_404sAndRecordsNothing(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "right-secret"})
	before := len(f.re.sentEmails())

	resp := postWebhook(t, f.re.ts, "wrong-secret", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path secret: got %d want 404", resp.StatusCode)
	}

	listed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("wrong secret must record nothing, got %d rows", len(listed))
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("wrong secret must send nothing, got %d new sends", len(f.re.sentEmails())-before)
	}
}

func TestWebhook_UnconfiguredSecret_404sEvenWithEmptyPath(t *testing.T) {
	f := regradeFixtureSetup(t, nil)
	resp := postWebhook(t, f.re.ts, "", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured secret: got %d want 404", resp.StatusCode)
	}
}

// --- ladder rung 1: bad/expired/v1 token ---------------------------------------------

func TestWebhook_Rung1_BadToken_RejectedNoBackscatter(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, "not-a-real-token", true, true))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("malformed token webhook: got %d want 200", resp.StatusCode)
	}

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedBadToken {
		t.Fatalf("expected rejected_bad_token, got %+v", row)
	}
	if row.PublishItemID.Valid {
		t.Errorf("bad-token row must have no publish_item_id, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("no-backscatter violated: %d new send(s) for a bad token", len(f.re.sentEmails())-before)
	}
}

func TestWebhook_Rung1_ExpiredToken_Rejected(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_WINDOW":         "1h",
	})
	before := len(f.re.sentEmails())

	key := loadMasterKeyForRegradeEnv(t, f.re)
	tokenKey := secrets.Derive(key, "regrade-token-v1")
	expired := email.MintTokenV2(tokenKey, f.itemID, 1, time.Now().Add(-time.Minute))

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, expired, true, true))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expired token webhook: got %d want 200", resp.StatusCode)
	}

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedBadToken {
		t.Fatalf("expired token: expected rejected_bad_token, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("no-backscatter violated for expired token: %d new sends", len(f.re.sentEmails())-before)
	}
}

// TestWebhook_Rung1_V1Token_Rejected: a legacy v1-shaped token (4 dot-separated parts,
// version "v1") is rejected outright — v2 verification only ever accepts the "v2." shape.
func TestWebhook_Rung1_V1Token_Rejected(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	// A v1-shaped token string — cannot verify as v2 regardless of signature.
	v1Shaped := fmt.Sprintf("v1.%d.9999999999.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", f.itemID)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, v1Shaped, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedBadToken {
		t.Fatalf("v1 token: expected rejected_bad_token, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("no-backscatter violated for v1 token: %d new sends", len(f.re.sentEmails())-before)
	}
}

// --- ladder rung 2: superseded batch -------------------------------------------------

func TestWebhook_Rung2_SupersededBatch_Rejected(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	admin := loginAs(t, f.re.ts, f.re.st, "admin@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.re.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	if uresp.StatusCode != http.StatusOK {
		t.Fatalf("unpublish: got %d", uresp.StatusCode)
	}

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedSupersed {
		t.Fatalf("expected rejected_superseded, got %+v", row)
	}
	if !row.PublishItemID.Valid || row.PublishItemID.Int64 != f.itemID {
		t.Errorf("superseded row should still carry the publish_item_id, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("no-backscatter violated for superseded batch: %d new sends", len(f.re.sentEmails())-before)
	}
}

// TestWebhook_Rung2_SupersededToken_RebindsToLiveItem covers C3: a reply carrying a
// token minted against a now-SUPERSEDED batch re-binds to the same student's item in the
// live batch — REMAPPED to the live chain's next open turn (1 here: the fresh chain has
// no filed requests yet) — files there, and is recorded against the LIVE item id.
func TestWebhook_Rung2_SupersededToken_RebindsToLiveItem(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	oldItemID := f.itemID
	oldToken := f.token // turn-1 token minted against batch1

	admin := loginAs(t, f.re.ts, f.re.st, "admin-c3@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.re.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()

	// Re-publish so a NEW live item exists for b01 (resend_all since nothing changed).
	pub := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.re.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	newBatchID := int64(pub["batch_id"].(float64))
	items, err := f.re.st.ListPublishItems(t.Context(), newBatchID)
	if err != nil {
		t.Fatal(err)
	}
	var newItemID int64
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			newItemID = it.ID
		}
	}
	if newItemID == 0 || newItemID == oldItemID {
		t.Fatalf("expected a fresh live item, got %d (old %d)", newItemID, oldItemID)
	}

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, oldToken, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled {
		t.Fatalf("re-bound reply should file, got kind %q status %q", row.Kind, row.Status)
	}
	if !row.PublishItemID.Valid || row.PublishItemID.Int64 != newItemID {
		t.Errorf("re-bound row should carry the LIVE item id %d, got %+v", newItemID, row.PublishItemID)
	}
	if !row.Turn.Valid || row.Turn.Int32 != 1 {
		t.Errorf("re-bind must file at the live chain's next open turn (1), got %+v", row.Turn)
	}
}

// filedRegradeRowsForItem returns the kind='filed' regrade rows recorded against ONE
// publish item, oldest first — for asserting which chain (old vs live) a reply landed on.
func filedRegradeRowsForItem(t *testing.T, re *regradeEnv, itemID int64) []db.ListRegradeRequestsRow {
	t.Helper()
	listed, err := re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: regradeKindFiled})
	if err != nil {
		t.Fatal(err)
	}
	var out []db.ListRegradeRequestsRow
	for i := len(listed) - 1; i >= 0; i-- { // ListRegradeRequests is newest-first
		if listed[i].PublishItemID.Valid && listed[i].PublishItemID.Int64 == itemID {
			out = append(out, listed[i])
		}
	}
	return out
}

// TestWebhook_Rung2_OldChainToken_RemapsToLiveChainNextOpenTurn (verifier finding, wave
// 5): an OLD chain's turn-N token replayed after an unpublish→re-publish must NOT carry
// its stale turn onto the new chain. Before the fix, rung 2 re-bound the token to the
// live item but PRESERVED its turn — a replayed old turn-2 token filed as
// (new item, turn 2) before the new chain ever sent result #1, so the student's later
// LEGITIMATE turn-2 reply hit the consumed-token check and died as a silent addendum
// with no escape hatch. The fix remaps the re-bound request to the live chain's next
// open turn.
func TestWebhook_Rung2_OldChainToken_RemapsToLiveChainNextOpenTurn(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	oldItemID := f.itemID

	// Chain 1: file at turn 2 (as if result #1 had gone out on the old chain).
	oldTok2 := mintV2Token(t, f.re, oldItemID, 2)
	r1 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, oldTok2, true, true, "pm-remap-old"))
	r1.Body.Close()
	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled || !row.Turn.Valid || row.Turn.Int32 != 2 {
		t.Fatalf("chain-1 turn-2 filing failed: %+v", row)
	}

	// Unpublish → re-publish: chain 1 superseded, a fresh live chain exists for b01.
	admin := loginAs(t, f.re.ts, f.re.st, "admin-remap@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.re.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	pub := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.re.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	newBatchID := int64(pub["batch_id"].(float64))
	items, err := f.re.st.ListPublishItems(t.Context(), newBatchID)
	if err != nil {
		t.Fatal(err)
	}
	var newItemID int64
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			newItemID = it.ID
		}
	}
	if newItemID == 0 || newItemID == oldItemID {
		t.Fatalf("expected a fresh live item, got %d (old %d)", newItemID, oldItemID)
	}

	// Replay the old chain's turn-2 token: it re-binds to the live item and must file
	// at the live chain's next open turn — 1, not the stale 2.
	r2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, oldTok2, true, true, "pm-remap-replay"))
	r2.Body.Close()
	filedNew := filedRegradeRowsForItem(t, f.re, newItemID)
	if len(filedNew) != 1 {
		t.Fatalf("expected the replay to file exactly once on the live item, got %d rows", len(filedNew))
	}
	if !filedNew[0].Turn.Valid || filedNew[0].Turn.Int32 != 1 {
		t.Fatalf("stale turn-2 replay must remap to the live chain's next open turn (1), got %+v", filedNew[0].Turn)
	}

	// Adjudicate + send result #1 on the live chain — the send that mints the live
	// chain's genuine turn-2 token.
	subs, err := f.re.st.ListRequestProblems(t.Context(), filedNew[0].ID)
	if err != nil || len(subs) != 1 {
		t.Fatalf("expected 1 sub-item on the remapped filing: %v (%d)", err, len(subs))
	}
	ta := loginAs(t, f.re.ts, f.re.st, "ta-remap@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, filedNew[0].ID, subs[0].ID),
		map[string]any{"outcome": "upheld", "note": "reviewed"}, http.StatusOK)
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, filedNew[0].ID), map[string]any{}, http.StatusOK)
	if int(got["turn"].(float64)) != 1 {
		t.Fatalf("live chain's first result should be #1, got %v", got["turn"])
	}

	// The student's GENUINE turn-2 reply still files normally — before the fix the
	// stale replay had already consumed (new item, turn 2) and this legitimate reply
	// was stranded as a silent addendum.
	genuineTok2 := mintV2Token(t, f.re, newItemID, 2)
	r3 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, genuineTok2, true, true, "pm-remap-genuine"))
	r3.Body.Close()
	filedNew = filedRegradeRowsForItem(t, f.re, newItemID)
	if len(filedNew) != 2 {
		t.Fatalf("the genuine turn-2 reply must FILE on the live chain, got %d filed rows", len(filedNew))
	}
	if !filedNew[1].Turn.Valid || filedNew[1].Turn.Int32 != 2 {
		t.Fatalf("genuine turn-2 reply should file at turn 2, got %+v", filedNew[1].Turn)
	}
	// And nothing on the live chain degraded into an addendum.
	addenda, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: regradeKindAddendum})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addenda {
		if a.PublishItemID.Valid && a.PublishItemID.Int64 == newItemID {
			t.Fatalf("no live-chain reply should have been recorded as an addendum, got %+v", a)
		}
	}
}

// TestWebhook_Rung2_OldChainToken_RemapCapsAtHandoffSlot: the remapped turn is capped at
// MAX+1 (the handoff slot) so a stale replay against a FULLY-CONSUMED live chain records
// an addendum on the already-fired handoff slot — it must never mint a fresh turn past
// MAX+1 and fire a second handoff.
func TestWebhook_Rung2_OldChainToken_RemapCapsAtHandoffSlot(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	oldTok1 := f.token // turn-1 token minted against chain 1

	admin := loginAs(t, f.re.ts, f.re.st, "admin-cap@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.re.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	pub := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.re.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)
	newBatchID := int64(pub["batch_id"].(float64))
	items, err := f.re.st.ListPublishItems(t.Context(), newBatchID)
	if err != nil {
		t.Fatal(err)
	}
	var newItemID int64
	for _, it := range items {
		if it.StudentExternalID == "b01" {
			newItemID = it.ID
		}
	}
	if newItemID == 0 {
		t.Fatal("no live item for b01")
	}

	// Fully consume the live chain under MAX=2: file turns 1 and 2, then the MAX+1(=3)
	// handoff token.
	for turn := 1; turn <= 3; turn++ {
		tok := mintV2Token(t, f.re, newItemID, turn)
		r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, tok, true, true, fmt.Sprintf("pm-cap-live-%d", turn)))
		r.Body.Close()
	}

	// Replay the old chain's token: next open turn on the live chain would be 4, but
	// the cap holds it at MAX+1 (3) — already consumed by the handoff — so the replay
	// records an addendum, never a second handoff.
	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, oldTok1, true, true, "pm-cap-replay"))
	r.Body.Close()

	listed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if err != nil {
		t.Fatal(err)
	}
	var handed, addenda int
	for _, row := range listed {
		if !row.PublishItemID.Valid || row.PublishItemID.Int64 != newItemID {
			continue
		}
		switch row.Kind {
		case regradeKindHandedOff:
			handed++
		case regradeKindFiled, regradeKindAddendum:
			if row.Kind == regradeKindAddendum {
				addenda++
			}
		}
		if row.Turn.Valid && row.Turn.Int32 > 3 && row.Kind != regradeKindAddendum {
			t.Fatalf("no filed/handed_off row may exist past the MAX+1 slot, got %+v", row)
		}
	}
	if handed != 1 {
		t.Fatalf("expected exactly 1 handoff on the live chain, got %d", handed)
	}
	if addenda != 1 {
		t.Fatalf("the capped stale replay must record exactly 1 addendum, got %d", addenda)
	}
}

func TestWebhook_Rung2_SupersededToken_SenderStillCheckedAfterRebind(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	oldToken := f.token

	admin := loginAs(t, f.re.ts, f.re.st, "admin-c3b@ntu.edu.tw", "admin")
	uresp := postJSON(t, admin, fmt.Sprintf("%s/api/assessments/%d/unpublish", f.re.ts.URL, f.aid), map[string]any{})
	uresp.Body.Close()
	postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/publish", f.re.ts.URL, f.aid), map[string]any{"resend_all": true}, http.StatusCreated)

	// Wrong sender even after a successful re-bind → rejected_sender_mismatch.
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload("imposter@example.edu", oldToken, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedSender {
		t.Fatalf("sender must still be checked after re-bind, got %+v", row)
	}
}

// --- ladder rung 3: sender match -----------------------------------------------------

func TestWebhook_Rung3_SenderMismatch_Rejected(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload("someone-else@example.edu", f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedSender {
		t.Fatalf("expected rejected_sender_mismatch, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("no-backscatter violated for sender mismatch: %d new sends", len(f.re.sentEmails())-before)
	}
}

func TestWebhook_Rung3_SenderMatch_CaseInsensitive(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload("ADA@EXAMPLE.EDU", f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled {
		t.Fatalf("case-insensitive sender match should file, got %+v", row)
	}
}

func TestWebhook_Rung3_RosterEmailChanged_OldAddressRejected(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	// Change the roster email — the OLD frozen recipient address must no longer match.
	if _, err := f.re.st.Pool.Exec(t.Context(),
		"UPDATE students SET email = 'ada.new@example.edu' WHERE student_id = 'b01'"); err != nil {
		t.Fatal(err)
	}
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Status != regradeStatusRejectedSender {
		t.Fatalf("changed roster email must reject the old address, got %+v", row)
	}
}

// --- rung 4: SPF/DKIM recorded, warn-not-block ---------------------------------------

func TestWebhook_Rung4_SPFDKIMFail_StillFiledButRecorded(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, false, false))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled {
		t.Fatalf("SPF/DKIM fail is warn-not-block, should still file, got %+v", row)
	}
	if !row.SpfVerdict.Valid || row.SpfVerdict.String != "fail" {
		t.Errorf("spf_verdict = %+v, want fail (recorded)", row.SpfVerdict)
	}
	if !row.DkimVerdict.Valid || row.DkimVerdict.String != "fail" {
		t.Errorf("dkim_verdict = %+v, want fail (recorded)", row.DkimVerdict)
	}
}

// --- §3/§4 filing + parsing ----------------------------------------------------------

func TestWebhook_ValidBlock_FilesAndSendsConfirmation(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled || !row.Turn.Valid || row.Turn.Int32 != 1 {
		t.Fatalf("expected a filed turn-1 row, got %+v", row)
	}
	// One sub-item for problem 1.
	subs, err := f.re.st.ListRequestProblems(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub-item, got %d", len(subs))
	}
	// Confirmation email sent, numbers-only, attempt counter, no token.
	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected exactly 1 confirmation send, got %d", len(sent)-before)
	}
	conf := sent[len(sent)-1]
	if !strings.Contains(conf.Subject, "regrade request received") {
		t.Errorf("confirmation subject = %q", conf.Subject)
	}
	if !strings.Contains(conf.Body, "attempt 1 of 3") {
		t.Errorf("confirmation should carry the attempt counter, got:\n%s", conf.Body)
	}
	if strings.Contains(conf.Body, "my base case n=1 is correct") {
		t.Errorf("confirmation must be numbers-only, must NOT echo complaint text:\n%s", conf.Body)
	}
}

// TestWebhook_FilingPath_AllOrNothingOnSubItemFailure (Finding 2, IMPORTANT): the
// webhook's filing path must insert the regrade_requests row AND its sub-items as one
// atomic unit. Before the fix, the webhook called InsertRegradeRequestV2 (commits alone,
// consuming the (publish_item_id, turn) slot via the partial unique index) and THEN
// InsertRequestProblems in a SEPARATE transaction — anything going wrong between the two
// (a crash between the statements, most plausibly) could leave a committed kind='filed'
// row with ZERO sub-items: a permanently stranded slot (the unique index can't tell
// "legitimately filed" from "orphaned"), so no retry could ever file against that
// (item, turn) again, and the send-result gate 409s forever with nothing to verdict.
//
// A genuine crash-between-two-statements is not reproducible through the HTTP webhook
// without a fault-injection seam (both statements run back-to-back inside one synchronous
// handler call with no yield point an external actor could land a race in — confirmed
// empirically: hammering a concurrent DELETE /api/problems/{id} against the contested
// problem across 200 attempts per trial never once landed between the two statements,
// because DeleteProblem is blocked by the FK the instant the sub-item exists, and the
// webhook's own parse step already excludes a problem deleted before it starts). So this
// test drives the exact same store call the webhook handler makes
// (store.FileRegradeRequestV2, with the httpapi fixture's real item/student/problem ids)
// and forces the sub-item half to fail deterministically (a bad problem_id FK, mirroring
// the store-layer coverage in TestFileRegradeRequestV2_AtomicAcrossRequestAndSubItems) to
// prove the httpapi-reachable data model can never end up with an orphaned filed row: the
// (item, turn) slot must be free afterward for a legitimate retry to file successfully.
func TestWebhook_FilingPath_AllOrNothingOnSubItemFailure(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	_, _, err := f.re.st.FileRegradeRequestV2(t.Context(), store.FileRegradeRequestParams{
		PublishItemID: f.itemID, StudentID: f.studentID, AssessmentID: f.aid,
		FromEmail: f.studentEmail, Subject: "re: grade", Body: "<p1>\nbase case wrong\n</p1>",
		Status: regradeStatusReceived, Kind: regradeKindFiled, Turn: 1,
		Problems: []store.RequestProblemInput{
			{ProblemID: f.pid, ComplaintText: "base case wrong"},
			{ProblemID: 999999999, ComplaintText: "bad FK, must roll back the WHOLE filing"},
		},
	})
	if err == nil {
		t.Fatal("expected an FK-violation error inserting a nonexistent problem_id, got nil")
	}

	// No orphaned row: NOT "filed with zero sub-items".
	listed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: regradeKindFiled})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("expected the failed filing to leave NO regrade_requests row, got %d: %+v", len(listed), listed)
	}

	// The (item, turn) slot must be free: the webhook path (via a real inbound delivery)
	// can still file successfully at the same turn afterward.
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled || !row.Turn.Valid || row.Turn.Int32 != 1 {
		t.Fatalf("expected the retried webhook filing to succeed at turn 1, got %+v", row)
	}
	subs, err := f.re.st.ListRequestProblems(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub-item on the successful retry, got %d", len(subs))
	}
}

// TestWebhook_UnparsedReply_RecordedNoConsumeSilent (spec §4 D58): 0 valid blocks records
// an unparsed row, does NOT consume the token, and sends nothing.
func TestWebhook_UnparsedReply_RecordedNoConsumeSilent(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	before := len(f.re.sentEmails())

	// No valid <pN> block in the body.
	resp := postWebhook(t, f.re.ts, "s3cr3t",
		inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re: results", "please just look at problem one again, thanks", ""))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindUnparsed {
		t.Fatalf("0-block reply must record unparsed, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("unparsed reply must be silent, got %d new sends", len(f.re.sentEmails())-before)
	}
	// Token NOT consumed: a subsequent valid reply to the SAME token still files.
	resp2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	defer resp2.Body.Close()
	filedCount := 0
	listed, _ := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	for _, r := range listed {
		if r.Kind == regradeKindFiled {
			filedCount++
		}
	}
	if filedCount != 1 {
		t.Fatalf("unparsed must NOT consume the token — a later valid reply should still file exactly once, got %d filed", filedCount)
	}
}

// TestWebhook_ConsumedToken_RecordsAddendumSilent (spec §4 D57): a second reply to an
// already-FILED token records an addendum, no processing, no confirmation.
func TestWebhook_ConsumedToken_RecordsAddendumSilent(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-file-1"))
	resp.Body.Close()
	sentAfterFile := len(f.re.sentEmails())

	// Second reply, same token (different message id so F1 doesn't dedup it).
	resp2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-file-2"))
	defer resp2.Body.Close()

	listed, _ := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	var filed, addendum int
	for _, r := range listed {
		switch r.Kind {
		case regradeKindFiled:
			filed++
		case regradeKindAddendum:
			addendum++
		}
	}
	if filed != 1 || addendum != 1 {
		t.Fatalf("expected 1 filed + 1 addendum, got filed=%d addendum=%d (%+v)", filed, addendum, listed)
	}
	if len(f.re.sentEmails()) != sentAfterFile {
		t.Fatalf("addendum must be silent — no second confirmation, got %d new sends", len(f.re.sentEmails())-sentAfterFile)
	}
}

// TestWebhook_UnknownProblemNumber_BlockEvaporates: a <p9> for a problem that doesn't
// exist in the assessment is silently dropped; if it's the ONLY block, the reply is
// unparsed (0 valid blocks).
func TestWebhook_UnknownProblemNumber_BlockEvaporates(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	body := "<p9>\nthere is no problem 9\n</p9>"
	resp := postWebhook(t, f.re.ts, "s3cr3t",
		inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re: results", body, ""))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindUnparsed {
		t.Fatalf("an unknown-N-only reply must be unparsed (block evaporates), got %+v", row)
	}
}

// --- §6 final-turn handoff -----------------------------------------------------------

func TestWebhook_TurnMaxPlusOne_HandsOffAndNotifiesTA(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})

	// Assign a TA to the fixture's problem so the handoff has a target.
	taID := seedRoleUser(t, f.re.st, "grader@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-assign@ntu.edu.tw", "lecturer")
	putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)

	before := len(f.re.sentEmails())

	// Reply to the MAX+1 (=3) handoff token with a valid block.
	handoffTok := mintV2Token(t, f.re, f.itemID, 3)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, handoffTok, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindHandedOff {
		t.Fatalf("MAX+1 reply must hand off, got %+v", row)
	}
	// A TA-notify email was sent to the assigned TA.
	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected exactly 1 TA-notify send, got %d", len(sent)-before)
	}
	tn := sent[len(sent)-1]
	if tn.To != "grader@ntu.edu.tw" {
		t.Errorf("TA-notify To = %q, want the assigned TA", tn.To)
	}
	if !strings.Contains(tn.Subject, "regrade handoff") {
		t.Errorf("TA-notify subject = %q", tn.Subject)
	}
	// Audited regrade.handoff.
	admin := loginAs(t, f.re.ts, f.re.st, "admin-ho@ntu.edu.tw", "admin")
	entries := getJSON[map[string]any](t, admin,
		fmt.Sprintf("%s/api/audit?target_kind=regrade_request&target_id=%d", f.re.ts.URL, row.ID), http.StatusOK)
	var found bool
	for _, e := range entries["entries"].([]any) {
		if e.(map[string]any)["action"] == "regrade.handoff" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a regrade.handoff audit row")
	}
}

// TestWebhook_Handoff_AppBaseURLConfigured_AbsoluteDeepLink covers F4: when
// ADAMARKER_APP_BASE_URL is set, the TA-notify email's deep link is an absolute URL
// (baseURL + "/regrades/{id}") rather than a bare path dead in any mail client.
func TestWebhook_Handoff_AppBaseURLConfigured_AbsoluteDeepLink(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
		"ADAMARKER_APP_BASE_URL":           "https://ada.csie.ntu.edu.tw",
	})
	taID := seedRoleUser(t, f.re.st, "grader-baseurl@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-baseurl@ntu.edu.tw", "lecturer")
	putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)

	handoffTok := mintV2Token(t, f.re, f.itemID, 3)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, handoffTok, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	sent := f.re.sentEmails()
	if len(sent) == 0 {
		t.Fatal("expected a TA-notify send")
	}
	body := sent[len(sent)-1].Body
	wantLink := fmt.Sprintf("https://ada.csie.ntu.edu.tw/regrades/%d", row.ID)
	if !strings.Contains(body, wantLink) {
		t.Errorf("TA-notify body should contain the absolute deep link %q, got:\n%s", wantLink, body)
	}
}

// TestWebhook_Handoff_AppBaseURLUnset_NoDeepLinkLine covers F4: when
// ADAMARKER_APP_BASE_URL is unset, the TA-notify email drops the link line entirely
// rather than emitting a dead bare path like "/regrades/42".
func TestWebhook_Handoff_AppBaseURLUnset_NoDeepLinkLine(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	taID := seedRoleUser(t, f.re.st, "grader-nobaseurl@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-nobaseurl@ntu.edu.tw", "lecturer")
	putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)

	handoffTok := mintV2Token(t, f.re, f.itemID, 3)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, handoffTok, true, true))
	defer resp.Body.Close()

	sent := f.re.sentEmails()
	if len(sent) == 0 {
		t.Fatal("expected a TA-notify send")
	}
	body := sent[len(sent)-1].Body
	if strings.Contains(body, "/regrades/") {
		t.Errorf("TA-notify body should have NO deep link line when ADAMARKER_APP_BASE_URL is unset, got:\n%s", body)
	}
	if strings.Contains(body, "Open in app") {
		t.Errorf("TA-notify body should drop the \"Open in app\" line entirely when unset, got:\n%s", body)
	}
}

// TestWebhook_TurnBeyondMaxPlusOne_StillHandsOffNotCrashesOrRejects covers the reviewer-
// noted gap: the handoff condition is `turn > maxTurns`, not `turn == maxTurns+1` — a
// token whose turn is further out than the very next slot (e.g. a stale/duplicate mint,
// or MAX lowered mid-term after a turn-4 token was already handed out under a higher MAX)
// must still hand off cleanly, not crash or fall through to a rejection/error path. This
// mints a turn-4 token directly under MAX=2 (turn == MAX+2, skipping past the MAX+1 slot
// entirely) and asserts it hands off exactly like the MAX+1 case.
func TestWebhook_TurnBeyondMaxPlusOne_StillHandsOffNotCrashesOrRejects(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	taID := seedRoleUser(t, f.re.st, "grader-turn4@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-turn4@ntu.edu.tw", "lecturer")
	putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)

	before := len(f.re.sentEmails())

	// turn 4 under MAX=2: MAX+2, not MAX+1 — well beyond the "next" handoff slot.
	tok4 := mintV2Token(t, f.re, f.itemID, 4)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, tok4, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindHandedOff {
		t.Fatalf("a turn beyond MAX+1 must still hand off, got %+v", row)
	}
	if !row.Turn.Valid || row.Turn.Int32 != 4 {
		t.Fatalf("handoff row should preserve the actual turn (4), got %+v", row.Turn)
	}
	subs, err := f.re.st.ListRequestProblems(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub-item on the handoff row, got %d", len(subs))
	}
	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected exactly 1 TA-notify send, got %d", len(sent)-before)
	}
	if !strings.Contains(sent[len(sent)-1].Subject, "regrade handoff") {
		t.Errorf("TA-notify subject = %q", sent[len(sent)-1].Subject)
	}
}

// TestWebhook_Handoff_UnassignedProblem_NoMail: a handoff whose contested problem has no
// assigned TA sends NO email (no target) but still records handed_off.
func TestWebhook_Handoff_UnassignedProblem_NoMail(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	before := len(f.re.sentEmails())

	handoffTok := mintV2Token(t, f.re, f.itemID, 3)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, handoffTok, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindHandedOff {
		t.Fatalf("expected handed_off, got %+v", row)
	}
	if len(f.re.sentEmails()) != before {
		t.Fatalf("unassigned handoff must send no mail, got %d new sends", len(f.re.sentEmails())-before)
	}
}

// TestWebhook_HandoffTokenConsumedTwice_SecondIsAddendum: a second reply to the same
// handoff token records an addendum — the consumed check treats handed_off as consumed.
func TestWebhook_HandoffTokenConsumedTwice_SecondIsAddendum(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	handoffTok := mintV2Token(t, f.re, f.itemID, 3)

	r1 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, handoffTok, true, true, "ho-1"))
	r1.Body.Close()
	r2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, handoffTok, true, true, "ho-2"))
	defer r2.Body.Close()

	listed, _ := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	var handed, addendum int
	for _, r := range listed {
		switch r.Kind {
		case regradeKindHandedOff:
			handed++
		case regradeKindAddendum:
			addendum++
		}
	}
	if handed != 1 || addendum != 1 {
		t.Fatalf("expected 1 handed_off + 1 addendum, got handed=%d addendum=%d", handed, addendum)
	}
}

// --- §4 turn budget: mid-flight MAX change -------------------------------------------

// TestWebhook_MidTermMaxChange_InFlightTokenCoherent: a turn-2 token still adjudicates
// when MAX=3, and a turn-3 token adjudicates too — the value is read at receipt time.
func TestWebhook_MidTermMaxChange_TurnBelowMaxStillAdjudicates(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "3",
	})

	// A turn-2 token (a result-#1 reply) files (adjudicates), not handoff.
	tok2 := mintV2Token(t, f.re, f.itemID, 2)
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, tok2, true, true))
	defer resp.Body.Close()

	row := onlyRow(t, f.re)
	if row.Kind != regradeKindFiled || row.Turn.Int32 != 2 {
		t.Fatalf("turn-2 token under MAX=3 must file (adjudicate), got %+v", row)
	}
}

// --- F1 idempotency ------------------------------------------------------------------

func TestWebhook_DuplicateMessageID_IdempotentNoDoubleRowNoDoubleSend(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	r1 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-dup-1"))
	r1.Body.Close()
	afterFirst := len(f.re.sentEmails())

	r2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-dup-1"))
	defer r2.Body.Close()

	listed, _ := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if len(listed) != 1 {
		t.Fatalf("duplicate message id must not create a second row, got %d", len(listed))
	}
	if len(f.re.sentEmails()) != afterFirst {
		t.Fatalf("duplicate message id must not send a second confirmation, got %d new", len(f.re.sentEmails())-afterFirst)
	}
}

func TestWebhook_DifferentMessageIDs_FirstFilesSecondAddendum(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})

	r1 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-a"))
	r1.Body.Close()
	r2 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, f.token, true, true, "pm-b"))
	defer r2.Body.Close()

	listed, _ := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{})
	if len(listed) != 2 {
		t.Fatalf("two distinct deliveries to the same token should both record (1 filed + 1 addendum), got %d", len(listed))
	}
}

// --- queue list/detail ---------------------------------------------------------------

func TestRegradeQueue_ListAndGet(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	resp.Body.Close()
	id := onlyRow(t, f.re).ID

	ta := loginAs(t, f.re.ts, f.re.st, "ta-list@ntu.edu.tw", "ta")
	list := getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades", http.StatusOK)
	rows := list["regrades"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 queue row, got %d", len(rows))
	}
	r0 := rows[0].(map[string]any)
	if r0["kind"].(string) != regradeKindFiled {
		t.Errorf("list row kind = %v, want filed", r0["kind"])
	}

	detail := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id), http.StatusOK)
	if detail["kind"].(string) != regradeKindFiled {
		t.Errorf("detail kind = %v, want filed", detail["kind"])
	}
	probs := detail["problems"].([]any)
	if len(probs) != 1 {
		t.Fatalf("detail should carry 1 sub-item, got %d", len(probs))
	}
	p0 := probs[0].(map[string]any)
	if int(p0["problem_number"].(float64)) != 1 {
		t.Errorf("sub-item problem_number = %v, want 1", p0["problem_number"])
	}
	if _, ok := p0["verdict"]; ok {
		t.Errorf("un-adjudicated sub-item should omit verdict, got %v", p0["verdict"])
	}
}

// TestRegradeQueue_StudentWithdrawnFlag pins locked semantics (f) (roster-lifecycle
// plan 2026-07-10, Task R2): the queue list flags a withdrawn student's rows with
// student_withdrawn — computed live from the roster, so an existing row flips when
// the student withdraws — while the regrade channel itself stays open (the row is
// still listed and adjudicable; no ladder rung rejects on withdrawal).
func TestRegradeQueue_StudentWithdrawnFlag(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	resp.Body.Close()

	ta := loginAs(t, f.re.ts, f.re.st, "ta-withdrawn@ntu.edu.tw", "ta")
	list := getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades", http.StatusOK)
	rows := list["regrades"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 queue row, got %d", len(rows))
	}
	if got := rows[0].(map[string]any)["student_withdrawn"]; got != false {
		t.Errorf("student_withdrawn before withdrawal = %v, want false", got)
	}

	if _, err := f.re.st.Q.SetStudentWithdrawn(t.Context(), db.SetStudentWithdrawnParams{ID: f.studentID, Withdrawn: true}); err != nil {
		t.Fatalf("SetStudentWithdrawn: %v", err)
	}

	list = getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades", http.StatusOK)
	rows = list["regrades"].([]any)
	if len(rows) != 1 {
		t.Fatalf("withdrawn student's request must stay listed (停修 rights), got %d rows", len(rows))
	}
	r0 := rows[0].(map[string]any)
	if got := r0["student_withdrawn"]; got != true {
		t.Errorf("student_withdrawn after withdrawal = %v, want true", got)
	}
	if r0["kind"].(string) != regradeKindFiled {
		t.Errorf("row kind = %v, want filed (unchanged by withdrawal)", r0["kind"])
	}
}

// TestRegradeDetail_ExposesRegradeMax: the detail JSON must carry the configured turn
// budget (regrade v2 UI review Finding 1) so the send-result card can render
// "Attempt {turn} of {regrade_max}" instead of a bare turn number with no ceiling.
func TestRegradeDetail_ExposesRegradeMax(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "5",
	})
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	resp.Body.Close()
	id := onlyRow(t, f.re).ID

	ta := loginAs(t, f.re.ts, f.re.st, "ta-max@ntu.edu.tw", "ta")
	detail := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id), http.StatusOK)
	got, ok := detail["regrade_max"]
	if !ok {
		t.Fatalf("detail response missing regrade_max, got %v", detail)
	}
	if int(got.(float64)) != 5 {
		t.Errorf("regrade_max = %v, want 5", got)
	}
}

func TestRegradeQueue_UnparsedFilter(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	// One filed, one unparsed (different token turns so both persist).
	r1 := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	r1.Body.Close()
	tok2 := mintV2Token(t, f.re, f.itemID, 2)
	r2 := postWebhook(t, f.re.ts, "s3cr3t",
		inboundPayloadWithBody(f.studentEmail, tok2, true, true, "Re", "no tags here", ""))
	r2.Body.Close()

	ta := loginAs(t, f.re.ts, f.re.st, "ta-filter@ntu.edu.tw", "ta")
	list := getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades?kind=unparsed", http.StatusOK)
	rows := list["regrades"].([]any)
	if len(rows) != 1 {
		t.Fatalf("unparsed filter should return exactly the 1 unparsed row, got %d", len(rows))
	}
	if rows[0].(map[string]any)["kind"].(string) != regradeKindUnparsed {
		t.Errorf("filtered row kind = %v, want unparsed", rows[0].(map[string]any)["kind"])
	}
}

// TestRegradeQueue_StudentFilterAndTotal: ?student= filters by external
// student-ID prefix, case-insensitively (the queue is server-paginated, so a
// 250+-roster search must happen server-side), and every list response carries
// a filter-aware total so the UI can render numbered pages.
func TestRegradeQueue_StudentFilterAndTotal(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayload(f.studentEmail, f.token, true, true))
	resp.Body.Close()

	ta := loginAs(t, f.re.ts, f.re.st, "ta-student-filter@ntu.edu.tw", "ta")

	assertList := func(qs string, wantRows, wantTotal int) {
		t.Helper()
		got := getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades"+qs, http.StatusOK)
		if rows := got["regrades"].([]any); len(rows) != wantRows {
			t.Fatalf("%q rows = %d, want %d", qs, len(rows), wantRows)
		}
		total, ok := got["total"]
		if !ok {
			t.Fatalf("%q response missing total: %v", qs, got)
		}
		if int(total.(float64)) != wantTotal {
			t.Errorf("%q total = %v, want %d", qs, total, wantTotal)
		}
	}

	assertList("", 1, 1)             // unfiltered: total drives the pager
	assertList("?student=b0", 1, 1)  // prefix match on the external ID
	assertList("?student=B0", 1, 1)  // case-insensitive
	assertList("?student=zzz", 0, 0) // no match
	assertList("?student=%25", 0, 0) // LIKE wildcards in input are literal, not wildcards
}

// TestRegradeQueue_OpenAndUndeliveredFilters (HCI audit, regrades-list correctness):
// the UI's "Actionable" tab is kind=filed AND status in the open group — it must be
// expressible server-side (?open=1) so page slices and the pager `total` are computed
// over the FILTERED set; the old fetch-unfiltered-narrow-client-side approach rendered
// empty pages while live appeals sat beyond the page limit, with a lying count.
// ?undelivered_result=1 selects the queue-invisible recovery set: filed requests that
// resolved but whose result email never got delivered (result_sent_at NULL).
func TestRegradeQueue_OpenAndUndeliveredFilters(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	ctx := t.Context()

	insert := func(kind, status string, turn int) int64 {
		t.Helper()
		p := store.InsertRegradeRequestV2Params{
			StudentID: f.studentID, AssessmentID: f.aid,
			FromEmail: f.studentEmail, Subject: "re: grade", Body: "body",
			Status: status, Kind: kind, Turn: turn,
		}
		if kind == regradeKindFiled {
			p.PublishItemID = f.itemID
		}
		rr, err := f.re.st.InsertRegradeRequestV2(ctx, p)
		if err != nil {
			t.Fatalf("InsertRegradeRequestV2(%s/%s): %v", kind, status, err)
		}
		return rr.ID
	}

	openFiled := insert(regradeKindFiled, "received", 1)
	delivered := insert(regradeKindFiled, "resolved_regraded", 2)
	undelivered := insert(regradeKindFiled, "resolved_upheld", 3)
	openUnparsed := insert(regradeKindUnparsed, "received", 0)
	if _, err := f.re.st.SetRegradeResultSentAt(ctx, delivered); err != nil {
		t.Fatalf("SetRegradeResultSentAt: %v", err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-open-filter@ntu.edu.tw", "ta")

	assertIDs := func(qs string, wantIDs ...int64) {
		t.Helper()
		got := getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades"+qs, http.StatusOK)
		rows := got["regrades"].([]any)
		gotIDs := make(map[int64]bool, len(rows))
		for _, r := range rows {
			gotIDs[int64(r.(map[string]any)["id"].(float64))] = true
		}
		if len(rows) != len(wantIDs) {
			t.Errorf("%q rows = %d, want %d", qs, len(rows), len(wantIDs))
		}
		for _, id := range wantIDs {
			if !gotIDs[id] {
				t.Errorf("%q missing row %d", qs, id)
			}
		}
		if total := int(got["total"].(float64)); total != len(wantIDs) {
			t.Errorf("%q total = %d, want %d (pager total must match the filtered set)", qs, total, len(wantIDs))
		}
	}

	// The Actionable tab's exact server mapping: filed AND open, total agrees.
	assertIDs("?kind=filed&open=1", openFiled)
	// open group alone spans kinds (the open unparsed row joins in). The fixture's
	// baseline has no other rows — regradeFixtureSetup files nothing by itself.
	assertIDs("?open=1", openFiled, openUnparsed)
	// Resolved-but-undelivered recovery set: exactly the failed-send filed row; the
	// delivered one and the (resolved-status-free) open rows stay out.
	assertIDs("?undelivered_result=1", undelivered)

	// Unrecognized values 400 rather than silently ignoring the filter.
	getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades?open=0", http.StatusBadRequest)
	getJSON[map[string]any](t, ta, f.re.ts.URL+"/api/regrades?undelivered_result=nope", http.StatusBadRequest)
}

func TestRegradeQueue_ListGet_RequireSession(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	unauth := &http.Client{}
	resp, err := unauth.Get(f.re.ts.URL + "/api/regrades")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: got %d want 401", resp.StatusCode)
	}
}

// --- §5 adjudication + gated result send ---------------------------------------------

// fileRequest drives one valid webhook and returns the filed request id + its single
// sub-item id.
func fileRequest(t *testing.T, f regradeFixture, token, messageID string) (requestID, subItemID int64) {
	t.Helper()
	resp := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithMessageID(f.studentEmail, token, true, true, messageID))
	resp.Body.Close()
	listed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: regradeKindFiled})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) == 0 {
		t.Fatal("no filed request after webhook")
	}
	requestID = listed[0].ID
	subs, err := f.re.st.ListRequestProblems(t.Context(), requestID)
	if err != nil || len(subs) == 0 {
		t.Fatalf("no sub-item after filing: %v", err)
	}
	return requestID, subs[0].ID
}

func TestSendResult_GatedUntilAllVerdicted(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-gate-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-gate@ntu.edu.tw", "ta")

	// Before any verdict, send-result 409s.
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("send-result before verdict: got %d want 409", r.StatusCode)
	}

	// Adjudicate the sub-item, then it sends.
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld", "note": "reviewed"}, http.StatusOK)

	before := len(f.re.sentEmails())
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	if int(got["turn"].(float64)) != 1 {
		t.Errorf("result turn = %v, want 1", got["turn"])
	}
	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected 1 result send, got %d", len(sent)-before)
	}
	if !strings.Contains(sent[len(sent)-1].Subject, "regrade result #1") {
		t.Errorf("result subject = %q", sent[len(sent)-1].Subject)
	}
}

// TestSendResult_ZeroSubItems_409 (vacuous-truth trap): a filed request with zero
// sub-items must NOT pass the gate.
func TestSendResult_ZeroSubItems_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-zero-1")
	// Delete the sub-item to create the zero-sub-items edge case.
	if _, err := f.re.st.Pool.Exec(t.Context(), "DELETE FROM regrade_request_problems WHERE id = $1", subID); err != nil {
		t.Fatal(err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-zero@ntu.edu.tw", "ta")
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("zero sub-items must NOT pass the gate, got %d want 409", r.StatusCode)
	}
}

// TestSendResult_SendsOnce_SecondSend409: after a successful send the request is resolved,
// so a second send-result 409s.
func TestSendResult_SendsOnce_SecondSend409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-once-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-once@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	r2 := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("second send-result: got %d want 409 (send is once-only)", r2.StatusCode)
	}
}

// TestSendResult_ConcurrentSend_ExactlyOneWinsRaceGuardsSend (Finding 1, CRITICAL
// TOCTOU): two goroutines call send-result on the SAME open, fully-verdicted request at
// the same moment. Before the fix, the handler's status pre-check
// (rr.Status IN ('received','under_review')) and the later SetRegradeStatus write were
// two separate steps with no atomic guard between them — both goroutines could pass the
// read, both send the result email, and only the last write "won". After the fix, the
// atomic ResolveRegradeRequest guard (UPDATE ... WHERE status IN (...) RETURNING) is what
// decides the race: exactly one goroutine's flip affects a row, that goroutine sends the
// email and audits: the other sees ErrNoRows and gets a 409 without sending anything.
func TestSendResult_ConcurrentSend_ExactlyOneWinsRaceGuardsSend(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-race-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-race@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	before := len(f.re.sentEmails())

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
			defer r.Body.Close()
			statuses[i] = r.StatusCode
		}(i)
	}
	wg.Wait()

	var ok200, conflict409 int
	for _, s := range statuses {
		switch s {
		case http.StatusOK:
			ok200++
		case http.StatusConflict:
			conflict409++
		default:
			t.Errorf("unexpected status %d in concurrent send-result race", s)
		}
	}
	if ok200 != 1 {
		t.Fatalf("expected exactly 1 winning send-result (200), got %d (statuses=%v)", ok200, statuses)
	}
	if conflict409 != n-1 {
		t.Fatalf("expected %d losers (409), got %d (statuses=%v)", n-1, conflict409, statuses)
	}

	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected exactly 1 result email sent under the race, got %d", len(sent)-before)
	}

	// Exactly one regrade.send_result audit row for this request.
	admin := loginAs(t, f.re.ts, f.re.st, "admin-race@ntu.edu.tw", "admin")
	entries := getJSON[map[string]any](t, admin,
		fmt.Sprintf("%s/api/audit?target_kind=regrade_request&target_id=%d", f.re.ts.URL, id), http.StatusOK)
	var auditCount int
	for _, e := range entries["entries"].([]any) {
		if e.(map[string]any)["action"] == "regrade.send_result" {
			auditCount++
		}
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 regrade.send_result audit row, got %d", auditCount)
	}
}

// TestSendResult_ProviderFailure_StaysResolvedNoRollback (Finding 1): the atomic status
// flip can win (this request is no longer racing anyone) and the provider send can still
// fail — a transient outage, say. The handler must NOT roll the status back to
// under_review to "allow a retry": reopening the row would reopen the exact double-send
// window the atomic flip-before-send ordering closes (a retry could then race a second
// concurrent send-result). So the request stays resolved with no email delivered, the
// failure is surfaced as a 500, and a second send-result call also 409s (the row is not
// open) rather than silently retrying the send.
func TestSendResult_ProviderFailure_StaysResolvedNoRollback(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-fail-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-fail@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld", "note": "reviewed"}, http.StatusOK)

	before := len(f.re.sentEmails())
	f.re.failNextSend(1)

	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("provider-failed send-result: got %d want 500", r.StatusCode)
	}
	if got := len(f.re.sentEmails()); got != before {
		t.Fatalf("a failed provider send must not be recorded as sent, got %d new sends", got-before)
	}

	rr, err := f.re.st.GetRegradeRequest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Status != regradeStatusResolvedUpheld {
		t.Fatalf("after a provider send failure the request must stay resolved (no rollback), got status=%q", rr.Status)
	}

	// A second send-result call 409s — the row is resolved, not open. No retry, no
	// second email, no reopened double-send window.
	r2 := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("send-result after a resolved-but-undelivered request: got %d want 409 (no reopen)", r2.StatusCode)
	}
	if got := len(f.re.sentEmails()); got != before {
		t.Fatalf("no email should ever be sent for this request, got %d new sends", got-before)
	}
}

// TestSendResult_FinalTurn_CarriesHandoffCopy: the MAX-turn result email carries the
// final-attempt copy (no "reply again" invitation via format template).
func TestSendResult_FinalTurn_CarriesHandoffCopy(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{
		"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t",
		"ADAMARKER_REGRADE_MAX":            "2",
	})
	// File at turn 2 (the final turn when MAX=2) by replying to a turn-2 token.
	tok2 := mintV2Token(t, f.re, f.itemID, 2)
	id, subID := fileRequest(t, f, tok2, "pm-final-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-final@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	before := len(f.re.sentEmails())
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	if fin, _ := got["final"].(bool); !fin {
		t.Errorf("turn-2 result under MAX=2 should be final, got %v", got["final"])
	}
	sent := f.re.sentEmails()
	body := sent[len(sent)-1].Body
	if !strings.Contains(body, "final attempt") {
		t.Errorf("final-turn result should carry final-attempt copy, got:\n%s", body)
	}
	_ = before
}

func TestSendResult_RegradedCarriesNewScore(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-regraded-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-regr@ntu.edu.tw", "ta")

	// Rounds design (0028): "regraded" ADOPTS the round's AI record as the new
	// grade — seed one with a distinct total and link it to the sub-item.
	recID := seedGradingRecordForSubItem(t, f, id, subID)
	mustExec(t, f.re.st, `UPDATE grading_records SET total = 9 WHERE id = $1`, recID)
	mustExec(t, f.re.st, `UPDATE regrade_request_problems SET ai_record_id = $2 WHERE id = $1`, subID, recID)
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "regraded", "note": "point restored"}, http.StatusOK)

	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	if got["status"].(string) != regradeStatusResolvedRegraded {
		t.Errorf("regraded verdict should resolve as regraded, got %v", got["status"])
	}

	// The result email reports the ADOPTED overlay total as the new score —
	// round 0's official (8.5) is never rewritten.
	sent := f.re.sentEmails()
	if len(sent) == 0 {
		t.Fatal("no result email sent")
	}
	body := sent[len(sent)-1].Body
	if !strings.Contains(body, "New score: 9/10") {
		t.Errorf("result email should carry the adopted total 9/10, got:\n%s", body)
	}
}

func TestSendResult_RequiresTAOrAbove(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-rbac-1")
	unauth := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), nil)
	req.Header.Set("X-ADA-CSRF", "1")
	resp, _ := unauth.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated send-result: got %d want 401", resp.StatusCode)
	}
}

// --- verdict endpoint ----------------------------------------------------------------

func TestVerdict_InvalidOutcome_400(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-verdict-bad")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-vb@ntu.edu.tw", "ta")
	resp := patchJSONRaw(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID), map[string]any{"outcome": "maybe"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid outcome: got %d want 400", resp.StatusCode)
	}
}

func TestVerdict_SubItemOfWrongRequest_400(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-verdict-wrong")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-vw@ntu.edu.tw", "ta")
	// Patch the sub-item under a DIFFERENT (nonexistent) request id.
	resp := patchJSONRaw(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id+999, subID), map[string]any{"outcome": "upheld"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sub-item under wrong request: got %d want 400", resp.StatusCode)
	}
}

// --- add sub-item escape hatch -------------------------------------------------------

func TestAddProblem_OnlyOnFiledRequest(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	// An unparsed row can't take a manual sub-item.
	tok2 := mintV2Token(t, f.re, f.itemID, 2)
	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithBody(f.studentEmail, tok2, true, true, "Re", "no tags", ""))
	r.Body.Close()
	unparsedID := onlyRow(t, f.re).ID

	ta := loginAs(t, f.re.ts, f.re.st, "ta-add@ntu.edu.tw", "ta")
	resp := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems", f.re.ts.URL, unparsedID),
		map[string]any{"problem_id": f.pid, "complaint": "manual add"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("add sub-item to unparsed row: got %d want 409", resp.StatusCode)
	}
}

func TestAddProblem_DuplicateProblem_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-dup-prob")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-dp@ntu.edu.tw", "ta")
	// Problem 1 is already contested — adding it again is a 409.
	resp := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/problems", f.re.ts.URL, id),
		map[string]any{"problem_id": f.pid, "complaint": "again"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate problem: got %d want 409", resp.StatusCode)
	}
}

// --- §7 reminder ---------------------------------------------------------------------

func TestRemind_UnparsedOnce_AnchoredContent(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re", "no tags", ""))
	r.Body.Close()
	id := onlyRow(t, f.re).ID

	ta := loginAs(t, f.re.ts, f.re.st, "ta-remind@ntu.edu.tw", "ta")
	before := len(f.re.sentEmails())
	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	sent := f.re.sentEmails()
	if len(sent) != before+1 {
		t.Fatalf("expected 1 reminder send, got %d", len(sent)-before)
	}
	rem := sent[len(sent)-1]
	if !strings.Contains(rem.Subject, "reminder") {
		t.Errorf("reminder subject = %q", rem.Subject)
	}
	if !strings.Contains(rem.Body, "Midterm") || !strings.Contains(rem.Body, "attempt 1 of 3") {
		t.Errorf("reminder must be anchored (assessment + attempt counter), got:\n%s", rem.Body)
	}
	if !strings.Contains(rem.Body, "results") {
		t.Errorf("reminder should anchor to the live-token email subject, got:\n%s", rem.Body)
	}

	// Second remind 409s (once-only).
	r2 := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("second remind: got %d want 409", r2.StatusCode)
	}
}

func TestRemind_ConcurrentRequestsSendExactlyOnce(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re", "no tags", ""))
	r.Body.Close()
	id := onlyRow(t, f.re).ID
	ta := loginAs(t, f.re.ts, f.re.st, "ta-remind-race@ntu.edu.tw", "ta")
	before := len(f.re.sentEmails())
	f.re.delayReminderSends(100 * time.Millisecond)

	const n = 8
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{})
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()

	var okCount, conflictCount int
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Errorf("unexpected concurrent reminder status %d (all=%v)", status, statuses)
		}
	}
	if okCount != 1 || conflictCount != n-1 {
		t.Errorf("concurrent reminder statuses = %v, want one 200 and %d 409s", statuses, n-1)
	}
	if got := len(f.re.sentEmails()) - before; got != 1 {
		t.Fatalf("concurrent reminders sent %d emails, want exactly 1", got)
	}
}

func TestRemind_ProviderFailureRollsBackForRetry(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	r := postWebhook(t, f.re.ts, "s3cr3t", inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re", "no tags", ""))
	r.Body.Close()
	id := onlyRow(t, f.re).ID
	ta := loginAs(t, f.re.ts, f.re.st, "ta-remind-retry@ntu.edu.tw", "ta")
	before := len(f.re.sentEmails())
	f.re.failNextSend(1)

	failed := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{})
	failed.Body.Close()
	if failed.StatusCode != http.StatusInternalServerError {
		t.Fatalf("provider-failed reminder: got %d want 500", failed.StatusCode)
	}
	rr, err := f.re.st.GetRegradeRequest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Status != regradeStatusReceived {
		t.Fatalf("provider failure left status %q, want received so retry stays possible", rr.Status)
	}

	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	if got := len(f.re.sentEmails()) - before; got != 1 {
		t.Fatalf("failed attempt plus retry recorded %d sent emails, want 1", got)
	}
}

func TestRemind_NonUnparsed_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-remind-filed")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-rnf@ntu.edu.tw", "ta")
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/remind", f.re.ts.URL, id), map[string]any{})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("remind on a filed row: got %d want 409", r.StatusCode)
	}
}

// --- §6/§8 TA assignment -------------------------------------------------------------

func TestAssignTA_LecturerAssignsTA(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	taID := seedRoleUser(t, f.re.st, "assignee@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-ta@ntu.edu.tw", "lecturer")

	got := putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)
	if int64(got["user_id"].(float64)) != taID {
		t.Errorf("assigned user_id = %v, want %d", got["user_id"], taID)
	}

	// Unassign (user_id null).
	got2 := putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": nil}, http.StatusOK)
	if got2["user_id"] != nil {
		t.Errorf("after unassign user_id = %v, want nil", got2["user_id"])
	}
}

func TestAssignTA_AssigneeMustBeTAOrAbove(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	// There is no "student" role; the enforcement is that the assignee must be a KNOWN
	// user with TA+. Assigning a nonexistent user id is a 400.
	lect := loginAs(t, f.re.ts, f.re.st, "lect-ta2@ntu.edu.tw", "lecturer")
	resp := putJSONRaw(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": 999999})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("assigning a nonexistent user: got %d want 400", resp.StatusCode)
	}
}

func TestAssignTA_RequiresLecturer(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	taID := seedRoleUser(t, f.re.st, "some-ta@ntu.edu.tw", "ta")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-noassign@ntu.edu.tw", "ta")
	resp := putJSONRaw(t, ta, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("TA assigning: got %d want 403 (lecturer+ only)", resp.StatusCode)
	}
}

// --- detail JSON: AI record embed ----------------------------------------------------

// TestRegradeDetail_EmbedsAIRecordOnSubItem: when a sub-item links an AI record, the
// detail JSON embeds the old-vs-new comparison on that sub-item.
func TestRegradeDetail_EmbedsAIRecordOnSubItem(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-embed-1")

	recID := seedGradingRecordForSubItem(t, f, id, subID)
	if _, err := f.re.st.SetProblemAIRecord(t.Context(), subID, recID); err != nil {
		t.Fatal(err)
	}

	ta := loginAs(t, f.re.ts, f.re.st, "ta-embed@ntu.edu.tw", "ta")
	detail := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id), http.StatusOK)
	probs := detail["problems"].([]any)
	p0 := probs[0].(map[string]any)
	if _, ok := p0["ai_record"]; !ok {
		t.Fatalf("sub-item should embed ai_record when linked, got %v", p0)
	}
}

// TestRegradeDetail_ExposesSubItemAIError: a terminal AI error persisted on a sub-item is
// visible in the detail JSON, and absent when unset.
func TestRegradeDetail_ExposesSubItemAIError(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-aierr-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-aierr@ntu.edu.tw", "ta")

	before := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id), http.StatusOK)
	p0 := before["problems"].([]any)[0].(map[string]any)
	if v, ok := p0["ai_error"]; ok {
		t.Errorf("ai_error should be omitted before any failure, got %v", v)
	}

	const reason = "AI unavailable — provider removed"
	if _, err := f.re.st.SetProblemAIError(t.Context(), subID, reason); err != nil {
		t.Fatal(err)
	}
	after := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id), http.StatusOK)
	pa := after["problems"].([]any)[0].(map[string]any)
	if got, _ := pa["ai_error"].(string); got != reason {
		t.Errorf("sub-item ai_error = %v, want %q", pa["ai_error"], reason)
	}
}

// --- test-only helpers ---------------------------------------------------------------

// seedRoleUser creates an active user with a role and returns its id.
func seedRoleUser(t *testing.T, st *store.Store, email, role string) int64 {
	t.Helper()
	u, err := st.Q.CreateUser(t.Context(), db.CreateUserParams{Email: email, Role: role, Active: true})
	if err != nil {
		t.Fatalf("seed %s user: %v", role, err)
	}
	return u.ID
}

// putJSONExpect PATCHes/PUTs with a method and asserts the status.
func putJSONExpect(t *testing.T, c *http.Client, method, url string, body any, want int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
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
		t.Fatalf("%s %s: got %d want %d — %s", method, url, resp.StatusCode, want, eb.String())
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v
}

// patchJSONRaw sends a PATCH with the CSRF header and returns the raw response.
func patchJSONRaw(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// seedGradingRecordForSubItem inserts a regrade_ai grading_records row for the answer
// behind a sub-item's request, so a test can link it.
func seedGradingRecordForSubItem(t *testing.T, f regradeFixture, requestID, subItemID int64) int64 {
	t.Helper()
	ctx := t.Context()
	rr, err := f.re.st.GetRegradeRequest(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	var answerID, recordID int64
	if err := f.re.st.Pool.QueryRow(ctx,
		`SELECT a.id, a.official_record_id FROM answers a
		 WHERE a.assessment_id = $1 AND a.student_id = $2 AND a.official_record_id IS NOT NULL LIMIT 1`,
		rr.AssessmentID.Int64, rr.StudentID.Int64).Scan(&answerID, &recordID); err != nil {
		t.Fatalf("resolve contested answer: %v", err)
	}
	var newID int64
	if err := f.re.st.Pool.QueryRow(ctx,
		`INSERT INTO grading_records (answer_id, source, model_id, rubric_version_id, criterion_scores, policy)
		 SELECT $1, 'regrade_ai', 'seed-model', gr.rubric_version_id, '[]'::jsonb, 'regrade_strict'
		 FROM grading_records gr WHERE gr.id = $2
		 RETURNING id`, answerID, recordID).Scan(&newID); err != nil {
		t.Fatalf("insert seed regrade_ai record: %v", err)
	}
	return newID
}

// --- gap 1: GET /api/assessments/{id}/ta-assignments ---------------------------------
//
// Backs the regrade queue's "handed to <TA>" badge and the problems-editor picker's
// current-assignment display (UI commit 1c626b7). Gated the same as the regrade queue's
// other read routes (GET /api/regrades, GET /api/regrades/{id}): any signed-in role
// (TA+), no requireRole wrapper — a TA viewing the queue must see assignment badges too.

func TestTAAssignments_ShapePerProblem(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	// A second, deliberately UNASSIGNED problem alongside the fixture's f.pid, so the
	// response must carry both an assigned and an unassigned row.
	p2 := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/problems", f.re.ts.URL, f.aid),
		map[string]any{"number": 2, "title": "DP", "max_points": "10"}, http.StatusCreated)
	pid2 := int64(p2["id"].(float64))

	// Seeded with a display name directly (seedRoleUser leaves it blank, which is a
	// legitimate real value the handler must also pass through) so this test can assert
	// user_name is actually populated from the assignee's row.
	taUser, err := f.re.st.Q.CreateUser(t.Context(), db.CreateUserParams{
		Email: "grader-badge@ntu.edu.tw", DisplayName: "Badge TA", Role: "ta", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	taID := taUser.ID
	lect := loginAs(t, f.re.ts, f.re.st, "lect-badge@ntu.edu.tw", "lecturer")
	putJSON(t, lect, fmt.Sprintf("%s/api/problems/%d/ta", f.re.ts.URL, f.pid), map[string]any{"user_id": taID}, http.StatusOK)

	ta := loginAs(t, f.re.ts, f.re.st, "ta-badge-view@ntu.edu.tw", "ta")
	got := getJSON[map[string]any](t, ta, fmt.Sprintf("%s/api/assessments/%d/ta-assignments", f.re.ts.URL, f.aid), http.StatusOK)
	rows, ok := got["assignments"].([]any)
	if !ok {
		t.Fatalf("response missing assignments array: %v", got)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 problem rows (1 assigned + 1 unassigned), got %d: %+v", len(rows), rows)
	}

	byProblem := map[int64]map[string]any{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		byProblem[int64(row["problem_id"].(float64))] = row
	}

	assigned, ok := byProblem[f.pid]
	if !ok {
		t.Fatalf("assigned problem %d missing from response: %+v", f.pid, rows)
	}
	if int(assigned["problem_number"].(float64)) != 1 {
		t.Errorf("assigned problem_number = %v, want 1", assigned["problem_number"])
	}
	if int64(assigned["user_id"].(float64)) != taID {
		t.Errorf("assigned user_id = %v, want %d", assigned["user_id"], taID)
	}
	if assigned["user_name"] != "Badge TA" {
		t.Errorf("assigned user_name = %v, want %q", assigned["user_name"], "Badge TA")
	}

	unassigned, ok := byProblem[pid2]
	if !ok {
		t.Fatalf("unassigned problem %d missing from response: %+v", pid2, rows)
	}
	if unassigned["user_id"] != nil {
		t.Errorf("unassigned user_id = %v, want null", unassigned["user_id"])
	}
	if unassigned["user_name"] != nil {
		t.Errorf("unassigned user_name = %v, want null", unassigned["user_name"])
	}
}

func TestTAAssignments_UnknownAssessment_404(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	ta := loginAs(t, f.re.ts, f.re.st, "ta-404@ntu.edu.tw", "ta")
	resp, err := ta.Get(fmt.Sprintf("%s/api/assessments/999999/ta-assignments", f.re.ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown assessment: got %d want 404", resp.StatusCode)
	}
}

func TestTAAssignments_RequiresSession(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	unauth := &http.Client{}
	resp, err := unauth.Get(fmt.Sprintf("%s/api/assessments/%d/ta-assignments", f.re.ts.URL, f.aid))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d want 401", resp.StatusCode)
	}
}

// --- gap 2: GET /api/graders -----------------------------------------------------------
//
// The TA-assignment picker's data source (lecturer+, distinct from admin-only
// GET /api/users): assignable graders (role >= TA) with MINIMAL fields only
// {id, name, role} -- no email/other PII.

func TestGraders_ListsTAAndAboveMinimalFields(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	taID := seedRoleUser(t, f.re.st, "grader-ta@ntu.edu.tw", "ta")
	lectID := seedRoleUser(t, f.re.st, "grader-lect@ntu.edu.tw", "lecturer")
	adminID := seedRoleUser(t, f.re.st, "grader-admin@ntu.edu.tw", "admin")

	lect := loginAs(t, f.re.ts, f.re.st, "lect-graders@ntu.edu.tw", "lecturer")
	got := getJSON[map[string]any](t, lect, f.re.ts.URL+"/api/graders", http.StatusOK)
	rows, ok := got["graders"].([]any)
	if !ok {
		t.Fatalf("response missing graders array: %v", got)
	}

	byID := map[int64]map[string]any{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		byID[int64(row["id"].(float64))] = row
	}
	for _, id := range []int64{taID, lectID, adminID} {
		if _, ok := byID[id]; !ok {
			t.Errorf("expected grader id %d in response, got %+v", id, rows)
		}
	}

	// Minimal-fields contract: exactly {id, name, role} -- no email or other PII, on
	// EVERY row (not just spot-checked ones).
	for _, raw := range rows {
		row := raw.(map[string]any)
		if len(row) != 3 {
			t.Fatalf("grader row has %d fields, want exactly 3 {id,name,role}: %+v", len(row), row)
		}
		for k := range row {
			if k != "id" && k != "name" && k != "role" {
				t.Errorf("unexpected field %q in grader row: %+v", k, row)
			}
		}
	}
}

func TestGraders_ExcludesStudentsAndInactive(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	// No "student" role exists in this system's role vocabulary (ta|lecturer|admin) --
	// the closest analogue to "excludes students" is an INACTIVE user, who must never
	// be offered as an assignee even though their role is nominally ta+.
	inactive, err := f.re.st.Q.CreateUser(t.Context(), db.CreateUserParams{
		Email: "grader-inactive@ntu.edu.tw", Role: "ta", Active: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	lect := loginAs(t, f.re.ts, f.re.st, "lect-graders2@ntu.edu.tw", "lecturer")
	got := getJSON[map[string]any](t, lect, f.re.ts.URL+"/api/graders", http.StatusOK)
	rows := got["graders"].([]any)
	for _, raw := range rows {
		row := raw.(map[string]any)
		if int64(row["id"].(float64)) == inactive.ID {
			t.Fatalf("inactive user must not appear in assignable graders: %+v", row)
		}
	}
}

func TestGraders_NoEmailLeaked(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	seedRoleUser(t, f.re.st, "grader-secret@ntu.edu.tw", "ta")
	lect := loginAs(t, f.re.ts, f.re.st, "lect-graders3@ntu.edu.tw", "lecturer")
	raw := getJSONRaw(t, lect, f.re.ts.URL+"/api/graders")
	if strings.Contains(string(raw), "grader-secret@ntu.edu.tw") || strings.Contains(string(raw), "\"email\"") {
		t.Fatalf("graders response leaked email/PII: %s", raw)
	}
}

func TestGraders_RequiresLecturer(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	ta := loginAs(t, f.re.ts, f.re.st, "ta-graders-403@ntu.edu.tw", "ta")
	resp, err := ta.Get(f.re.ts.URL + "/api/graders")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("TA listing graders: got %d want 403 (lecturer+ only)", resp.StatusCode)
	}
}

func TestGraders_DoesNotRelaxUsersEndpoint(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	lect := loginAs(t, f.re.ts, f.re.st, "lect-graders4@ntu.edu.tw", "lecturer")
	resp, err := lect.Get(f.re.ts.URL + "/api/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lecturer hitting /api/users: got %d want 403 (still admin-only)", resp.StatusCode)
	}
}

// --- gap 3: structured send-result 409 --------------------------------------------------
//
// The 409 body now carries {error, unverdicted:[{problem_id, problem_number}]} computed
// from the sub-items lacking a verdict, so the UI can render the per-problem checklist
// authoritatively instead of only deriving it client-side. The 200 path is unchanged
// (already covered by TestSendResult_GatedUntilAllVerdicted et al.).

func TestSendResult_409IncludesUnverdictedProblems(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-409-payload-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-409@ntu.edu.tw", "ta")

	resp := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("send-result before verdict: got %d want 409", resp.StatusCode)
	}
	var body struct {
		Error       string `json:"error"`
		Unverdicted []struct {
			ProblemID     int64 `json:"problem_id"`
			ProblemNumber int32 `json:"problem_number"`
		} `json:"unverdicted"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body.Error == "" {
		t.Errorf("409 body missing error string: %+v", body)
	}
	if len(body.Unverdicted) != 1 {
		t.Fatalf("expected 1 unverdicted problem, got %d: %+v", len(body.Unverdicted), body.Unverdicted)
	}
	if body.Unverdicted[0].ProblemNumber != 1 {
		t.Errorf("unverdicted[0].problem_number = %d, want 1", body.Unverdicted[0].ProblemNumber)
	}

	sub, err := f.re.st.GetRequestProblem(t.Context(), subID)
	if err != nil {
		t.Fatal(err)
	}
	if body.Unverdicted[0].ProblemID != sub.ProblemID {
		t.Errorf("unverdicted[0].problem_id = %d, want %d", body.Unverdicted[0].ProblemID, sub.ProblemID)
	}
}

// TestSendResult_409UnverdictedShrinksAsProblemsAreVerdicted: with multiple sub-items,
// the unverdicted list only names the STILL-unverdicted ones, not already-verdicted ones.
func TestSendResult_409UnverdictedShrinksAsProblemsAreVerdicted(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	p2 := postExpect(t, f.c, fmt.Sprintf("%s/api/assessments/%d/problems", f.re.ts.URL, f.aid),
		map[string]any{"number": 2, "title": "DP", "max_points": "10"}, http.StatusCreated)
	pid2 := int64(p2["id"].(float64))

	resp := postWebhook(t, f.re.ts, "s3cr3t",
		inboundPayloadWithBody(f.studentEmail, f.token, true, true, "Re: results",
			"<p1>\nfirst complaint\n</p1>\n<p2>\nsecond complaint\n</p2>", "pm-409-multi-1"))
	resp.Body.Close()
	listed, err := f.re.st.ListRegradeRequests(t.Context(), store.ListRegradeRequestsFilters{Kind: regradeKindFiled})
	if err != nil || len(listed) == 0 {
		t.Fatalf("no filed request after webhook: %v", err)
	}
	id := listed[0].ID
	subs, err := f.re.st.ListRequestProblems(t.Context(), id)
	if err != nil || len(subs) != 2 {
		t.Fatalf("expected 2 sub-items, got %d: %v", len(subs), err)
	}
	var subForP1 int64
	for _, s := range subs {
		if s.ProblemID == f.pid {
			subForP1 = s.ID
		}
	}
	if subForP1 == 0 {
		t.Fatalf("could not find sub-item for problem 1 among %+v", subs)
	}
	_ = pid2

	ta := loginAs(t, f.re.ts, f.re.st, "ta-409-multi@ntu.edu.tw", "ta")
	// Verdict problem 1 only; problem 2 stays unverdicted.
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subForP1),
		map[string]any{"outcome": "upheld", "note": "ok"}, http.StatusOK)

	resp2 := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("send-result with 1 of 2 verdicted: got %d want 409", resp2.StatusCode)
	}
	var body struct {
		Unverdicted []struct {
			ProblemID     int64 `json:"problem_id"`
			ProblemNumber int32 `json:"problem_number"`
		} `json:"unverdicted"`
	}
	if err := decodeJSONResp(t, resp2, &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if len(body.Unverdicted) != 1 {
		t.Fatalf("expected exactly 1 still-unverdicted problem, got %d: %+v", len(body.Unverdicted), body.Unverdicted)
	}
	if body.Unverdicted[0].ProblemNumber != 2 {
		t.Errorf("remaining unverdicted problem_number = %d, want 2", body.Unverdicted[0].ProblemNumber)
	}
}

// TestSendResult_409ZeroSubItems_EmptyUnverdictedList: the vacuous-truth trap (zero
// sub-items) still 409s but with an EMPTY unverdicted list, not a crash or nil-vs-omitted
// ambiguity that would confuse the UI's checklist render.
func TestSendResult_409ZeroSubItems_EmptyUnverdictedList(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-409-zero-1")
	if _, err := f.re.st.Pool.Exec(t.Context(), "DELETE FROM regrade_request_problems WHERE id = $1", subID); err != nil {
		t.Fatal(err)
	}
	ta := loginAs(t, f.re.ts, f.re.st, "ta-409-zero@ntu.edu.tw", "ta")
	resp := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("zero sub-items: got %d want 409", resp.StatusCode)
	}
	var body struct {
		Unverdicted []struct {
			ProblemID int64 `json:"problem_id"`
		} `json:"unverdicted"`
	}
	if err := decodeJSONResp(t, resp, &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if len(body.Unverdicted) != 0 {
		t.Errorf("zero sub-items should yield an empty unverdicted list, got %+v", body.Unverdicted)
	}
}

// TestSendResult_200PathUnchanged_NoUnverdictedField: a successful send's 200 body keeps
// its existing shape -- no stray unverdicted field bleeding into the success path.
func TestSendResult_200PathUnchanged_NoUnverdictedField(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-409-200-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-409-200@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	if _, ok := got["unverdicted"]; ok {
		t.Errorf("200 response must not carry an unverdicted field: %+v", got)
	}
	if int(got["turn"].(float64)) != 1 {
		t.Errorf("turn = %v, want 1 (200 path unchanged)", got["turn"])
	}
}

// --- F1: send-failure recovery path (result_sent_at + resend-result) ------------------
//
// Regrade v2 whole-branch review, Finding F1: the atomic flip-before-send (kept as the
// race arbiter) means a provider send failure after the flip leaves the request
// resolved with no email delivered and, before this fix, no way to tell that state
// apart from "resolved because we successfully sent" and no route to recover. These
// tests drive the new result_sent_at marker + POST /api/regrades/{id}/resend-result.

// TestSendResult_Success_SetsResultSentAt: a successful send-result records
// result_sent_at, and the detail JSON exposes it.
func TestSendResult_Success_SetsResultSentAt(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-sentat-ok-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-sentat-ok@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	rr, err := f.re.st.GetRegradeRequest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !rr.ResultSentAt.Valid {
		t.Fatal("a successful send-result must set result_sent_at")
	}

	resp, err := ta.Get(fmt.Sprintf("%s/api/regrades/%d", f.re.ts.URL, id))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var detail map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail["result_sent_at"] == nil {
		t.Errorf("detail JSON must expose result_sent_at after a successful send, got %+v", detail)
	}
}

// TestSendResult_ProviderFailure_ResultSentAtStaysNil_ThenResendRecovers: a provider
// failure after the atomic flip leaves result_sent_at NULL (the recoverable state);
// resend-result then delivers the email and sets the marker.
func TestSendResult_ProviderFailure_ResultSentAtStaysNil_ThenResendRecovers(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-sentat-fail-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-sentat-fail@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	f.re.failNextSend(1)
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	r.Body.Close()
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("provider-failed send-result: got %d want 500", r.StatusCode)
	}

	rr, err := f.re.st.GetRegradeRequest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rr.ResultSentAt.Valid {
		t.Fatal("a failed provider send must NOT set result_sent_at")
	}
	if rr.Status != regradeStatusResolvedUpheld {
		t.Fatalf("status must stay resolved after the failed send, got %q", rr.Status)
	}

	before := len(f.re.sentEmails())
	got := postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)
	_ = got
	after := f.re.sentEmails()
	if len(after) != before+1 {
		t.Fatalf("resend-result should send exactly one email, got %d new sends", len(after)-before)
	}

	rr2, err := f.re.st.GetRegradeRequest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !rr2.ResultSentAt.Valid {
		t.Fatal("resend-result must set result_sent_at on success")
	}
}

// TestResendResult_SecondCall_409: once recovered, a second resend-result 409s (the
// marker is already set — nothing to recover).
func TestResendResult_SecondCall_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-resend-twice-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-resend-twice@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	f.re.failNextSend(1)
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{})
	r.Body.Close()

	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	r2 := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), map[string]any{})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("resend-result once the marker is set: got %d want 409", r2.StatusCode)
	}
}

// TestResendResult_HealthyRequest_409: resend-result on a request whose result was
// already successfully delivered (marker set on the FIRST send, not a recovery) also
// 409s — the route is only for the "sent nothing yet" case.
func TestResendResult_HealthyRequest_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, subID := fileRequest(t, f, f.token, "pm-resend-healthy-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-resend-healthy@ntu.edu.tw", "ta")
	putJSONExpect(t, ta, http.MethodPatch, fmt.Sprintf("%s/api/regrades/%d/problems/%d", f.re.ts.URL, id, subID),
		map[string]any{"outcome": "upheld"}, http.StatusOK)

	postExpect(t, ta, fmt.Sprintf("%s/api/regrades/%d/send-result", f.re.ts.URL, id), map[string]any{}, http.StatusOK)

	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), map[string]any{})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("resend-result on an already-delivered request: got %d want 409", r.StatusCode)
	}
}

// TestResendResult_NotFiled_409: resend-result on a non-filed (e.g. unparsed) row 409s.
func TestResendResult_NotResolved_409(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-resend-open-1")
	ta := loginAs(t, f.re.ts, f.re.st, "ta-resend-open@ntu.edu.tw", "ta")

	// Never verdicted / never sent — request is still open (received).
	r := postJSON(t, ta, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), map[string]any{})
	defer r.Body.Close()
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("resend-result on a still-open (never sent) request: got %d want 409", r.StatusCode)
	}
}

// TestResendResult_RequiresTAOrAbove: unauthenticated resend-result is rejected.
func TestResendResult_RequiresTAOrAbove(t *testing.T) {
	f := regradeFixtureSetup(t, map[string]string{"ADAMARKER_INBOUND_WEBHOOK_SECRET": "s3cr3t"})
	id, _ := fileRequest(t, f, f.token, "pm-resend-rbac-1")
	unauth := &http.Client{}
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/regrades/%d/resend-result", f.re.ts.URL, id), nil)
	req.Header.Set("X-ADA-CSRF", "1")
	resp, _ := unauth.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated resend-result: got %d want 401", resp.StatusCode)
	}
}

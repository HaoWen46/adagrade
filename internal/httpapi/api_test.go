package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaoWen46/adagrade/internal/blobstore"
	"github.com/HaoWen46/adagrade/internal/config"
	"github.com/HaoWen46/adagrade/internal/domain"
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

// testEnv is the fully-wired API under test: real store/sessions/queue against the
// test DB, fake renderer + fake vision provider, dev-login enabled.
type testEnv struct {
	ts       *httptest.Server
	st       *store.Store
	runner   *grading.Runner
	fakeProv *fake.Provider
	ing      *ingest.Service
	scans    *scan.Service
	blobs    blobstore.Store        // same masked-blob store the runner reads (F17 tests build their own runner)
	sender   *publish.Sender        // publish send seam; drive it directly to simulate the email queue worker
	email    *countingEmailProvider // the Server's provider; its counter observes async email-login sends
	outbox   string
}

// sendPublishItem drives the generation-aware publish sender synchronously. The real
// River worker supplies its durable job id; HTTP tests use a deterministic synthetic
// id because their queue is intentionally not started.
func sendPublishItem(t *testing.T, env *testEnv, itemID int64, final bool) error {
	t.Helper()
	return sendPublishItemWith(t, env.st, env.sender, itemID, final)
}

// isFirstAttempt is always true here: every httpapi test drives a single simulated
// send per item/generation, so each call represents that job's first (and only)
// execution (A2's rescue discriminator has no bearing on these non-legacy rows).
func sendPublishItemWith(t *testing.T, st *store.Store, sender *publish.Sender, itemID int64, final bool) error {
	t.Helper()
	item, err := st.Q.GetPublishItem(t.Context(), itemID)
	if err != nil {
		return err
	}
	jobID := itemID*1_000_000 + int64(item.EmailGeneration)
	return sender.SendItem(t.Context(), publish.DeliveryRef{
		ItemID: itemID, Generation: item.EmailGeneration,
	}, jobID, final, true)
}

// countingEmailProvider wraps the file provider and counts Send attempts, so
// tests of the asynchronous email-login path can wait for "the handler's
// goroutine reached Send" instead of guessing with sleeps.
type countingEmailProvider struct {
	domain.EmailProvider
	attempts atomic.Int64
}

func (p *countingEmailProvider) Send(ctx context.Context, msg domain.OutboundEmail) (string, error) {
	id, err := p.EmailProvider.Send(ctx, msg)
	p.attempts.Add(1)
	return id, err
}

func harnessEnv(t *testing.T) *testEnv {
	t.Helper()
	return harnessEnvWithEnv(t, nil)
}

// harnessEnvWithEnv is harnessEnv with additional env vars fed into
// config.Load — for tests that need a non-default Config (e.g.
// ADAMARKER_MONTHLY_BUDGET_USD for the D36 monthly cap tests). extra always
// wins over the dev-login default; extra["ADAMARKER_DEV_LOGIN"] can't unset it
// since these tests need dev-login to authenticate.
func harnessEnvWithEnv(t *testing.T, extra map[string]string) *testEnv {
	t.Helper()
	s := storetest.Fresh(t)

	cfg, err := config.Load(func(k string) string {
		if k == "ADAMARKER_DEV_LOGIN" {
			return "1"
		}
		if v, ok := extra[k]; ok {
			return v
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}

	blobs, err := blobstore.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Most HTTP direct-upload fixtures define one problem. Keep their synthetic
	// whole-assessment PDFs one page too; tests that intentionally define a
	// two-problem paper override this ingest renderer explicitly.
	ing := &ingest.Service{Store: s, Blobs: blobs, Renderer: render.NewFake(1)}

	// A real (unstarted) queue + a runner on the fake provider: API tests can launch
	// runs and drive Plan/ExecuteLeaf synchronously without River workers. Provider
	// CRUD endpoints get a real DB-backed source with a throwaway master key.
	fakeProv := &fake.Provider{}
	fakeProv2 := &fake.Provider{NameStr: "fake2", ScorePerCriterion: "2"}
	staticSource := llm.StaticSource{"fake": fakeProv, "fake2": fakeProv2}
	runner := &grading.Runner{Store: s, Blobs: blobs, Providers: staticSource}
	scans := &scan.Service{
		Store: s, Blobs: blobs, Renderer: render.NewFake(2),
		Providers: staticSource, Ingest: ing,
	}
	key, err := secrets.LoadOrCreateKey(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}

	// Publish send pipeline (spec §3): a file-provider email sender so the "email"
	// queue and its worker are registered — publish handlers enqueue real send jobs.
	// Tests drive the send worker synchronously via sendPublishEmails when they need
	// delivery; most publish tests only assert item/batch state and never run it.
	outboxDir := t.TempDir()
	emailProv, err := email.New(email.Config{Provider: "file", OutboxDir: outboxDir})
	if err != nil {
		t.Fatal(err)
	}
	tokenKey := secrets.Derive(key, "regrade-token-v1")
	emailSender := publish.NewSender(s, emailProv, tokenKey, cfg.RegradeWindow, "inbound.example.edu", nil, blobs, cfg.ReportFontPath)
	qc, err := queue.New(s.Pool, queue.Deps{
		Runner: runner, Scans: scans, Ingest: ing,
		Email: emailSender, EmailRate: cfg.Email.Rate,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	countingProv := &countingEmailProvider{EmailProvider: emailProv}
	srv := New(cfg, Deps{
		Store: s, Ingest: ing, Scans: scans, Queue: qc,
		Providers: registry.NewDBSource(s, key), SecretKey: key,
		EmailProvider: countingProv,
	})
	ts := httptest.NewServer(srv.Handler(http.NotFoundHandler()))
	t.Cleanup(ts.Close)

	return &testEnv{ts: ts, st: s, runner: runner, fakeProv: fakeProv, ing: ing, scans: scans, blobs: blobs, sender: emailSender, email: countingProv, outbox: outboxDir}
}

// harness keeps the original 3-tuple shape used by most tests.
func harness(t *testing.T) (*httptest.Server, *http.Client, *store.Store) {
	t.Helper()
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	return e.ts, &http.Client{Jar: jar}, e.st
}

// bootstrapAdmin seeds an active admin directly through the store.
func bootstrapAdmin(t *testing.T, s *store.Store, email string) {
	t.Helper()
	if _, err := s.Q.UpsertActiveAdmin(t.Context(), email); err != nil {
		t.Fatal(err)
	}
}

// postJSON sends a JSON POST with the CSRF header (like the SPA does).
func postJSON(t *testing.T, c *http.Client, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func devLogin(t *testing.T, ts *httptest.Server, c *http.Client, email string) *http.Response {
	t.Helper()
	return postJSON(t, c, ts.URL+"/auth/dev-login", map[string]string{"email": email})
}

func TestAuthModes_ReflectsConfig(t *testing.T) {
	ts, c, _ := harness(t)
	resp, err := c.Get(ts.URL + "/api/auth/modes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m struct{ Email, Google, Dev bool }
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if !m.Email || m.Google || !m.Dev {
		t.Errorf("modes: got email=%v google=%v dev=%v, want email=true google=false dev=true", m.Email, m.Google, m.Dev)
	}
}

func TestMe_Requires_Session(t *testing.T) {
	ts, c, _ := harness(t)
	resp, err := c.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/me without session: got %d want 401", resp.StatusCode)
	}
}

func TestDevLogin_AllowlistedOnly(t *testing.T) {
	ts, c, _ := harness(t)

	// Nobody on the allowlist yet.
	resp := devLogin(t, ts, c, "stranger@ntu.edu.tw")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("dev-login for unlisted email: got %d want 403", resp.StatusCode)
	}
}

func TestEmailLogin_AllowlistedUserGetsLinkAndSession(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	bootstrapAdmin(t, e.st, "boss@ntu.edu.tw")

	resp := postJSON(t, c, e.ts.URL+"/auth/email-login", map[string]string{
		"email": "boss@ntu.edu.tw", "return_to": "/runs?scope=mine#active",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("email login request: got %d want 204", resp.StatusCode)
	}

	waitForLoginEmails(t, e.outbox, 1)
	link, token := readLoginToken(t, e.outbox)
	linkURL, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := linkURL.Query().Get("return_to"); got != "/runs?scope=mine#active" {
		t.Fatalf("email link return_to = %q, want preserved internal route", got)
	}

	// A mail-scanner prefetch is a plain GET on the emailed link (the SPA route
	// is a 404 stub in tests; the status is irrelevant). It must not consume the
	// token — only the SPA's explicit POST callback below does.
	prefetch, err := c.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	prefetch.Body.Close()

	cb := postJSON(t, c, e.ts.URL+"/auth/email-callback", map[string]string{"token": token})
	cb.Body.Close()
	if cb.StatusCode != http.StatusNoContent {
		t.Fatalf("callback status: got %d want 204", cb.StatusCode)
	}

	me, err := c.Get(e.ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer me.Body.Close()
	var body struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := json.NewDecoder(me.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.User.Email != "boss@ntu.edu.tw" || body.User.Role != "admin" {
		t.Fatalf("me after email login = %+v", body.User)
	}

	second := postJSON(t, c, e.ts.URL+"/auth/email-callback", map[string]string{"token": token})
	second.Body.Close()
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed token: got %d want 400", second.StatusCode)
	}
}

func TestEmailLogin_RateLimitCapsEmailsAtThree(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	bootstrapAdmin(t, e.st, "boss@ntu.edu.tw")

	for i := 0; i < 4; i++ {
		resp := postJSON(t, c, e.ts.URL+"/auth/email-login", map[string]string{"email": "boss@ntu.edu.tw"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("email login request %d: got %d want 204", i+1, resp.StatusCode)
		}
	}

	waitForLoginEmails(t, e.outbox, 3)
	// Sends are asynchronous: give a straggling 4th goroutine time to (wrongly)
	// deliver before asserting the cap held.
	time.Sleep(500 * time.Millisecond)
	if got := emlFiles(t, e.outbox); len(got) != 3 {
		t.Fatalf("outbox emails after 4 requests = %d, want 3", len(got))
	}
}

// TestEmailLogin_FailedSendFreesRateLimitSlot pins the cleanup path: a token
// whose email never went out must not count toward the 3-active cap, otherwise
// a few SMTP failures lock the user out of email login for the whole TTL.
func TestEmailLogin_FailedSendFreesRateLimitSlot(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	u, err := e.st.Q.UpsertActiveAdmin(t.Context(), "boss@ntu.edu.tw")
	if err != nil {
		t.Fatal(err)
	}

	// An unwritable outbox makes the file provider's Send fail: os.WriteFile
	// needs write permission on the directory.
	if err := os.Chmod(e.outbox, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.outbox, 0o700) })

	resp := postJSON(t, c, e.ts.URL+"/auth/email-login", map[string]string{"email": "boss@ntu.edu.tw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("email login with failing send: got %d want 204 (failure must not leak)", resp.StatusCode)
	}

	// Wait for the async send attempt (so the token row provably existed), then
	// for the failed send to delete it and free the cap slot.
	waitForSendAttempts(t, e.email, 1)
	waitForActiveLoginTokens(t, e.st, u.ID, 0)

	if err := os.Chmod(e.outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	resp = postJSON(t, c, e.ts.URL+"/auth/email-login", map[string]string{"email": "boss@ntu.edu.tw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("email login after restore: got %d want 204", resp.StatusCode)
	}
	waitForLoginEmails(t, e.outbox, 1)
}

func TestEmailLogin_DoesNotRevealUnallowlistedEmail(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	resp := postJSON(t, c, e.ts.URL+"/auth/email-login", map[string]string{"email": "stranger@ntu.edu.tw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unallowlisted email login request: got %d want 204", resp.StatusCode)
	}
	// The (no-op) send path is asynchronous; give a wrong implementation time to
	// deliver before asserting the outbox stayed empty.
	time.Sleep(500 * time.Millisecond)
	if got := emlFiles(t, e.outbox); len(got) != 0 {
		t.Fatalf("outbox emails for unallowlisted user = %d, want 0", len(got))
	}
}

func TestEmailLogin_ExpiredTokenDoesNotCreateSession(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	u, err := e.st.Q.UpsertActiveAdmin(t.Context(), "boss@ntu.edu.tw")
	if err != nil {
		t.Fatal(err)
	}
	token, hash, err := newLoginToken()
	if err != nil {
		t.Fatal(err)
	}
	created, err := e.st.CreateLoginToken(t.Context(), u.ID, hash, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("CreateLoginToken: created = false, want true")
	}

	resp := postJSON(t, c, e.ts.URL+"/auth/email-callback", map[string]string{"token": token})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired token: got %d want 400", resp.StatusCode)
	}

	me, err := c.Get(e.ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if me.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/me after expired token: got %d want 401", me.StatusCode)
	}
}

func TestEmailLogin_CallbackWithoutCSRFHeaderForbidden(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	b, _ := json.Marshal(map[string]string{"token": "whatever"})
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/auth/email-callback", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("callback without CSRF header: got %d want 403", resp.StatusCode)
	}
}

func TestEmailLogin_InactiveUserCannotCompleteSignIn(t *testing.T) {
	e := harnessEnv(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	u, err := e.st.Q.CreateUser(t.Context(), db.CreateUserParams{
		Email: "gone@ntu.edu.tw", Role: "ta", Active: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, hash, err := newLoginToken()
	if err != nil {
		t.Fatal(err)
	}
	created, err := e.st.CreateLoginToken(t.Context(), u.ID, hash, time.Now().Add(emailLoginTTL))
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("CreateLoginToken: created = false, want true")
	}

	resp := postJSON(t, c, e.ts.URL+"/auth/email-callback", map[string]string{"token": token})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("inactive user callback: got %d want 403", resp.StatusCode)
	}

	me, err := c.Get(e.ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if me.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/me after inactive-user callback: got %d want 401", me.StatusCode)
	}
}

func TestEmailLoginURL_TrustedRequestHostUsesCurrentHTTPSHost(t *testing.T) {
	s := &Server{cfg: config.Config{Env: config.EnvProduction, EmailLoginTrustRequestHost: true}}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/auth/email-login", nil)
	r.Host = "current-tunnel.lhr.life"

	base, err := s.emailLoginBaseURL(r)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://current-tunnel.lhr.life/login/email?token=token123"
	if got := emailLoginLink(base, "token123", ""); got != want {
		t.Fatalf("emailLoginLink = %q, want %q", got, want)
	}
}

func TestEmailLoginURL_TrustedRequestHostHonorsForwardedHeaders(t *testing.T) {
	s := &Server{cfg: config.Config{Env: config.EnvProduction, EmailLoginTrustRequestHost: true}}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/auth/email-login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "marker.example.edu")

	base, err := s.emailLoginBaseURL(r)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://marker.example.edu/login/email?token=token123"
	if got := emailLoginLink(base, "token123", ""); got != want {
		t.Fatalf("emailLoginLink = %q, want %q", got, want)
	}
}

// emlFiles lists the completed .eml messages in the outbox. The suffix filter
// matters: FileProvider writes temp-then-rename, so a transient *.eml.tmp entry
// can coexist with the finished messages and must not count.
func emlFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".eml") {
			names = append(names, e.Name())
		}
	}
	return names
}

// waitForLoginEmails polls until the outbox holds n .eml files (the login send
// is asynchronous — the 204 races the write). Overshooting n fails immediately.
func waitForLoginEmails(t *testing.T, dir string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		names := emlFiles(t, dir)
		if len(names) > n {
			t.Fatalf("outbox emails = %d, want %d", len(names), n)
		}
		if len(names) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d login emails, have %d", n, len(names))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForSendAttempts polls until the Server's email provider has seen n Send
// calls, successful or not.
func waitForSendAttempts(t *testing.T, p *countingEmailProvider, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.attempts.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d send attempts, have %d", n, p.attempts.Load())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForActiveLoginTokens polls until the user holds exactly n active
// (unconsumed, unexpired) login tokens.
func waitForActiveLoginTokens(t *testing.T, st *store.Store, userID int64, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got int
		err := st.Pool.QueryRow(t.Context(), `
			SELECT count(*) FROM login_tokens
			WHERE user_id = $1 AND consumed_at IS NULL AND expires_at > now()
		`, userID).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d active login tokens, have %d", n, got)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// readLoginToken extracts the sign-in link and token from the single .eml in the
// outbox, asserting the link lands on the SPA's /login/email page (not a
// consuming GET).
func readLoginToken(t *testing.T, outbox string) (link, token string) {
	t.Helper()
	names := emlFiles(t, outbox)
	if len(names) != 1 {
		t.Fatalf("outbox emails = %d, want 1", len(names))
	}
	raw, err := os.ReadFile(filepath.Join(outbox, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`https?://[^ \r\n<]+/login/email\?[^ \r\n<]+`)
	link = re.FindString(string(raw))
	if link == "" {
		t.Fatalf("login link not found in email:\n%s", string(raw))
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link %q: %v", link, err)
	}
	if u.Path != "/login/email" || u.Query().Get("token") == "" {
		t.Fatalf("bad login link: %s", link)
	}
	return link, u.Query().Get("token")
}

func TestUsersCRUD_AdminOnly_And_MeFlow(t *testing.T) {
	ts, c, st := harness(t)
	bootstrapAdmin(t, st, "boss@ntu.edu.tw")

	// Admin logs in.
	resp := devLogin(t, ts, c, "boss@ntu.edu.tw")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin dev-login: got %d", resp.StatusCode)
	}

	// /api/me reflects the session.
	me, err := c.Get(ts.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	var meBody struct {
		User struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := json.NewDecoder(me.Body).Decode(&meBody); err != nil {
		t.Fatal(err)
	}
	me.Body.Close()
	if meBody.User.Email != "boss@ntu.edu.tw" || meBody.User.Role != "admin" {
		t.Errorf("/api/me: got %+v", meBody)
	}

	// Admin creates a TA (email arrives denormalized; server must normalize).
	resp = postJSON(t, c, ts.URL+"/api/users", map[string]string{
		"email": "  TA@ntu.edu.tw ", "role": "ta", "display_name": "The TA",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create user: got %d", resp.StatusCode)
	}
	var created struct {
		ID    int64  `json:"id"`
		Email string `json:"email"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Email != "ta@ntu.edu.tw" {
		t.Errorf("created email not normalized: %q", created.Email)
	}

	// Invalid role rejected.
	resp = postJSON(t, c, ts.URL+"/api/users", map[string]string{"email": "x@ntu.edu.tw", "role": "root"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid role: got %d want 400", resp.StatusCode)
	}

	// TA session cannot manage users.
	jar, _ := cookiejar.New(nil)
	taClient := &http.Client{Jar: jar}
	resp = devLogin(t, ts, taClient, "ta@ntu.edu.tw")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ta dev-login: got %d", resp.StatusCode)
	}
	resp = postJSON(t, taClient, ts.URL+"/api/users", map[string]string{"email": "y@ntu.edu.tw", "role": "ta"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("ta creating users: got %d want 403", resp.StatusCode)
	}

	// Admin cannot deactivate themself.
	var adminID int64
	list, _ := c.Get(ts.URL + "/api/users")
	var users struct {
		Users []struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
		} `json:"users"`
	}
	_ = json.NewDecoder(list.Body).Decode(&users)
	list.Body.Close()
	for _, u := range users.Users {
		if u.Email == "boss@ntu.edu.tw" {
			adminID = u.ID
		}
	}
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/users/%d", ts.URL, adminID), bytes.NewReader([]byte(`{"active": false}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("self-deactivation: got %d want 400", resp.StatusCode)
	}
}

// Only admins may manage users/roles — lecturers and TAs are both refused
// (the user explicitly wants role changes to be admin-only).
func TestUsers_RoleChangesAreAdminOnly(t *testing.T) {
	ts, c, st := harness(t)
	bootstrapAdmin(t, st, "boss@ntu.edu.tw")
	resp := devLogin(t, ts, c, "boss@ntu.edu.tw")
	resp.Body.Close()

	// Admin creates a lecturer and a TA.
	for _, u := range []map[string]string{
		{"email": "lect@ntu.edu.tw", "role": "lecturer"},
		{"email": "ta@ntu.edu.tw", "role": "ta"},
	} {
		r := postJSON(t, c, ts.URL+"/api/users", u)
		r.Body.Close()
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create %s: got %d", u["role"], r.StatusCode)
		}
	}
	var taID int64
	list, _ := c.Get(ts.URL + "/api/users")
	var users struct {
		Users []struct {
			ID    int64  `json:"id"`
			Email string `json:"email"`
		} `json:"users"`
	}
	_ = json.NewDecoder(list.Body).Decode(&users)
	list.Body.Close()
	for _, u := range users.Users {
		if u.Email == "ta@ntu.edu.tw" {
			taID = u.ID
		}
	}

	// A LECTURER must not be able to touch users at all (list, create, or role change).
	jar, _ := cookiejar.New(nil)
	lect := &http.Client{Jar: jar}
	resp = devLogin(t, ts, lect, "lect@ntu.edu.tw")
	resp.Body.Close()

	lr, _ := lect.Get(ts.URL + "/api/users")
	lr.Body.Close()
	if lr.StatusCode != http.StatusForbidden {
		t.Errorf("lecturer listing users: got %d want 403", lr.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/api/users/%d", ts.URL, taID),
		bytes.NewReader([]byte(`{"role":"lecturer"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ADA-CSRF", "1")
	pr, err := lect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body.Close()
	if pr.StatusCode != http.StatusForbidden {
		t.Errorf("lecturer changing a role: got %d want 403", pr.StatusCode)
	}
}

func TestCSRF_HeaderRequiredOnMutations(t *testing.T) {
	ts, c, st := harness(t)
	bootstrapAdmin(t, st, "boss@ntu.edu.tw")
	resp := devLogin(t, ts, c, "boss@ntu.edu.tw")
	resp.Body.Close()

	// POST without the custom header must be rejected.
	b, _ := json.Marshal(map[string]string{"email": "z@ntu.edu.tw", "role": "ta"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/users", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("mutation without CSRF header: got %d want 403", resp.StatusCode)
	}
}

func TestLogout_EndsSession(t *testing.T) {
	ts, c, st := harness(t)
	bootstrapAdmin(t, st, "boss@ntu.edu.tw")
	resp := devLogin(t, ts, c, "boss@ntu.edu.tw")
	resp.Body.Close()

	resp = postJSON(t, c, ts.URL+"/auth/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: got %d", resp.StatusCode)
	}
	me, _ := c.Get(ts.URL + "/api/me")
	me.Body.Close()
	if me.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after logout: got %d want 401", me.StatusCode)
	}
}

func TestReadyz_ChecksDB(t *testing.T) {
	ts, c, _ := harness(t)
	resp, err := c.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz: got %d want 200", resp.StatusCode)
	}
}

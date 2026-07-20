package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
)

// --- test doubles -----------------------------------------------------------

// fakeExchanger stands in for Google's token endpoint. It enforces the PKCE
// relation: the verifier the flow sends here must hash (S256) to the challenge the
// flow previously sent in the login redirect.
type fakeExchanger struct {
	wantCode      string
	wantChallenge string
	idToken       string
	err           error
}

func (f *fakeExchanger) Exchange(ctx context.Context, code, verifier string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if code != f.wantCode {
		return "", fmt.Errorf("unexpected code %q", code)
	}
	sum := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != f.wantChallenge {
		return "", fmt.Errorf("PKCE broken: S256(verifier)=%q, challenge sent to Google was %q", got, f.wantChallenge)
	}
	return f.idToken, nil
}

type fakeUserStore map[string]UserRecord

func (f fakeUserStore) ByEmail(_ context.Context, email string) (UserRecord, bool, error) {
	u, ok := f[email]
	return u, ok, nil
}

// unsignedToken builds a JWT-shaped token (header.payload.sig) with the given claims.
// Signature is bogus by design: the flow trusts the TLS channel to Google's token
// endpoint, not the signature (see oauth.go).
func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	seg := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return seg(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + seg(claims) + ".sig"
}

func validClaims(nonce string) map[string]any {
	return map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            "test-client-id",
		"exp":            time.Now().Add(time.Hour).Unix(),
		"email":          "ta@ntu.edu.tw",
		"email_verified": true,
		"hd":             "ntu.edu.tw",
		"nonce":          nonce,
		"name":           "Test TA",
	}
}

// flowHarness wires a Flow into an httptest server with real scs sessions.
func flowHarness(t *testing.T, ex Exchanger, users UserStore) (*httptest.Server, *http.Client) {
	t.Helper()
	sm := scs.New()

	f := &Flow{
		ClientID:     "test-client-id",
		AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		RedirectURL:  "http://app.local/auth/callback",
		HostedDomain: "ntu.edu.tw",
		Sessions:     sm,
		Exchange:     ex,
		Users:        users,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", f.BeginLogin)
	mux.HandleFunc("GET /auth/callback", f.Callback)
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		id := sm.GetInt64(r.Context(), SessionUserIDKey)
		fmt.Fprintf(w, "%d", id)
	})
	srv := httptest.NewServer(sm.LoadAndSave(mux))
	t.Cleanup(srv.Close)

	jar := newTestJar(t)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow redirects
		},
	}
	return srv, client
}

// beginLogin drives GET /auth/login and returns the parsed Google redirect URL.
func beginLogin(t *testing.T, srv *httptest.Server, client *http.Client) *url.URL {
	t.Helper()
	resp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: got status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// --- tests -------------------------------------------------------------------

func TestSafeReturnPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "path query and hash", raw: "/runs?scope=mine#active", want: "/runs?scope=mine#active"},
		{name: "absolute URL", raw: "https://evil.example/path", want: "/"},
		{name: "protocol relative", raw: "//evil.example/path", want: "/"},
		{name: "backslash authority", raw: `/\\evil.example/path`, want: "/"},
		{name: "encoded protocol relative", raw: "/%2f%2fevil.example/path", want: "/"},
		{name: "header injection", raw: "/ok\r\nLocation: https://evil.example", want: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeReturnPath(tc.raw); got != tc.want {
				t.Fatalf("SafeReturnPath(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestBeginLogin_RedirectsToGoogleWithPKCEStateNonceHd(t *testing.T) {
	srv, client := flowHarness(t, &fakeExchanger{}, fakeUserStore{})
	loc := beginLogin(t, srv, client)

	if loc.Host != "accounts.google.com" {
		t.Errorf("redirect host: %s", loc.Host)
	}
	q := loc.Query()
	if q.Get("client_id") != "test-client-id" ||
		q.Get("response_type") != "code" ||
		!strings.Contains(q.Get("scope"), "email") ||
		q.Get("redirect_uri") != "http://app.local/auth/callback" ||
		q.Get("hd") != "ntu.edu.tw" ||
		q.Get("code_challenge_method") != "S256" {
		t.Errorf("missing/incorrect standard params: %v", q)
	}
	for _, p := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(p)) < 20 {
			t.Errorf("param %s too short/missing: %q", p, q.Get(p))
		}
	}
}

func TestCallback_HappyPathLogsInAndRotatesSession(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	loc := beginLogin(t, srv, client)
	q := loc.Query()

	ex.wantChallenge = q.Get("code_challenge")
	ex.idToken = unsignedToken(t, validClaims(q.Get("nonce")))

	resp, err := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	who, err := client.Get(srv.URL + "/whoami")
	if err != nil {
		t.Fatal(err)
	}
	defer who.Body.Close()
	var buf [8]byte
	n, _ := who.Body.Read(buf[:])
	if got := string(buf[:n]); got != "42" {
		t.Errorf("session user id: got %q want 42", got)
	}
}

func TestCallback_ReturnsToRequestedInternalPath(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	resp, err := client.Get(srv.URL + "/auth/login?return_to=" + url.QueryEscape("/runs?scope=mine#active"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	ex.idToken = unsignedToken(t, validClaims(q.Get("nonce")))

	callback, err := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	if err != nil {
		t.Fatal(err)
	}
	callback.Body.Close()
	if got := callback.Header.Get("Location"); got != "/runs?scope=mine#active" {
		t.Fatalf("callback location = %q, want requested internal route", got)
	}
}

func TestCallback_RejectsExternalReturnPath(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	resp, err := client.Get(srv.URL + "/auth/login?return_to=" + url.QueryEscape("//evil.example/path"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	ex.idToken = unsignedToken(t, validClaims(q.Get("nonce")))

	callback, err := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	if err != nil {
		t.Fatal(err)
	}
	callback.Body.Close()
	if got := callback.Header.Get("Location"); got != "/" {
		t.Fatalf("callback location = %q, want safe root fallback", got)
	}
}

func TestCallback_RejectsWrongState(t *testing.T) {
	srv, client := flowHarness(t, &fakeExchanger{}, fakeUserStore{})
	beginLogin(t, srv, client)

	resp, err := client.Get(srv.URL + "/auth/callback?state=forged&code=auth-code")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusFound && strings.HasPrefix(resp.Header.Get("Location"), "/") && resp.Header.Get("Location") == "/" {
		t.Fatal("forged state must not log in")
	}
	assertNotLoggedIn(t, srv, client)
}

func TestCallback_RejectsWrongNonce(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	loc := beginLogin(t, srv, client)
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	ex.idToken = unsignedToken(t, validClaims("evil-nonce"))

	resp, _ := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	resp.Body.Close()
	assertNotLoggedIn(t, srv, client)
}

func TestCallback_RejectsNotAllowlisted(t *testing.T) {
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, fakeUserStore{}) // empty allowlist

	loc := beginLogin(t, srv, client)
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	ex.idToken = unsignedToken(t, validClaims(q.Get("nonce")))

	resp, _ := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "denied") {
		t.Errorf("expected redirect to /login?error=denied, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	assertNotLoggedIn(t, srv, client)
}

func TestCallback_RejectsExpiredToken(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	loc := beginLogin(t, srv, client)
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	claims := validClaims(q.Get("nonce"))
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	ex.idToken = unsignedToken(t, claims)

	resp, _ := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	resp.Body.Close()
	assertNotLoggedIn(t, srv, client)
}

func TestCallback_RejectsWrongAudience(t *testing.T) {
	users := fakeUserStore{"ta@ntu.edu.tw": {ID: 42, Email: "ta@ntu.edu.tw", Role: RoleTA, Active: true}}
	ex := &fakeExchanger{wantCode: "auth-code"}
	srv, client := flowHarness(t, ex, users)

	loc := beginLogin(t, srv, client)
	q := loc.Query()
	ex.wantChallenge = q.Get("code_challenge")
	claims := validClaims(q.Get("nonce"))
	claims["aud"] = "some-other-client"
	ex.idToken = unsignedToken(t, claims)

	resp, _ := client.Get(srv.URL + "/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=auth-code")
	resp.Body.Close()
	assertNotLoggedIn(t, srv, client)
}

func assertNotLoggedIn(t *testing.T, srv *httptest.Server, client *http.Client) {
	t.Helper()
	who, err := client.Get(srv.URL + "/whoami")
	if err != nil {
		t.Fatal(err)
	}
	defer who.Body.Close()
	var buf [8]byte
	n, _ := who.Body.Read(buf[:])
	if got := string(buf[:n]); got != "0" {
		t.Errorf("expected no session user, got %q", got)
	}
}

func newTestJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}

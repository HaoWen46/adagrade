package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/oauth2"
)

// The hardened login flow per docs/DECISIONS.md D7: random state bound to the
// pre-login session, PKCE (S256), an ID-token nonce, and session-token rotation on
// success. ID-token *signature* verification is deliberately skipped: the token
// arrives directly from Google's token endpoint over TLS in the code exchange, which
// authenticates its origin (per Google's own guidance for server-side flows); the
// claims (iss/aud/exp/nonce/email_verified) are still fully validated here.

// SessionUserIDKey is the scs key holding the logged-in user's DB id.
const SessionUserIDKey = "user_id"

const (
	sessKeyState    = "oauth_state"
	sessKeyVerifier = "oauth_verifier"
	sessKeyNonce    = "oauth_nonce"
	sessKeyReturnTo = "oauth_return_to"
)

// SafeReturnPath accepts only same-origin, root-relative paths. It is shared by
// both production sign-in flows so a caller-controlled return_to can never turn
// into an open redirect. Backslashes are rejected because browsers may normalize
// them as path separators while interpreting a Location header.
func SafeReturnPath(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.Contains(raw, `\`) || strings.ContainsAny(raw, "\r\n\x00") {
		return "/"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" ||
		!strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") ||
		strings.Contains(parsed.Path, `\`) {
		return "/"
	}
	return raw
}

// UserRecord is the allowlist row the flow resolves a Google identity against.
type UserRecord struct {
	ID          int64
	Email       string
	DisplayName string
	Role        Role
	Active      bool
}

// UserStore resolves a normalized email to its allowlist entry.
type UserStore interface {
	ByEmail(ctx context.Context, normalizedEmail string) (UserRecord, bool, error)
}

// Exchanger swaps an authorization code (+ PKCE verifier) for a raw ID token.
type Exchanger interface {
	Exchange(ctx context.Context, code, verifier string) (idToken string, err error)
}

// Flow implements /auth/login and /auth/callback.
type Flow struct {
	ClientID     string
	AuthURL      string // Google's authorization endpoint
	RedirectURL  string
	HostedDomain string
	Sessions     *scs.SessionManager
	Exchange     Exchanger
	Users        UserStore
	Logger       *slog.Logger
}

// GoogleAuthURL is the standard authorization endpoint.
const GoogleAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"

// BeginLogin generates state/nonce/PKCE material, stashes it in the (pre-)session,
// and redirects to Google.
func (f *Flow) BeginLogin(w http.ResponseWriter, r *http.Request) {
	state := randToken(32)
	nonce := randToken(32)
	verifier := randToken(48)

	ctx := r.Context()
	f.Sessions.Put(ctx, sessKeyState, state)
	f.Sessions.Put(ctx, sessKeyNonce, nonce)
	f.Sessions.Put(ctx, sessKeyVerifier, verifier)
	f.Sessions.Put(ctx, sessKeyReturnTo, SafeReturnPath(r.URL.Query().Get("return_to")))

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := url.Values{
		"client_id":             {f.ClientID},
		"redirect_uri":          {f.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	if f.HostedDomain != "" {
		q.Set("hd", f.HostedDomain)
	}
	http.Redirect(w, r, f.AuthURL+"?"+q.Encode(), http.StatusFound)
}

// Callback validates the handshake, exchanges the code, verifies claims, applies the
// allowlist policy, and establishes the session.
func (f *Flow) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := f.logger()

	wantState := f.Sessions.PopString(ctx, sessKeyState)
	wantNonce := f.Sessions.PopString(ctx, sessKeyNonce)
	verifier := f.Sessions.PopString(ctx, sessKeyVerifier)
	returnTo := SafeReturnPath(f.Sessions.PopString(ctx, sessKeyReturnTo))

	fail := func(code string, err error) {
		log.Warn("oauth callback rejected", "reason", code, "err", err)
		http.Redirect(w, r, "/login?error="+code, http.StatusFound)
	}

	if wantState == "" || subtleNeq(r.FormValue("state"), wantState) {
		fail("state", errors.New("state missing or mismatched"))
		return
	}
	code := r.FormValue("code")
	if code == "" {
		fail("denied", fmt.Errorf("provider returned error=%q", r.FormValue("error")))
		return
	}

	rawToken, err := f.Exchange.Exchange(ctx, code, verifier)
	if err != nil {
		fail("exchange", err)
		return
	}

	claims, err := parseIDToken(rawToken, f.ClientID, wantNonce, time.Now())
	if err != nil {
		fail("token", err)
		return
	}

	email := NormalizeEmail(claims.Email)
	rec, found, err := f.Users.ByEmail(ctx, email)
	if err != nil {
		fail("internal", err)
		return
	}
	decision := Authorize(Claims{
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		HostedDomain:  claims.Hd,
	}, f.HostedDomain, func(string) (AllowlistEntry, bool) {
		if !found {
			return AllowlistEntry{}, false
		}
		return AllowlistEntry{Role: rec.Role, Active: rec.Active}, true
	})
	if !decision.Authorized {
		// Reason is logged (user id, not PII beyond the allowlist decision), never
		// surfaced verbatim to the browser.
		fail("denied", fmt.Errorf("authorize: %s", decision.Reason))
		return
	}

	// Session fixation defense: rotate the token before elevating the session.
	if err := f.Sessions.RenewToken(ctx); err != nil {
		fail("internal", err)
		return
	}
	f.Sessions.Put(ctx, SessionUserIDKey, rec.ID)
	log.Info("login", "user_id", rec.ID, "role", string(rec.Role))
	http.Redirect(w, r, returnTo, http.StatusFound)
}

func (f *Flow) logger() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.Default()
}

// GoogleExchanger performs the real code exchange against Google's token endpoint.
type GoogleExchanger struct {
	Conf *oauth2.Config
}

// NewGoogleExchanger builds the oauth2 config for the standard Google endpoints.
func NewGoogleExchanger(clientID, clientSecret, redirectURL string) *GoogleExchanger {
	return &GoogleExchanger{Conf: &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  GoogleAuthURL,
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}}
}

func (g *GoogleExchanger) Exchange(ctx context.Context, code, verifier string) (string, error) {
	tok, err := g.Conf.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	idt, _ := tok.Extra("id_token").(string)
	if idt == "" {
		return "", errors.New("token response missing id_token")
	}
	return idt, nil
}

// idClaims is the validated subset of a Google ID token.
type idClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"` // string or []string per OIDC
	Exp           int64           `json:"exp"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	Hd            string          `json:"hd"`
	Nonce         string          `json:"nonce"`
	Name          string          `json:"name"`
}

// parseIDToken decodes the JWT payload and validates iss/aud/exp/nonce. See the
// package comment on why the signature is not re-verified here.
func parseIDToken(raw, wantAud, wantNonce string, now time.Time) (idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, errors.New("id token: not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, fmt.Errorf("id token payload: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return idClaims{}, fmt.Errorf("id token claims: %w", err)
	}

	if c.Iss != "https://accounts.google.com" && c.Iss != "accounts.google.com" {
		return idClaims{}, fmt.Errorf("id token: unexpected issuer %q", c.Iss)
	}
	if !audContains(c.Aud, wantAud) {
		return idClaims{}, errors.New("id token: audience mismatch")
	}
	if now.Unix() >= c.Exp {
		return idClaims{}, errors.New("id token: expired")
	}
	if wantNonce != "" && subtleNeq(c.Nonce, wantNonce) {
		return idClaims{}, errors.New("id token: nonce mismatch")
	}
	if c.Email == "" {
		return idClaims{}, errors.New("id token: no email claim")
	}
	return c, nil
}

func audContains(raw json.RawMessage, want string) bool {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == want
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, a := range list {
			if a == want {
				return true
			}
		}
	}
	return false
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// subtleNeq is a constant-time string inequality check.
func subtleNeq(a, b string) bool {
	if len(a) != len(b) {
		return true
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v != 0
}

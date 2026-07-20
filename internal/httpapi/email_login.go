package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HaoWen46/adagrade/internal/auth"
	"github.com/HaoWen46/adagrade/internal/domain"
	"github.com/HaoWen46/adagrade/internal/store"
)

const emailLoginTTL = 15 * time.Minute

func (s *Server) emailLoginEnabled() bool {
	if s.cfg.Env == "production" && s.cfg.AppBaseURL == "" && !s.cfg.EmailLoginTrustRequestHost {
		return false
	}
	return s.email != nil && s.cfg.Email.Provider != "none"
}

// handleEmailLogin requests a sign-in link. Anti-enumeration invariant: after
// input validation every response is an immediate 204 — the allowlist lookup,
// token mint, and email send all run in a detached goroutine — so neither the
// status code nor the response latency reveals whether an address is
// allowlisted.
func (s *Server) handleEmailLogin(w http.ResponseWriter, r *http.Request) {
	if !s.emailLoginEnabled() {
		apiError(w, http.StatusServiceUnavailable, "email login is not configured")
		return
	}

	var body struct {
		Email    string `json:"email"`
		ReturnTo string `json:"return_to"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	emailAddr := auth.NormalizeEmail(body.Email)
	if emailAddr == "" {
		apiError(w, http.StatusBadRequest, "email is required")
		return
	}

	// The link base reads the request (Host / forwarded headers), which must not
	// be touched after the handler returns — resolve it before going async.
	base, err := s.emailLoginBaseURL(r)
	if err != nil {
		// Unreachable in practice: emailLoginEnabled already rejects the
		// missing-base-URL misconfiguration. Still 204 — uniformity above all.
		s.log.Warn("email login base url unresolved", "err", err)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	go func() {
		defer cancel()
		s.sendLoginLink(ctx, emailAddr, base, auth.SafeReturnPath(body.ReturnTo))
	}()
	w.WriteHeader(http.StatusNoContent)
}

// sendLoginLink is handleEmailLogin's detached tail: allowlist lookup, token
// mint, and send. Every failure is logged (user_id only — never the address)
// and deliberately not surfaced to the client.
func (s *Server) sendLoginLink(ctx context.Context, emailAddr, base, returnTo string) {
	rec, found, err := userStore{s.store}.ByEmail(ctx, emailAddr)
	if err != nil {
		s.log.Warn("email login lookup failed", "err", err)
		return
	}
	if !found || !rec.Active {
		// Not allowlisted: drop silently. Only allowlisted active users get an email.
		return
	}

	token, tokenHash, err := newLoginToken()
	if err != nil {
		s.log.Warn("email login token generation failed", "user_id", rec.ID, "err", err)
		return
	}
	created, err := s.store.CreateLoginToken(ctx, rec.ID, tokenHash, time.Now().Add(emailLoginTTL))
	if err != nil {
		s.log.Warn("email login token store failed", "user_id", rec.ID, "err", err)
		return
	}
	if !created {
		s.log.Info("email login rate limited", "user_id", rec.ID)
		return
	}

	if _, err := s.email.Send(ctx, loginEmail(emailAddr, emailLoginLink(base, token, returnTo))); err != nil {
		s.log.Warn("email login send failed", "user_id", rec.ID, "err", err)
		// A token the user never received must not consume a slot under the
		// 3-active cap: three failed sends would otherwise lock the user out of
		// email login for the whole TTL. The delete runs on a fresh context — a
		// Send that died by exhausting the shared deadline (provider hang) must
		// not take this cleanup down with it.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.store.DeleteLoginToken(dctx, tokenHash); err != nil {
			s.log.Warn("email login token cleanup failed", "user_id", rec.ID, "err", err)
		}
		return
	}
	s.log.Info("email login link sent", "user_id", rec.ID)
}

// handleEmailCallback consumes the one-time token via an SPA-issued POST. The
// emailed link itself is a harmless GET to /login/email (see emailLoginURL), so
// a mail-scanner prefetch cannot burn the token — only the user's click does.
// Failures never use 401: the SPA api client treats 401 as "session expired".
func (s *Server) handleEmailCallback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw := strings.TrimSpace(body.Token)
	if raw == "" {
		apiError(w, http.StatusBadRequest, "token is required")
		return
	}
	userID, err := s.store.ConsumeLoginToken(r.Context(), loginTokenHash(raw))
	if err != nil {
		if errors.Is(err, store.ErrLoginTokenInvalid) {
			apiError(w, http.StatusBadRequest, "sign-in link is invalid or expired")
			return
		}
		s.log.Warn("email login consume failed", "err", err)
		apiError(w, http.StatusInternalServerError, "sign-in failed")
		return
	}

	u, err := s.store.Q.GetUserByID(r.Context(), userID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !u.Active) {
		apiError(w, http.StatusForbidden, "account is disabled")
		return
	}
	if err != nil {
		s.log.Warn("email login user lookup failed", "user_id", userID, "err", err)
		apiError(w, http.StatusInternalServerError, "sign-in failed")
		return
	}

	if err := s.sessions.RenewToken(r.Context()); err != nil {
		s.log.Warn("email login session renew failed", "user_id", u.ID, "err", err)
		apiError(w, http.StatusInternalServerError, "sign-in failed")
		return
	}
	s.sessions.Put(r.Context(), auth.SessionUserIDKey, u.ID)
	s.log.Info("email login", "user_id", u.ID, "role", u.Role)
	w.WriteHeader(http.StatusNoContent)
}

// emailLoginBaseURL resolves the public base URL for sign-in links. It reads
// the *http.Request (Host / forwarded headers) and therefore must be called
// before the handler returns — never from the async send goroutine.
func (s *Server) emailLoginBaseURL(r *http.Request) (string, error) {
	base := s.cfg.AppBaseURL
	if base == "" {
		if s.cfg.Env == "production" && !s.cfg.EmailLoginTrustRequestHost {
			return "", fmt.Errorf("ADAMARKER_APP_BASE_URL is required for email login")
		}
		base = requestBaseURL(r, s.cfg.Env == "production")
	}
	return base, nil
}

// emailLoginLink builds the emailed sign-in link. It lands on an SPA page that
// renders plain HTML on GET; the token is only consumed by the explicit POST
// /auth/email-callback the page issues on click, so mail-scanner link
// prefetching cannot invalidate it.
func emailLoginLink(base, token, returnTo string) string {
	link := strings.TrimRight(base, "/") + "/login/email?token=" + url.QueryEscape(token)
	if safe := auth.SafeReturnPath(returnTo); safe != "/" {
		link += "&return_to=" + url.QueryEscape(safe)
	}
	return link
}

func requestBaseURL(r *http.Request, preferHTTPS bool) string {
	scheme := "http"
	if r.TLS != nil || preferHTTPS {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto == "http" || proto == "https" {
		scheme = proto
	}

	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func loginEmail(to, link string) domain.OutboundEmail {
	escaped := html.EscapeString(link)
	return domain.OutboundEmail{
		To:       to,
		Subject:  "Your ADA-Marker sign-in link",
		TextBody: "Use this link to sign in to ADA-Marker:\n\n" + link + "\n\nThis link expires in 15 minutes. If you did not request it, ignore this email.\n",
		HTMLBody: `<p>Use this link to sign in to ADA-Marker:</p><p><a href="` + escaped + `">Sign in to ADA-Marker</a></p><p>This link expires in 15 minutes. If you did not request it, ignore this email.</p>`,
	}
}

func newLoginToken() (raw string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, loginTokenHash(raw), nil
}

func loginTokenHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

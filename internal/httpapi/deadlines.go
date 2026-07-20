package httpapi

import (
	"net/http"
	"time"
)

// Per-request body deadlines (F5). The server no longer sets a global
// http.Server.ReadTimeout / WriteTimeout: in HTTP/1.x those are anchored at the
// moment the request header is read and span the ENTIRE body read plus response
// write, so a 30 s ReadTimeout (and even the 60 s WriteTimeout, which is likewise
// reset per request-header and covers body-read time) kills a large but perfectly
// healthy zip/submissions upload mid-body. Instead every request gets a per-request
// deadline set by deadlineMiddleware, and the upload handlers extend it to a
// generous window before reading the body. Slow-loris is still guarded by the
// server's ReadHeaderTimeout (headers must arrive fast) and IdleTimeout (idle
// keep-alives are reaped) — those stay on the http.Server.
const (
	// defaultReadDeadline bounds how long a normal request's body may take to read.
	defaultReadDeadline = 30 * time.Second
	// defaultWriteDeadline bounds how long the response write may take.
	defaultWriteDeadline = 60 * time.Second
	// uploadBodyDeadline is the generous window the upload handlers extend to before
	// parsing a potentially 1 GiB multipart body over a slow link.
	uploadBodyDeadline = 20 * time.Minute
)

// deadlineMiddleware sets a per-request read+write deadline via
// http.NewResponseController, replacing the removed global Server timeouts (F5).
// It runs outermost so the deadline is in force before any handler reads the body.
// A handler that needs longer (uploads) calls extendBodyDeadline to push both out.
// SetReadDeadline/SetWriteDeadline are best-effort: on a connection type that does
// not support them (rare — e.g. a test recorder), the controller returns
// ErrNotSupported, which we ignore, leaving the request untimed rather than failing
// it.
func deadlineMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		now := time.Now()
		_ = rc.SetReadDeadline(now.Add(defaultReadDeadline))
		_ = rc.SetWriteDeadline(now.Add(defaultWriteDeadline))
		next.ServeHTTP(w, r)
	})
}

// extendBodyDeadline pushes both the read and write deadlines out to now+d, so an
// upload handler can accept a large body (and write its response) without the
// default 30 s/60 s deadlines killing it mid-transfer (F5). Call it BEFORE
// ParseMultipartForm. Returns the controller's error only for the caller's
// awareness; handlers currently treat it as best-effort (an unsupported connection
// just means no deadline, same as the default path).
func extendBodyDeadline(w http.ResponseWriter, d time.Duration) error {
	rc := http.NewResponseController(w)
	now := time.Now()
	if err := rc.SetReadDeadline(now.Add(d)); err != nil {
		return err
	}
	return rc.SetWriteDeadline(now.Add(d))
}

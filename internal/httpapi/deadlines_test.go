package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDeadlineMiddleware_DoesNotBreakNormalHandler asserts the middleware wraps a
// handler transparently: the handler still runs, sees the request, and its response
// reaches the client. On an httptest.ResponseRecorder the ResponseController's
// Set*Deadline calls return ErrNotSupported (the recorder has no deadline methods),
// which the middleware swallows — so an unsupported connection means "no deadline",
// never a failed request.
func TestDeadlineMiddleware_DoesNotBreakNormalHandler(t *testing.T) {
	called := false
	h := deadlineMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if !called {
		t.Fatal("wrapped handler was not called")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status: got %d want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body: got %q want %q", rec.Body.String(), "ok")
	}
}

// TestExtendBodyDeadline_UnsupportedConnection covers the helper's error path: on a
// ResponseWriter without SetReadDeadline (the recorder), the controller reports
// ErrNotSupported. The helper surfaces it to the caller (who treats it as
// best-effort), and does not panic.
func TestExtendBodyDeadline_UnsupportedConnection(t *testing.T) {
	rec := httptest.NewRecorder()
	err := extendBodyDeadline(rec, uploadBodyDeadline)
	if !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("extendBodyDeadline on recorder: got %v want ErrNotSupported", err)
	}
}

// slowBody is an io.Reader that yields its payload one byte at a time, sleeping
// between bytes, so the whole request body takes longer than the server's read
// deadline. It is used to prove the per-request deadline actually times out a slow
// upload — and that extending it lets the same slow body through.
type slowBody struct {
	data  []byte
	i     int
	delay time.Duration
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.i >= len(s.data) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	p[0] = s.data[s.i]
	s.i++
	return 1, nil
}

// TestPerRequestDeadline_SlowBody drives a REAL server (deadlines only work over a
// real net.Conn, not the recorder) with a tiny default read deadline, then proves:
//   - a slow body beyond the default deadline fails, and
//   - a handler that extends the deadline lets the same slow body through.
//
// Kept fast (< 2 s total): the default deadline is 50 ms and the slow body drips ~10
// bytes at 15 ms each (~150 ms), comfortably past 50 ms but well under the extended
// window.
func TestPerRequestDeadline_SlowBody(t *testing.T) {
	const (
		shortDeadline = 50 * time.Millisecond
		perByteDelay  = 15 * time.Millisecond
		extended      = 5 * time.Second
	)
	payload := bytes.Repeat([]byte("x"), 10) // ~150 ms to fully read

	// A middleware mirroring deadlineMiddleware but with the tiny test deadline.
	shortDeadlineMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			now := time.Now()
			_ = rc.SetReadDeadline(now.Add(shortDeadline))
			_ = rc.SetWriteDeadline(now.Add(shortDeadline))
			next.ServeHTTP(w, r)
		})
	}

	readAll := func(w http.ResponseWriter, r *http.Request) error {
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			// The server-side read deadline surfaces as a timeout on the body read.
			w.WriteHeader(http.StatusRequestTimeout)
			return err
		}
		w.WriteHeader(http.StatusOK)
		return nil
	}

	mux := http.NewServeMux()
	// /strict: default (short) deadline only — a slow body must time out.
	mux.HandleFunc("POST /strict", func(w http.ResponseWriter, r *http.Request) {
		_ = readAll(w, r)
	})
	// /extend: the handler pushes the deadline out before reading — slow body OK.
	mux.HandleFunc("POST /extend", func(w http.ResponseWriter, r *http.Request) {
		if err := extendBodyDeadline(w, extended); err != nil {
			t.Errorf("extendBodyDeadline on real conn: %v", err)
		}
		_ = readAll(w, r)
	})

	srv := httptest.NewServer(shortDeadlineMW(mux))
	defer srv.Close()

	post := func(path string) (int, error) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, &slowBody{data: payload, delay: perByteDelay})
		req.ContentLength = int64(len(payload))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	// Strict route: the slow body must NOT be read to completion within 50 ms — the
	// request fails (either a transport error from the server dropping the conn on
	// deadline, or a 408 the handler wrote after the read error).
	code, err := post("/strict")
	if err == nil && code == http.StatusOK {
		t.Fatalf("strict route accepted a slow body past the deadline (code %d) — deadline not enforced", code)
	}

	// Extended route: the same slow body succeeds.
	code, err = post("/extend")
	if err != nil {
		t.Fatalf("extend route failed to accept a slow body within the extended window: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("extend route: got %d want 200 (extended deadline should let the slow body through)", code)
	}
}

// Package web serves the React SPA embedded into the binary via go:embed (spec §2).
//
// Build wiring per docs/DECISIONS.md D9: Vite builds frontend/ into
// internal/web/assets/dist/ (gitignored, produced by `make frontend`). When that
// directory is present at compile time it is embedded and served; otherwise the
// committed placeholder assets/index.html serves, so `go build` works without Node.
// In both cases unknown paths fall back to index.html for client-side routing.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var assetsFS embed.FS

// Handler returns an http.Handler that serves embedded static assets, falling back to
// index.html for unknown paths so client-side routing works.
//
// Caching: Vite's hashed files under dist/assets/ are immutable (a content change
// changes the filename), so they get a long-lived immutable Cache-Control; index.html
// is always served with no-cache so a new deploy is picked up immediately.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}

	// Prefer the Vite build output when it was embedded (D9); otherwise fall back
	// to the committed placeholder directly under assets/.
	dist := false
	if distFS, err := fs.Sub(sub, "dist"); err == nil {
		if _, err := fs.Stat(distFS, "index.html"); err == nil {
			sub, dist = distFS, true
		}
	}

	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if clean == "" || clean == "index.html" {
			serveIndex(w, index)
			return
		}
		if _, err := fs.Stat(sub, clean); err != nil {
			serveIndex(w, index) // unknown path → SPA client route
			return
		}
		if dist && strings.HasPrefix(clean, "assets/") {
			// Vite content-hashed filenames: safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	}), nil
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}

// Package webui serves the embedded, pre-built web UI (web/ builds into
// internal/webui/dist).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the UI. Hashed assets under /assets/ are cached for a year;
// index.html is never cached. If the UI was not built, a short notice is served.
func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("PiCache web UI is not built. Run `npm ci && npm run build` in web/ and rebuild the binary.\nThe API is available under /api/v1.\n"))
		})
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		switch {
		case strings.HasPrefix(p, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		if p != "/" {
			if _, err := fs.Stat(sub, strings.TrimPrefix(p, "/")); err != nil {
				// Hash router: unknown paths serve the app shell.
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}

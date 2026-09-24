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
	return handler(sub)
}

// handler serves the UI in fsys (the contents of dist).
//
// The UI is built with relative asset URLs (vite base './') so it also works
// behind a reverse proxy that mounts it under a sub-path. Its URLs therefore
// only resolve from the UI root: index.html is served in place for unknown
// paths one level deep ("/admin"); deeper unknown paths ("/admin/",
// "/dns/queries") are redirected to the root with a relative Location, since
// the page's ./assets/… would otherwise resolve to unknown paths as well and
// the browser would refuse the HTML answers as scripts and styles (blank
// page). Unknown files below /assets/ are 404: an HTML answer there would be
// cached as an immutable asset.
func handler(fsys fs.FS) http.Handler {
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("PiCache web UI is not built. Run `npm ci && npm run build` in web/ and rebuild the binary.\nThe API is available under /api/v1.\n"))
		})
	}
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		if p == "/" || isFile(fsys, p) {
			if strings.HasPrefix(p, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		// Hash router: unknown paths show the app shell, served from the root.
		if depth := strings.Count(r.URL.Path, "/") - 1; depth > 0 {
			// Relative, so a reverse proxy's path prefix is kept (http.Redirect
			// would turn it into an absolute path). The browser keeps #fragments.
			w.Header().Set("Location", strings.Repeat("../", depth))
			w.WriteHeader(http.StatusFound)
			return
		}
		r = r.Clone(r.Context())
		r.URL.Path = "/"
		r.URL.RawPath = ""
		files.ServeHTTP(w, r)
	})
}

// isFile reports whether p (clean, absolute) names a regular file in fsys.
// Directories count as unknown: they would be served as listings.
func isFile(fsys fs.FS, p string) bool {
	fi, err := fs.Stat(fsys, strings.TrimPrefix(p, "/"))
	return err == nil && fi.Mode().IsRegular()
}

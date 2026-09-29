package api

import (
	_ "embed"
	"net/http"
)

// openAPI is the OpenAPI 3.1 description of this API (docs/API.md stays
// the reference; the file is maintained by hand next to it). Its test
// checks every route of the registry with its permission, lock class and
// destructive flag, and the settings schema against settings.All.
//
//go:embed openapi.json
var openAPI []byte

// registerOpenAPIRoutes registers GET /openapi.json (v0.16.0).
func (s *Server) registerOpenAPIRoutes() {
	s.route("GET /api/v1/openapi.json", permRead, s.openAPIDocument)
}

// openAPIDocument serves the embedded description: revalidated by the
// browser on every use (no-cache), never stored with a cookie.
func (s *Server) openAPIDocument(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_, err := w.Write(openAPI)
	if err != nil {
		return errAlreadyWritten{err}
	}
	return nil
}

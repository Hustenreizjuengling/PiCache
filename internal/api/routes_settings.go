package api

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// registerSettingsRoutes registers the settings endpoints (docs/API.md).
func (s *Server) registerSettingsRoutes() {
	s.route("GET /api/v1/settings", permRead, s.settingsGet)
	s.route("PUT /api/v1/settings", permAdmin, s.settingsPut)
	s.route("PATCH /api/v1/settings/{section}", permAdmin, s.settingsPatch)
	s.route("GET /api/v1/settings/defaults", permRead, s.settingsDefaults)
}

// errActiveStoreID rejects store switches through the settings endpoints:
// switching needs the checks of POST /storage/targets/{id}/activate.
var errActiveStoreID = apperr.Invalid("cache.activeStoreId", "use the storage page to switch stores")

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Settings.Get())
}

func (s *Server) settingsDefaults(w http.ResponseWriter, r *http.Request) error {
	return ok(w, settings.Defaults())
}

// settingsPut replaces the whole document. The body is decoded on top of the
// current settings, so members a client does not know yet keep their values.
func (s *Server) settingsPut(w http.ResponseWriter, r *http.Request) error {
	in := s.d.Settings.Get().Clone()
	if err := decode(w, r, in); err != nil {
		return err
	}
	return s.updateSettings(w, r, func(a *settings.All) error {
		if in.Cache.ActiveStoreID != a.Cache.ActiveStoreID {
			return errActiveStoreID
		}
		*a = *in
		return nil
	})
}

// settingsPatch replaces one section. The body is decoded on top of the
// current section (omitted members keep their values; arrays are replaced).
func (s *Server) settingsPatch(w http.ResponseWriter, r *http.Request) error {
	cur := s.d.Settings.Get().Clone()
	var (
		dst   any
		apply func(a *settings.All) error
	)
	switch r.PathValue("section") {
	case "dns":
		dst, apply = &cur.DNS, func(a *settings.All) error { a.DNS = cur.DNS; return nil }
	case "filter":
		dst, apply = &cur.Filter, func(a *settings.All) error { a.Filter = cur.Filter; return nil }
	case "lancache":
		dst, apply = &cur.LanCache, func(a *settings.All) error { a.LanCache = cur.LanCache; return nil }
	case "cache":
		dst, apply = &cur.Cache, func(a *settings.All) error {
			if cur.Cache.ActiveStoreID != a.Cache.ActiveStoreID {
				return errActiveStoreID
			}
			a.Cache = cur.Cache
			return nil
		}
	case "logs":
		dst, apply = &cur.Logs, func(a *settings.All) error { a.Logs = cur.Logs; return nil }
	case "web":
		dst, apply = &cur.Web, func(a *settings.All) error { a.Web = cur.Web; return nil }
	case "updates":
		dst, apply = &cur.Updates, func(a *settings.All) error { a.Updates = cur.Updates; return nil }
	default:
		return apperr.Invalid("section", "unknown settings section (dns, filter, lancache, cache, logs, web, updates)")
	}
	if err := decode(w, r, dst); err != nil {
		return err
	}
	return s.updateSettings(w, r, apply)
}

// updateSettings validates, persists and audits a change (with the changed
// member paths) and responds with the new document.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request, fn func(*settings.All) error) error {
	old := s.d.Settings.Get()
	next, err := s.d.Settings.Update(r.Context(), fn)
	if err != nil {
		return err
	}
	if changed := changedSettings(old, next); len(changed) > 0 {
		s.audit(r, "settings.update", "", map[string][]string{"changed": changed})
	}
	return ok(w, next)
}

// changedSettings returns the paths ("dns.upstreams", …) of the members that
// differ between two documents, sorted.
func changedSettings(old, next *settings.All) []string {
	a, b := flattenSettings(old), flattenSettings(next)
	var out []string
	for k, v := range b {
		if !bytes.Equal(a[k], v) {
			out = append(out, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func flattenSettings(a *settings.All) map[string]jsontext.Value {
	out := map[string]jsontext.Value{}
	raw, err := json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return out
	}
	var sections map[string]map[string]jsontext.Value
	if err := json.Unmarshal(raw, &sections); err != nil {
		return out
	}
	for sec, members := range sections {
		for name, v := range members {
			out[sec+"."+name] = v
		}
	}
	return out
}

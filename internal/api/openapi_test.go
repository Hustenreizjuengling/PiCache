package api

import (
	"encoding"
	"encoding/json/v2"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// The OpenAPI description (openapi.json, maintained by hand next to
// docs/API.md) against the route registry and settings.All. The other
// response schemas are not checked against the code: docs/API.md stays the
// reference.

func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	return doc
}

// openAPIPath maps a registry pattern to its OpenAPI operation: {$}
// removed, {x...} as {x}, a subtree pattern ending in / as <pattern>{path}.
func openAPIPath(pattern string) (method, path string) {
	method, path, _ = strings.Cut(pattern, " ")
	switch {
	case strings.HasSuffix(path, "{$}"):
		path = strings.TrimSuffix(path, "{$}")
	case strings.HasSuffix(path, "/"):
		path += "{path}"
	}
	return method, regexp.MustCompile(`\{(\w+)\.\.\.\}`).ReplaceAllString(path, "{$1}")
}

func TestOpenAPIPathMapping(t *testing.T) {
	for pattern, want := range map[string]string{
		"GET /debug/pprof/{$}":             "GET /debug/pprof/",
		"GET /debug/pprof/heap":            "GET /debug/pprof/heap",
		"GET /api/v1/files/":               "GET /api/v1/files/{path}",
		"GET /api/v1/files/{name...}":      "GET /api/v1/files/{name}",
		"DELETE /api/v1/clients/{id}":      "DELETE /api/v1/clients/{id}",
		"PATCH /api/v1/settings/{section}": "PATCH /api/v1/settings/{section}",
	} {
		if m, p := openAPIPath(pattern); m+" "+p != want {
			t.Errorf("%s → %s %s, want %s", pattern, m, p, want)
		}
	}
}

var permLetters = map[perm]string{permPublic: "P", permRead: "R", permAdmin: "A", permSession: "S", permSelf: "U", permExport: "X"}

var lockNames = map[lockClass]string{lockNone: "none", lockLocked: "locked", lockExempt: "exempt", lockPause: "pause"}

// Every registered route is an operation with its permission, lock class,
// destructive flag and security, and every operation outside the registry
// is marked x-picache-extra (exactly /healthz, /metrics and /dns-query).
func TestOpenAPIMatchesRegistry(t *testing.T) {
	e := newCoreEnv(t)
	doc := loadOpenAPI(t)
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v", doc["openapi"])
	}
	want := map[string]routeInfo{}
	for _, r := range e.srv.routes {
		m, p := openAPIPath(r.Pattern)
		want[m+" "+p] = r
	}
	ops := map[string]map[string]any{}
	var extras []string
	for p, item := range doc["paths"].(map[string]any) {
		for m, v := range item.(map[string]any) {
			op, ok := v.(map[string]any)
			if !ok || !slices.Contains([]string{"get", "put", "post", "patch", "delete", "head", "options"}, m) {
				continue
			}
			key := strings.ToUpper(m) + " " + p
			if op["x-picache-extra"] == true {
				extras = append(extras, key)
				continue
			}
			ops[key] = op
		}
	}
	slices.Sort(extras)
	if wantExtras := []string{"GET /dns-query", "GET /dns-query/{dnsClientId}", "GET /healthz", "GET /metrics",
		"POST /dns-query", "POST /dns-query/{dnsClientId}"}; !slices.Equal(extras, wantExtras) {
		t.Errorf("extras %v, want %v", extras, wantExtras)
	}
	for k := range ops {
		if _, ok := want[k]; !ok {
			t.Errorf("openapi.json has %s, which is not in the route registry", k)
		}
	}
	security := map[string]string{"P": `[]`, "R": `[{"session":[]},{"bearer":[]}]`, "A": `[{"session":[]},{"bearer":[]}]`,
		"X": `[{"session":[]},{"bearer":[]}]`, "U": `[{"session":[]}]`, "S": `[{"session":[]}]`}
	paramRe := regexp.MustCompile(`\{(\w+)\}`)
	for k, r := range want {
		op, ok := ops[k]
		if !ok {
			t.Errorf("openapi.json lacks %s (%s)", k, r.Pattern)
			continue
		}
		letter := permLetters[r.Perm]
		if op["x-picache-permission"] != letter || op["x-picache-lock"] != lockNames[r.Lock] || op["x-picache-destructive"] != r.Destructive {
			t.Errorf("%s: permission %v lock %v destructive %v, want %s %s %v", k, op["x-picache-permission"], op["x-picache-lock"],
				op["x-picache-destructive"], letter, lockNames[r.Lock], r.Destructive)
		}
		if b, _ := json.Marshal(op["security"]); string(b) != security[letter] {
			t.Errorf("%s: security %s, want %s", k, b, security[letter])
		}
		if s, _ := op["summary"].(string); s == "" {
			t.Errorf("%s: no summary", k)
		}
		if resp, _ := op["responses"].(map[string]any); len(resp) == 0 {
			t.Errorf("%s: no responses", k)
		}
		var declared []string
		params, _ := op["parameters"].([]any)
		for _, p := range params {
			if pm, _ := p.(map[string]any); pm["in"] == "path" {
				declared = append(declared, pm["name"].(string))
			}
		}
		_, path, _ := strings.Cut(k, " ")
		for _, m := range paramRe.FindAllStringSubmatch(path, -1) {
			if !slices.Contains(declared, m[1]) {
				t.Errorf("%s: path parameter %s is not declared", k, m[1])
			}
		}
	}
}

// Every $ref points at an existing component, and the error response is
// {error:{code, message, field?}}.
func TestOpenAPIReferences(t *testing.T) {
	doc := loadOpenAPI(t)
	comps := doc["components"].(map[string]any)
	var walk func(v any)
	n := 0
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				n++
				kind, name, ok := strings.Cut(strings.TrimPrefix(ref, "#/components/"), "/")
				if !strings.HasPrefix(ref, "#/components/") || !ok {
					t.Errorf("reference %s", ref)
				} else if c, _ := comps[kind].(map[string]any); c[name] == nil {
					t.Errorf("unresolved reference %s", ref)
				}
			}
			for _, x := range v {
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	if n < 100 {
		t.Fatalf("only %d references", n)
	}
	schemes := comps["securitySchemes"].(map[string]any)
	if schemes["session"] == nil || schemes["bearer"] == nil {
		t.Fatalf("security schemes %v", schemes)
	}
	body := comps["schemas"].(map[string]any)["ErrorBody"].(map[string]any)
	inner := body["properties"].(map[string]any)["error"].(map[string]any)
	var names []string
	for k := range inner["properties"].(map[string]any) {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"code", "field", "message"}) {
		t.Fatalf("error members %v", names)
	}
	if b, _ := json.Marshal(comps["responses"].(map[string]any)["Error"]); !strings.Contains(string(b), `"#/components/schemas/ErrorBody"`) {
		t.Fatalf("components.responses.Error %s", b)
	}
}

// components.schemas.Settings has exactly the JSON members of settings.All,
// recursively.
func TestOpenAPISettingsSchema(t *testing.T) {
	doc := loadOpenAPI(t)
	s, ok := doc["components"].(map[string]any)["schemas"].(map[string]any)["Settings"].(map[string]any)
	if !ok {
		t.Fatal("no components.schemas.Settings")
	}
	compareSchema(t, "Settings", s, reflect.TypeFor[settings.All]())
}

var textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()

func compareSchema(t *testing.T, at string, s map[string]any, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() || typ.Implements(textMarshalerType) || reflect.PointerTo(typ).Implements(textMarshalerType) {
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		props, _ := s["properties"].(map[string]any)
		fields := jsonFields(typ)
		var got, want []string
		for k := range props {
			got = append(got, k)
		}
		for k := range fields {
			want = append(want, k)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: schema members %v, settings.All %v", at, got, want)
			return
		}
		for name, ft := range fields {
			sub, _ := props[name].(map[string]any)
			compareSchema(t, at+"."+name, sub, ft)
		}
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return
		}
		items, _ := s["items"].(map[string]any)
		if items == nil {
			t.Errorf("%s: an array without items", at)
			return
		}
		compareSchema(t, at+"[]", items, typ.Elem())
	case reflect.Map:
		ap, _ := s["additionalProperties"].(map[string]any)
		if ap == nil {
			t.Errorf("%s: a map without additionalProperties", at)
			return
		}
		compareSchema(t, at+"{}", ap, typ.Elem())
	}
}

// jsonFields returns the JSON members of a struct (encoding/json tags:
// "-" skipped, embedded structs without a name inlined).
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if (f.Anonymous && name == "") || strings.Contains(opts, "inline") {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// GET /openapi.json: R (401 without authentication), the embedded file as
// application/json, revalidated (no-cache), with the security headers and
// without a cookie.
func TestOpenAPIRoute(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	readTok := e.createToken(t, session, "read")
	coreWantError(t, e.do("GET", "/api/v1/openapi.json", "", ""), http.StatusUnauthorized, "unauthorized", "")
	for _, cred := range []string{readTok, session} {
		w := e.do("GET", "/api/v1/openapi.json", "", cred)
		h := w.Header()
		if w.Code != http.StatusOK || h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-cache" ||
			h.Get("Set-Cookie") != "" || h.Get("X-Content-Type-Options") != "nosniff" || w.Body.String() != string(openAPI) {
			t.Fatalf("GET /openapi.json: %d %v (%d bytes)", w.Code, h, w.Body.Len())
		}
	}
}

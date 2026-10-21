// Package epictest is a fake Epic for the tests of the prefill service:
// the account service (client credentials, device code, exchange codes,
// refresh, sessions), the launcher's library and manifest API, the catalog,
// the download tickets and signed URLs, and a CDN that serves the manifests
// and chunks it describes. It is imported by tests only.
package epictest

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

// Chunk describes a generated chunk.
type Chunk struct {
	GUID       [4]uint32
	Hash       uint64
	Group      uint8
	Size       int
	SecretGUID [4]uint32
}

// Data is the chunk's generated body.
func (c Chunk) Data() []byte {
	b := make([]byte, c.Size)
	for i := range b {
		b[i] = byte(int(c.GUID[0]) + i%253)
	}
	return b
}

// App is a fake game of the account.
type App struct {
	AppName, Namespace, CatalogItemID, Title, BuildVersion string
	FeatureLevel                                           uint32
	Chunks                                                 []Chunk
	// DLC makes it a DLC of that catalog item; Category a catalog
	// category path (plugins, …).
	DLC, Category string
	Signed        bool
}

// Server is the fake Epic.
type Server struct {
	HTTP *httptest.Server

	mu        sync.Mutex
	apps      []App
	polls     int
	confirmAt int
	tokens    map[string]string // access token → client
	refresh   map[string]bool
	killed    []string
	requests  []string // CDN requests: Host + path
	hits      map[string]bool
	refreshes int
	seq       int
}

// Account is the fake account.
const (
	AccountID   = "0123456789abcdef0123456789abcdef"
	DisplayName = "EpicGamer"
)

// New starts the fake with apps.
func New(apps []App) *Server {
	s := &Server{apps: apps, tokens: map[string]string{}, refresh: map[string]bool{}, hits: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /account/api/oauth/token", s.token)
	mux.HandleFunc("POST /account/api/oauth/deviceAuthorization", s.deviceAuth)
	mux.HandleFunc("GET /account/api/oauth/exchange", s.exchange)
	mux.HandleFunc("DELETE /account/api/oauth/sessions/kill/{token}", s.kill)
	mux.HandleFunc("GET /launcher/api/public/assets/Windows", s.assets)
	mux.HandleFunc("GET /launcher/api/public/assets/v2/platform/Windows/namespace/{ns}/catalogItem/{item}/app/{app}/label/Live", s.manifestInfo)
	mux.HandleFunc("GET /catalog/api/shared/namespace/{ns}/bulk/items", s.catalog)
	mux.HandleFunc("POST /launcher/api/public/assets/v2/ticket", s.ticket)
	mux.HandleFunc("POST /artifact-delivery/api/public/v1/delivery/account/{account}/downloadurls", s.downloadURLs)
	mux.HandleFunc("GET /lancache-heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-LanCache-Processed-By", "fake-cache")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /Builds/", s.cdn)
	s.HTTP = httptest.NewServer(mux)
	return s
}

func (s *Server) Close()      { s.HTTP.Close() }
func (s *Server) URL() string { return s.HTTP.URL }
func (s *Server) Addr() string {
	return strings.TrimPrefix(s.HTTP.URL, "http://")
}

// ConfirmAfter makes the device code confirmed at the n-th poll.
func (s *Server) ConfirmAfter(n int) {
	s.mu.Lock()
	s.confirmAt, s.polls = n, 0
	s.mu.Unlock()
}

// Revoke makes Epic refuse a refresh token.
func (s *Server) Revoke(refreshToken string) {
	s.mu.Lock()
	delete(s.refresh, refreshToken)
	s.mu.Unlock()
}

// Killed lists the access tokens of ended sessions.
func (s *Server) Killed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.killed)
}

// Requests lists the CDN requests (host + path).
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ResetRequests forgets them.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

// Refreshes counts refresh_token grants.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// SetBuild changes an app's build (a new version).
func (s *Server) SetBuild(appName, build string, chunks []Chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.apps {
		if s.apps[i].AppName == appName {
			s.apps[i].BuildVersion, s.apps[i].Chunks = build, chunks
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, v)
}

func epicError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"errorCode": code, "errorMessage": code, "numericErrorCode": 1})
}

func clientOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), "basic ") {
		return ""
	}
	b, _ := base64.StdEncoding.DecodeString(h[6:])
	id, _, _ := strings.Cut(string(b), ":")
	return id
}

func (s *Server) bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[h[7:]]
}

func (s *Server) issue(client string, user bool) map[string]any {
	s.seq++
	n := s.seq
	at := fmt.Sprintf("at-%s-%d", client[:4], n)
	s.tokens[at] = client
	out := map[string]any{"access_token": at, "expires_in": 28800, "expires_at": time.Now().Add(8 * time.Hour).UTC().Format(time.RFC3339),
		"token_type": "bearer", "client_id": client}
	if user {
		rt := fmt.Sprintf("rt-%s-%d", client[:4], n)
		s.refresh[rt] = true
		out["refresh_token"] = rt
		out["refresh_expires"] = 1987200
		out["refresh_expires_at"] = time.Now().Add(23 * 24 * time.Hour).UTC().Format(time.RFC3339)
		out["account_id"] = AccountID
		out["displayName"] = DisplayName
	}
	return out
}

const (
	switchClient   = "98f7e42c2e3a4f86a74eb43fbb41ed39"
	launcherClient = "34a02cf8f4414e29b15921876da36f9a"
)

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	client := clientOf(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "client_credentials":
		writeJSON(w, 200, s.issue(client, false))
	case "device_code":
		if client != switchClient || r.Form.Get("device_code") != "dc-1" {
			epicError(w, 400, "errors.com.epicgames.not_found")
			return
		}
		s.polls++
		if s.confirmAt == 0 || s.polls < s.confirmAt {
			epicError(w, 400, "errors.com.epicgames.account.oauth.authorization_pending")
			return
		}
		writeJSON(w, 200, s.issue(client, true))
	case "exchange_code":
		if client != launcherClient || !strings.HasPrefix(r.Form.Get("exchange_code"), "ex-") || r.Form.Get("token_type") != "eg1" {
			epicError(w, 400, "errors.com.epicgames.account.oauth.exchange_code_not_found")
			return
		}
		writeJSON(w, 200, s.issue(client, true))
	case "refresh_token":
		rt := r.Form.Get("refresh_token")
		if client != launcherClient || !s.refresh[rt] {
			epicError(w, 400, "errors.com.epicgames.account.auth_token.invalid_refresh_token")
			return
		}
		delete(s.refresh, rt) // rotates
		s.refreshes++
		writeJSON(w, 200, s.issue(client, true))
	default:
		epicError(w, 400, "errors.com.epicgames.common.oauth.unsupported_grant_type")
	}
}

func (s *Server) deviceAuth(w http.ResponseWriter, r *http.Request) {
	if s.bearer(r) != switchClient {
		epicError(w, 401, "errors.com.epicgames.common.authentication.token_verification_failed")
		return
	}
	s.mu.Lock()
	s.polls = 0
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"user_code": "ABCD1234", "device_code": "dc-1", "verification_uri": "https://www.epicgames.com/activate",
		"verification_uri_complete": "https://www.epicgames.com/activate?userCode=ABCD1234", "expires_in": 600, "interval": 10})
}

func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	if s.bearer(r) != switchClient {
		epicError(w, 401, "errors.com.epicgames.common.authentication.token_verification_failed")
		return
	}
	writeJSON(w, 200, map[string]any{"code": "ex-1", "expiresInSeconds": 300})
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.killed = append(s.killed, r.PathValue("token"))
	delete(s.tokens, r.PathValue("token"))
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) launcher(w http.ResponseWriter, r *http.Request) bool {
	if s.bearer(r) != launcherClient {
		epicError(w, 403, "errors.com.epicgames.common.missing_permission")
		return false
	}
	return true
}

func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	if !s.launcher(w, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, a := range s.apps {
		out = append(out, map[string]any{"appName": a.AppName, "namespace": a.Namespace, "catalogItemId": a.CatalogItemID,
			"buildVersion": a.BuildVersion, "labelName": "Live-Windows"})
	}
	out = append(out, map[string]any{"appName": "UEAsset", "namespace": "ue", "catalogItemId": "x", "buildVersion": "1", "labelName": "Live-Windows"})
	writeJSON(w, 200, out)
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	if !s.launcher(w, r) {
		return
	}
	ids := strings.Split(r.URL.Query().Get("id"), ",")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{}
	for _, a := range s.apps {
		if a.Namespace != r.PathValue("ns") || !slices.Contains(ids, a.CatalogItemID) {
			continue
		}
		item := map[string]any{"id": a.CatalogItemID, "title": a.Title, "namespace": a.Namespace, "categories": []any{map[string]any{"path": "games"}}}
		if a.Category != "" {
			item["categories"] = []any{map[string]any{"path": a.Category}}
		}
		if a.DLC != "" {
			item["mainGameItem"] = map[string]any{"id": a.DLC}
		}
		out[a.CatalogItemID] = item
	}
	writeJSON(w, 200, out)
}

func (s *Server) app(name string) (App, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.AppName == name {
			return a, true
		}
	}
	return App{}, false
}

// BasePath is the CDN path of an app's build.
func BasePath(a App) string { return "/Builds/Org/o-test/" + a.CatalogItemID + "/default" }

func (s *Server) manifestInfo(w http.ResponseWriter, r *http.Request) {
	if !s.launcher(w, r) {
		return
	}
	a, ok := s.app(r.PathValue("app"))
	if !ok || a.Namespace != r.PathValue("ns") || a.CatalogItemID != r.PathValue("item") {
		epicError(w, 404, "errors.com.epicgames.launcher.not_found")
		return
	}
	m := Manifest(a)
	sum := sha1.Sum(m)
	host := s.Addr()
	writeJSON(w, 200, map[string]any{"elements": []any{map[string]any{
		"appName": a.AppName, "labelName": "Live-Windows", "buildVersion": a.BuildVersion, "hash": hex.EncodeToString(sum[:]),
		"useSignedUrl": a.Signed,
		"manifests": []any{
			map[string]any{"uri": "https://egs-cloudfront-chunks.epicgamescdn.com" + BasePath(a) + "/" + a.AppName + ".manifest",
				"queryParams": []any{map[string]any{"name": "cf_token", "value": "1_abc"}}},
			map[string]any{"uri": "https://" + host + BasePath(a) + "/" + a.AppName + ".manifest"},
		},
	}}})
}

func (s *Server) ticket(w http.ResponseWriter, r *http.Request) {
	if !s.launcher(w, r) {
		return
	}
	var in struct {
		AppName string `json:"appName"`
	}
	if err := json.UnmarshalRead(r.Body, &in); err != nil {
		epicError(w, 400, "errors.com.epicgames.bad_request")
		return
	}
	if _, ok := s.app(in.AppName); !ok {
		epicError(w, 404, "errors.com.epicgames.launcher.not_found")
		return
	}
	writeJSON(w, 200, map[string]any{"signedTicket": "ticket-" + in.AppName, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
}

func (s *Server) downloadURLs(w http.ResponseWriter, r *http.Request) {
	if !s.launcher(w, r) || r.PathValue("account") != AccountID {
		return
	}
	var in struct {
		SignedTicket string   `json:"signedTicket"`
		ChunkIDs     []string `json:"chunkIds"`
	}
	if err := json.UnmarshalRead(r.Body, &in); err != nil || len(in.ChunkIDs) > 50 {
		epicError(w, 400, "errors.com.epicgames.bad_request")
		return
	}
	a, ok := s.app(strings.TrimPrefix(in.SignedTicket, "ticket-"))
	if !ok || !strings.HasPrefix(in.SignedTicket, "ticket-") {
		epicError(w, 400, "errors.com.epicgames.bad_request")
		return
	}
	out := map[string]string{}
	for _, p := range in.ChunkIDs {
		out[p] = "https://egdownload.fastly-edge.com" + BasePath(a) + "/" + p + "?sig=1"
	}
	writeJSON(w, 200, out)
}

// cdn serves the manifests and chunks (any Host), records the requests and
// answers MISS, then HIT, like a cache. Signed builds want ?sig.
func (s *Server) cdn(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Host+r.URL.Path)
	hit := s.hits[r.URL.Path]
	s.hits[r.URL.Path] = true
	apps := slices.Clone(s.apps)
	s.mu.Unlock()
	w.Header().Set("X-LanCache-Processed-By", "fake-cache")
	status := "MISS"
	if hit {
		status = "HIT"
	}
	w.Header().Set("X-Upstream-Cache-Status", status)
	for _, a := range apps {
		base := BasePath(a)
		if !strings.HasPrefix(r.URL.Path, base+"/") {
			continue
		}
		rest := strings.TrimPrefix(r.URL.Path, base+"/")
		if rest == a.AppName+".manifest" {
			if r.URL.Query().Get("cf_token") == "" && !strings.Contains(r.Host, "127.0.0.1") {
				http.Error(w, "token", http.StatusForbidden)
				return
			}
			w.Write(Manifest(a))
			return
		}
		for _, c := range a.Chunks {
			if rest == ChunkPath(c, a.FeatureLevel) {
				if a.Signed && r.URL.Query().Get("sig") == "" {
					http.Error(w, "signed", http.StatusForbidden)
					return
				}
				w.Write(c.Data())
				return
			}
		}
	}
	http.NotFound(w, r)
}

// ChunkPath is the path of a chunk (the same rules as the prefill).
func ChunkPath(c Chunk, fl uint32) string {
	gb := func(g [4]uint32) []byte {
		b := make([]byte, 16)
		for i, v := range g {
			binary.LittleEndian.PutUint32(b[i*4:], v)
		}
		return b
	}
	if fl >= 22 {
		secret := "plain"
		if c.SecretGUID != ([4]uint32{}) {
			secret = base64.RawURLEncoding.EncodeToString(gb(c.SecretGUID))
		}
		h := binary.LittleEndian.AppendUint64(nil, c.Hash)
		return fmt.Sprintf("ChunksV5/%s/%02d/%s_%s.chunk", secret, c.Group, base64.RawURLEncoding.EncodeToString(h), base64.RawURLEncoding.EncodeToString(gb(c.GUID)))
	}
	return fmt.Sprintf("ChunksV4/%02d/%016X_%08X%08X%08X%08X.chunk", c.Group, c.Hash, c.GUID[0], c.GUID[1], c.GUID[2], c.GUID[3])
}

// Manifest builds the binary manifest of an app: header (41 bytes, 73 from
// version 22), zlib body with meta, chunk data list (every chunk once,
// the first twice to test the de-duplication) and an empty file list.
func Manifest(a App) []byte {
	le := binary.LittleEndian
	var body []byte
	// meta
	var meta []byte
	meta = append(meta, 0)                       // data version
	meta = le.AppendUint32(meta, a.FeatureLevel) // feature level
	meta = append(meta, 1)                       // is file data
	meta = le.AppendUint32(meta, 0)              // app id
	for _, s := range []string{a.AppName, a.BuildVersion, "Game.exe", ""} {
		meta = appendFString(meta, s)
	}
	meta = le.AppendUint32(meta, 0) // prereq ids
	for range 3 {
		meta = appendFString(meta, "")
	}
	body = le.AppendUint32(body, uint32(4+len(meta)))
	body = append(body, meta...)
	// chunk data list
	chunks := slices.Clone(a.Chunks)
	if len(chunks) > 0 {
		chunks = append(chunks, chunks[0])
	}
	var cdl []byte
	cdl = append(cdl, 0)
	cdl = le.AppendUint32(cdl, uint32(len(chunks)))
	for _, c := range chunks {
		for _, v := range c.GUID {
			cdl = le.AppendUint32(cdl, v)
		}
	}
	for _, c := range chunks {
		cdl = le.AppendUint64(cdl, c.Hash)
	}
	for range chunks {
		cdl = append(cdl, make([]byte, 20)...)
	}
	for _, c := range chunks {
		cdl = append(cdl, c.Group)
	}
	for range chunks {
		cdl = le.AppendUint32(cdl, 1<<20)
	}
	for _, c := range chunks {
		cdl = le.AppendUint64(cdl, uint64(c.Size))
	}
	if a.FeatureLevel >= 22 {
		for _, c := range chunks {
			for _, v := range c.SecretGUID {
				cdl = le.AppendUint32(cdl, v)
			}
		}
		for range chunks {
			cdl = le.AppendUint32(cdl, 1000)
		}
		for range chunks {
			cdl = append(cdl, make([]byte, 16)...)
		}
	}
	body = le.AppendUint32(body, uint32(4+len(cdl)))
	body = append(body, cdl...)
	// empty file list
	body = le.AppendUint32(body, 9)
	body = append(body, 0)
	body = le.AppendUint32(body, 0)

	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(body)
	zw.Close()
	hs := uint32(41)
	if a.FeatureLevel >= 22 {
		hs = 73
	}
	sum := sha1.Sum(body)
	var h []byte
	h = le.AppendUint32(h, 0x44BEC00C)
	h = le.AppendUint32(h, hs)
	h = le.AppendUint32(h, uint32(len(body)))
	h = le.AppendUint32(h, uint32(zb.Len()))
	h = append(h, sum[:]...)
	h = append(h, 1) // zlib
	h = le.AppendUint32(h, a.FeatureLevel)
	h = append(h, make([]byte, int(hs)-len(h))...)
	return append(h, zb.Bytes()...)
}

func appendFString(b []byte, s string) []byte {
	if s == "" {
		return binary.LittleEndian.AppendUint32(b, 0)
	}
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)+1))
	b = append(b, s...)
	return append(b, 0)
}

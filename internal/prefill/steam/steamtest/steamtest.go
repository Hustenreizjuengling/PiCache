// Package steamtest is a fake Steam for the tests of the prefill service:
// the CM directory, a CM over WebSocket (QR sign-in, logon, licenses, PICS,
// content server list, manifest request codes, token revocation) and a
// content server that serves the manifests and chunks it describes. It is
// imported by tests only.
package steamtest

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Depot is a depot of a fake app. Its chunks are generated from ID.
type Depot struct {
	ID       uint32
	Manifest uint64
	OS       string // config/oslist
	Language string // config/language
	// FromApp makes it a shared depot of another app (depotfromapp; the
	// manifest is in that app's info).
	FromApp uint32
	// Chunks and ChunkSize describe the generated content.
	Chunks    int
	ChunkSize int
}

// App is a fake app.
type App struct {
	Name   string
	Type   string // Game, DLC, Tool, …
	Depots []Depot
	DLC    []uint32
}

// Account is the account the QR sign-in signs in.
type Account struct {
	Name    string
	SteamID uint64
	// Packages are the account's licenses: package ID → app IDs; their
	// depots are the depots of those apps.
	Packages map[uint32][]uint32
}

// Server is the fake Steam.
type Server struct {
	HTTP *httptest.Server

	mu       sync.Mutex
	account  Account
	apps     map[uint32]App
	token    string
	polls    int
	confirm  int // polls until the sign-in is confirmed (0: never)
	codes    map[string]uint64
	revoked  []string
	logons   []Logon
	requests []Request
	beats    int
	hits     map[string]bool
	forbid   map[string]bool // host|depot → 403
	reject   bool            // refuse every logon with the token
}

// Logon records a ClientLogon.
type Logon struct {
	SteamID     uint64
	AccountName string
	Token       string
	LoginID     uint32
	MachineName string
}

// Request records a content request.
type Request struct {
	Host, Path, UserAgent string
}

// New starts a fake Steam with the account and apps.
func New(acct Account, apps map[uint32]App) *Server {
	s := &Server{account: acct, apps: apps, codes: map[string]uint64{}, hits: map[string]bool{}, forbid: map[string]bool{}}
	s.token = jwt(acct.SteamID, time.Now().Add(200*24*time.Hour))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ISteamDirectory/GetCMListForConnect/v1/", s.directory)
	mux.Handle("/cmsocket/", websocket.Handler(s.cm))
	mux.HandleFunc("GET /lancache-heartbeat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-LanCache-Processed-By", "fake-cache")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /depot/", s.content)
	s.HTTP = httptest.NewServer(mux)
	return s
}

// Close stops the server.
func (s *Server) Close() { s.HTTP.Close() }

// Addr is host:port of the server.
func (s *Server) Addr() string { return strings.TrimPrefix(s.HTTP.URL, "http://") }

// DirectoryURL is the CM list of the fake.
func (s *Server) DirectoryURL() string {
	return s.HTTP.URL + "/ISteamDirectory/GetCMListForConnect/v1/?cmtype=websockets"
}

// ConfirmAfter makes the QR sign-in succeed at the n-th poll.
func (s *Server) ConfirmAfter(n int) {
	s.mu.Lock()
	s.confirm, s.polls = n, 0
	s.mu.Unlock()
}

// RejectToken makes every logon with the token fail (Revoked).
func (s *Server) RejectToken(on bool) {
	s.mu.Lock()
	s.reject = on
	s.mu.Unlock()
}

// Forbid makes host answer 403 for depot.
func (s *Server) Forbid(host string, depot uint32) {
	s.mu.Lock()
	s.forbid[host+"|"+strconv.FormatUint(uint64(depot), 10)] = true
	s.mu.Unlock()
}

// Token is the refresh token the sign-in hands out.
func (s *Server) Token() string { return s.token }

// Revoked lists the tokens revoked with Authentication.RevokeToken.
func (s *Server) Revoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.revoked)
}

// Logons lists the logons.
func (s *Server) Logons() []Logon {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.logons)
}

// Requests lists the content requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ResetRequests forgets the content requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	s.requests = nil
	s.mu.Unlock()
}

func (s *Server) directory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"response":{"success":true,"message":"","serverlist":[`+
		`{"endpoint":"%s","type":"websockets","realm":"steamglobal","load":10,"wtd_load":12.5},`+
		`{"endpoint":"tcp.invalid:27017","type":"netfilter","realm":"steamglobal","load":1,"wtd_load":1}]}}`, s.Addr())
}

// --- content ---

// ChunkData returns the generated chunk i of a depot.
func ChunkData(d Depot, i int) []byte {
	b := make([]byte, d.ChunkSize)
	for j := range b {
		b[j] = byte(int(d.ID) + i*7 + j%251)
	}
	binary.LittleEndian.PutUint32(b, uint32(i))
	return b
}

// ChunkPaths lists the chunk paths of a depot.
func ChunkPaths(d Depot) []string {
	var out []string
	for i := range d.Chunks {
		sum := sha1.Sum(ChunkData(d, i))
		out = append(out, fmt.Sprintf("/depot/%d/chunk/%s", d.ID, hex.EncodeToString(sum[:])))
	}
	return out
}

func (s *Server) depot(id uint32) (Depot, bool) {
	for _, a := range s.apps {
		for _, d := range a.Depots {
			if d.ID == id && d.FromApp == 0 {
				return d, true
			}
		}
	}
	return Depot{}, false
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, Request{Host: r.Host, Path: r.URL.Path, UserAgent: r.UserAgent()})
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/depot/"), "/")
	id, _ := strconv.ParseUint(parts[0], 10, 32)
	host, _, _ := strings.Cut(r.Host, ":")
	forbidden := s.forbid[host+"|"+parts[0]]
	d, ok := s.depot(uint32(id))
	hit := s.hits[r.URL.Path]
	s.hits[r.URL.Path] = true
	s.mu.Unlock()
	w.Header().Set("X-LanCache-Processed-By", "fake-cache")
	if forbidden {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !ok || len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "manifest":
		gid, _ := strconv.ParseUint(parts[2], 10, 64)
		var code uint64
		if len(parts) == 5 {
			code, _ = strconv.ParseUint(parts[4], 10, 64)
		}
		s.mu.Lock()
		want := s.codes[fmt.Sprint(d.ID, "/", gid)]
		s.mu.Unlock()
		if gid != d.Manifest || code == 0 || code != want {
			http.Error(w, "bad request code", http.StatusUnauthorized)
			return
		}
		w.Write(Manifest(d))
	case "chunk":
		for i := range d.Chunks {
			data := ChunkData(d, i)
			sum := sha1.Sum(data)
			if hex.EncodeToString(sum[:]) == parts[2] {
				if hit {
					w.Header().Set("X-Upstream-Cache-Status", "HIT")
				} else {
					w.Header().Set("X-Upstream-Cache-Status", "MISS")
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Manifest builds the ZIP archive of a depot's manifest: payload (one file
// per two chunks, every chunk listed once more to test the de-duplication),
// metadata, signature, end.
func Manifest(d Depot) []byte {
	var payload []byte
	for i := 0; i < d.Chunks; i += 2 {
		var file []byte
		file = appendBytes(file, 1, []byte(base64.StdEncoding.EncodeToString([]byte(fmt.Sprint("file", i)))))
		for j := i; j < min(i+2, d.Chunks); j++ {
			sum := sha1.Sum(ChunkData(d, j))
			var c []byte
			c = appendBytes(c, 1, sum[:])
			c = appendVarint(c, 5, uint64(d.ChunkSize))
			file = appendBytes(file, 6, c)
		}
		payload = appendBytes(payload, 1, file)
	}
	if d.Chunks > 0 { // a duplicate of the first chunk in another file
		sum := sha1.Sum(ChunkData(d, 0))
		var c []byte
		c = appendBytes(c, 1, sum[:])
		c = appendVarint(c, 5, uint64(d.ChunkSize))
		payload = appendBytes(payload, 1, appendBytes(nil, 6, c))
	}
	var meta []byte
	meta = appendVarint(meta, 1, uint64(d.ID))
	meta = appendVarint(meta, 2, d.Manifest)
	var raw []byte
	section := func(magic uint32, b []byte) {
		raw = binary.LittleEndian.AppendUint32(raw, magic)
		raw = binary.LittleEndian.AppendUint32(raw, uint32(len(b)))
		raw = append(raw, b...)
	}
	section(0x71F617D0, payload)
	section(0x1F4812BE, meta)
	section(0x1B81B817, appendBytes(nil, 1, []byte("sig")))
	raw = binary.LittleEndian.AppendUint32(raw, 0x32C415AB)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("z")
	f.Write(raw)
	zw.Close()
	return buf.Bytes()
}

// Bytes is the download size of a depot.
func (d Depot) Bytes() int64 { return int64(d.Chunks * d.ChunkSize) }

// --- protobuf ---

func appendKey(b []byte, field, wire int) []byte {
	return binary.AppendUvarint(b, uint64(field)<<3|uint64(wire))
}

func appendVarint(b []byte, field int, v uint64) []byte {
	return binary.AppendUvarint(appendKey(b, field, 0), v)
}

func appendFixed64(b []byte, field int, v uint64) []byte {
	return binary.LittleEndian.AppendUint64(appendKey(b, field, 1), v)
}

func appendFloat(b []byte, field int, v float32) []byte {
	return binary.LittleEndian.AppendUint32(appendKey(b, field, 5), math.Float32bits(v))
}

func appendBytes(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(appendKey(b, field, 2), uint64(len(v)))
	return append(b, v...)
}

type pbField struct {
	num, wire int
	n         uint64
	b         []byte
}

func parse(b []byte) []pbField {
	var out []pbField
	for len(b) > 0 {
		k, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		f := pbField{num: int(k >> 3), wire: int(k & 7)}
		switch f.wire {
		case 0:
			f.n, n = binary.Uvarint(b)
			b = b[n:]
		case 1:
			f.n, b = binary.LittleEndian.Uint64(b), b[8:]
		case 5:
			f.n, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
		case 2:
			l, n := binary.Uvarint(b)
			f.b, b = b[n:n+int(l)], b[n+int(l):]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}

func get(fs []pbField, num int) (pbField, bool) {
	for _, f := range fs {
		if f.num == num {
			return f, true
		}
	}
	return pbField{}, false
}

// jwt builds an unsigned token with the claims the client reads.
func jwt(steamID uint64, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := fmt.Sprintf(`{"iss":"steam","sub":"%d","aud":["client","web","derive"],"exp":%d,"jti":"1A2B_3C4D5E6F_7A8B9"}`, steamID, exp.Unix())
	return enc([]byte(`{"typ":"JWT","alg":"EdDSA"}`)) + "." + enc([]byte(claims)) + "." + enc([]byte("signature"))
}

// --- CM ---

const protoMask = 0x80000000

type cmConn struct {
	s         *Server
	ws        *websocket.Conn
	wmu       sync.Mutex
	steamID   uint64
	sessionID int32
	authed    bool
}

func (s *Server) cm(ws *websocket.Conn) {
	ws.PayloadType = websocket.BinaryFrame
	c := &cmConn{s: s, ws: ws}
	for {
		var b []byte
		if err := websocket.Message.Receive(ws, &b); err != nil {
			return
		}
		if len(b) < 8 || binary.LittleEndian.Uint32(b)&protoMask == 0 {
			continue
		}
		emsg := binary.LittleEndian.Uint32(b) &^ protoMask
		hl := binary.LittleEndian.Uint32(b[4:])
		hdr := parse(b[8 : 8+hl])
		body := b[8+hl:]
		job := uint64(math.MaxUint64)
		if f, ok := get(hdr, 10); ok {
			job = f.n
		}
		if !c.handle(emsg, hdr, body, job) {
			ws.Close()
			return
		}
	}
}

// packet builds a protobuf packet answering job (MaxUint64: none).
func (c *cmConn) packet(emsg uint32, job uint64, eresult int32, name string, body []byte) []byte {
	var h []byte
	if c.steamID != 0 {
		h = appendFixed64(h, 1, c.steamID)
	}
	if c.sessionID != 0 {
		h = appendVarint(h, 2, uint64(c.sessionID))
	}
	if job != math.MaxUint64 {
		h = appendFixed64(h, 11, job)
	}
	if name != "" {
		h = appendBytes(h, 12, []byte(name))
	}
	h = appendVarint(h, 13, uint64(int64(eresult)))
	b := binary.LittleEndian.AppendUint32(nil, emsg|protoMask)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(h)))
	return append(append(b, h...), body...)
}

func (c *cmConn) send(b []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = websocket.Message.Send(c.ws, b)
}

// sendMulti wraps packets in a gzip CMsgMulti.
func (c *cmConn) sendMulti(packets ...[]byte) {
	var raw []byte
	for _, p := range packets {
		raw = binary.LittleEndian.AppendUint32(raw, uint32(len(p)))
		raw = append(raw, p...)
	}
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write(raw)
	zw.Close()
	var body []byte
	body = appendVarint(body, 1, uint64(len(raw)))
	body = appendBytes(body, 2, zb.Bytes())
	c.send(c.packet(1, math.MaxUint64, 1, "", body))
}

func (c *cmConn) handle(emsg uint32, hdr []pbField, body []byte, job uint64) bool {
	switch emsg {
	case 9805, 146: // ClientHello, notifications
	case 703:
		c.s.mu.Lock()
		c.s.beats++
		c.s.mu.Unlock()
	case 706: // ClientLogOff
		return false
	case 9804, 151:
		name := ""
		if f, ok := get(hdr, 12); ok {
			name = string(f.b)
		}
		if emsg == 151 && !c.authed {
			c.send(c.packet(147, job, 21, name, nil)) // NotLoggedOn
			return true
		}
		res, out := c.s.service(name, parse(body), c)
		c.send(c.packet(147, job, res, name, out))
	case 5514:
		c.logon(hdr, parse(body))
	case 8905:
		c.accessTokens(parse(body), job)
	case 8903:
		c.productInfo(parse(body), job)
	default:
		c.send(c.packet(113, job, 2, "", nil)) // DestJobFailed
	}
	return true
}

func (c *cmConn) logon(hdr []pbField, body []pbField) {
	s := c.s
	var l Logon
	if f, ok := get(hdr, 1); ok {
		l.SteamID = f.n
	}
	if f, ok := get(body, 50); ok {
		l.AccountName = string(f.b)
	}
	if f, ok := get(body, 108); ok {
		l.Token = string(f.b)
	}
	if f, ok := get(body, 11); ok {
		if g, ok := get(parse(f.b), 1); ok {
			l.LoginID = uint32(g.n)
		}
	}
	if f, ok := get(body, 96); ok {
		l.MachineName = string(f.b)
	}
	s.mu.Lock()
	s.logons = append(s.logons, l)
	ok := l.Token == s.token && !s.reject && !slices.Contains(s.revoked, l.Token)
	anon := l.Token == "" && l.SteamID>>52&0xF == 10
	acct := s.account
	s.mu.Unlock()
	if !ok && !anon {
		c.send(c.packet(751, math.MaxUint64, 1, "", appendVarint(nil, 1, 26))) // Revoked
		return
	}
	c.authed = true
	c.sessionID = 4711
	var resp []byte
	resp = appendVarint(resp, 1, 1)
	resp = appendVarint(resp, 2, 9)
	resp = appendVarint(resp, 7, 42)
	resp = appendVarint(resp, 30, 123456789)
	if anon {
		c.steamID = 1<<56 | 10<<52 | 99
		c.send(c.packet(751, math.MaxUint64, 1, "", resp))
		return
	}
	c.steamID = acct.SteamID
	var lic []byte
	lic = appendVarint(lic, 1, 1)
	ids := make([]uint32, 0, len(acct.Packages))
	for id := range acct.Packages {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		var l []byte
		l = appendVarint(l, 1, uint64(id))
		l = appendVarint(l, 17, uint64(id)*1000)
		lic = appendBytes(lic, 2, l)
	}
	// The logon answer and the license list in one Multi, like Steam.
	c.sendMulti(c.packet(751, math.MaxUint64, 1, "", resp), c.packet(780, math.MaxUint64, 1, "", lic))
}

func (s *Server) service(name string, body []pbField, c *cmConn) (int32, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "Authentication.BeginAuthSessionViaQR#1":
		dev, ok := get(body, 3)
		if !ok {
			return 8, nil
		}
		if _, ok := get(parse(dev.b), 1); !ok {
			return 8, nil
		}
		s.polls = 0
		var out []byte
		out = appendVarint(out, 1, 1001)
		out = appendBytes(out, 2, []byte("https://s.team/q/1/1001"))
		out = appendBytes(out, 3, []byte{1, 2, 3})
		out = appendFloat(out, 4, 1)
		return 1, out
	case "Authentication.PollAuthSessionStatus#1":
		id, _ := get(body, 1)
		if id.n != 1001 && id.n != 1002 {
			return 9, nil
		}
		s.polls++
		var out []byte
		switch {
		case s.confirm > 0 && s.polls >= s.confirm:
			out = appendBytes(out, 3, []byte(s.token))
			out = appendBytes(out, 6, []byte(s.account.Name))
		case s.polls == 1:
			out = appendVarint(out, 1, 1002)
			out = appendBytes(out, 2, []byte("https://s.team/q/1/1002"))
		default:
			out = appendVarint(out, 5, 1) // scanned
		}
		return 1, out
	case "Authentication.RevokeToken#1":
		tok, _ := get(body, 1)
		s.revoked = append(s.revoked, string(tok.b))
		return 1, nil
	case "ContentServerDirectory.GetServersForSteamPipe#1":
		var out []byte
		for i, h := range []string{"proxy.example", "cache1.example", "cache2.example"} {
			var sv []byte
			typ := "SteamCache"
			if i == 0 {
				typ = "OpenCache"
			}
			sv = appendBytes(sv, 1, []byte(typ))
			sv = appendFloat(sv, 5, float32(10+i))
			sv = appendBytes(sv, 8, []byte(h))
			sv = appendBytes(sv, 9, []byte(h))
			sv = appendBytes(sv, 12, []byte("mandatory"))
			out = appendBytes(out, 1, sv)
		}
		return 1, out
	case "ContentServerDirectory.GetManifestRequestCode#1":
		dep, _ := get(body, 2)
		gid, _ := get(body, 3)
		code := uint64(time.Now().UnixNano()) | 1
		s.codes[fmt.Sprint(dep.n, "/", gid.n)] = code
		return 1, appendVarint(nil, 1, code)
	}
	return 2, nil
}

func (c *cmConn) accessTokens(body []pbField, job uint64) {
	var out []byte
	for _, f := range body {
		if f.num == 2 {
			var t []byte
			t = appendVarint(t, 1, f.n)
			t = appendVarint(t, 2, f.n*7)
			out = appendBytes(out, 3, t)
		}
	}
	c.send(c.packet(8906, job, 1, "", out))
}

// productInfo answers in two messages (response_pending on the first).
func (c *cmConn) productInfo(body []pbField, job uint64) {
	s := c.s
	var parts [2][]byte
	n := 0
	for _, f := range body {
		sub := parse(f.b)
		id, _ := get(sub, 1)
		tok, _ := get(sub, 2)
		var p []byte
		p = appendVarint(p, 1, id.n)
		switch f.num {
		case 1:
			s.mu.Lock()
			apps, ok := s.account.Packages[uint32(id.n)]
			s.mu.Unlock()
			if id.n == 17906 {
				apps, ok = []uint32{1007}, true
			}
			if !ok || (id.n != 17906 && tok.n != id.n*1000) {
				p = appendVarint(p, 3, 1) // missing token
			} else {
				p = appendBytes(p, 5, s.packageKV(uint32(id.n), apps))
			}
			parts[n%2] = appendBytes(parts[n%2], 3, p)
		case 2:
			s.mu.Lock()
			app, ok := s.apps[uint32(id.n)]
			s.mu.Unlock()
			if !ok {
				parts[n%2] = appendVarint(parts[n%2], 2, id.n)
				continue
			}
			if tok.n != id.n*7 {
				p = appendVarint(p, 3, 1)
			} else {
				p = appendBytes(p, 5, appKV(uint32(id.n), app))
			}
			parts[n%2] = appendBytes(parts[n%2], 1, p)
		}
		n++
	}
	c.send(c.packet(8904, job, 1, "", appendVarint(parts[0], 6, 1)))
	c.send(c.packet(8904, job, 1, "", parts[1]))
}

func (s *Server) packageKV(id uint32, apps []uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, 1)
	cs := func(v string) { b = append(append(b, v...), 0) }
	i32 := func(k string, v uint32) {
		b = append(b, 2)
		cs(k)
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	b = append(b, 0)
	cs(strconv.FormatUint(uint64(id), 10))
	i32("packageid", id)
	b = append(b, 0)
	cs("appids")
	for i, a := range apps {
		i32(strconv.Itoa(i), a)
	}
	b = append(b, 8, 0)
	cs("depotids")
	i := 0
	s.mu.Lock()
	for _, a := range apps {
		for _, d := range s.apps[a].Depots {
			i32(strconv.Itoa(i), d.ID)
			i++
		}
	}
	s.mu.Unlock()
	b = append(b, 8, 8, 8)
	return b
}

func appKV(id uint32, a App) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\"appinfo\"\n{\n\t\"appid\"\t\"%d\"\n\t\"common\"\n\t{\n\t\t\"name\"\t\"%s\"\n\t\t\"type\"\t\"%s\"\n\t}\n", id, a.Name, a.Type)
	if len(a.DLC) > 0 {
		var ids []string
		for _, d := range a.DLC {
			ids = append(ids, strconv.FormatUint(uint64(d), 10))
		}
		fmt.Fprintf(&sb, "\t\"extended\" { \"listofdlc\" \"%s\" }\n", strings.Join(ids, ","))
	}
	sb.WriteString("\t\"depots\"\n\t{\n")
	for _, d := range a.Depots {
		fmt.Fprintf(&sb, "\t\t\"%d\"\n\t\t{\n\t\t\t\"config\" { ", d.ID)
		if d.OS != "" {
			fmt.Fprintf(&sb, "\"oslist\" \"%s\" ", d.OS)
		}
		if d.Language != "" {
			fmt.Fprintf(&sb, "\"language\" \"%s\" ", d.Language)
		}
		sb.WriteString("}\n")
		if d.FromApp != 0 {
			fmt.Fprintf(&sb, "\t\t\t\"depotfromapp\" \"%d\"\n\t\t\t\"sharedinstall\" \"1\"\n", d.FromApp)
		} else {
			fmt.Fprintf(&sb, "\t\t\t\"manifests\" { \"public\" { \"gid\" \"%d\" \"size\" \"%d\" \"download\" \"%d\" } }\n", d.Manifest, d.Bytes()*2, d.Bytes())
		}
		sb.WriteString("\t\t}\n")
	}
	sb.WriteString("\t\t\"branches\" { \"public\" { \"buildid\" \"1\" } }\n\t}\n}\n")
	return append([]byte(sb.String()), 0)
}

// Dial returns a DialContext that connects every host to the fake (the
// directory, the CM and the content servers).
func (s *Server) Dial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, s.Addr())
	}
}

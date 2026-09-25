package cachestore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObjectID(t *testing.T) {
	// Pinned vector: first 32 hex chars of SHA-256("steam\x00<path>").
	got := ObjectID("steam", "/depot/228990/chunk/0123456789abcdef0123456789abcdef01234567")
	if got != "5a7a741dce9c2bc67ec1b7c3d98a6996" {
		t.Fatalf("ObjectID = %s", got)
	}
	if !ValidObjectID(got) {
		t.Fatal("ObjectID result must be valid")
	}
	if ObjectID("a", "/b") == ObjectID("a/", "b") {
		t.Fatal("service and path must be separated")
	}
	for _, id := range []string{"", "5A7A741DCE9C2BC67EC1B7C3D98A6996", "5a7a741dce9c2bc67ec1b7c3d98a699", "5a7a741dce9c2bc67ec1b7c3d98a6996 ", "../../../../../../../../etc/pass"} {
		if ValidObjectID(id) {
			t.Errorf("ValidObjectID(%q) = true", id)
		}
	}
}

func TestParseSliceFile(t *testing.T) {
	id := ObjectID("steam", "/x")
	tests := []struct {
		name string
		idx  int64
		ok   bool
	}{
		{id + ".0", 0, true},
		{id + ".17", 17, true},
		{id + ".017", 0, false},
		{id + ".-1", 0, false},
		{id + ".+1", 0, false},
		{id + ".", 0, false},
		{id + ".1a", 0, false},
		{id + ".9999999999999", 0, false},
		{id, 0, false},
		{strings.ToUpper(id) + ".1", 0, false},
		{id + ".1.tmp", 0, false},
	}
	for _, tt := range tests {
		gotID, idx, ok := parseSliceFile(tt.name)
		if ok != tt.ok || ok && (gotID != id || idx != tt.idx) {
			t.Errorf("parseSliceFile(%q) = %q, %d, %v", tt.name, gotID, idx, ok)
		}
	}
}

// craft builds a slice file from a header and data (the header length can be
// overridden to simulate hostile files).
func craft(t *testing.T, h *sliceHeader, data []byte) []byte {
	t.Helper()
	b, err := encodePrefix(h)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, data...)
}

func TestHeaderValidation(t *testing.T) {
	const S = testSlice
	id := ObjectID("steam", "/depot/1/chunk/a")
	total := int64(S + 1000) // two slices, the last one 1000 bytes
	good := func() sliceHeader {
		return sliceHeader{O: id, I: 1, S: "steam", H: "cdn.example.com", P: "/depot/1/chunk/a", T: total, Z: S, C: 1, K: 7}
	}
	data := make([]byte, 1000)
	tests := []struct {
		name   string
		file   func() []byte
		wantOK bool
	}{
		{"valid", func() []byte { h := good(); return craft(t, &h, data) }, true},
		{"bad magic", func() []byte {
			h := good()
			b := craft(t, &h, data)
			copy(b, "XCS1")
			return b
		}, false},
		{"huge header length", func() []byte {
			h := good()
			b := craft(t, &h, data)
			binary.LittleEndian.PutUint32(b[4:], 0xFFFFFFFF)
			return b
		}, false},
		{"header length beyond file", func() []byte {
			h := good()
			b := craft(t, &h, nil)
			binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
			return b
		}, false},
		{"header length 65 KiB", func() []byte {
			b := make([]byte, 8+(65<<10))
			copy(b, sliceMagic)
			binary.LittleEndian.PutUint32(b[4:], 65<<10)
			return b
		}, false},
		{"not JSON", func() []byte {
			b := []byte("PCS1\x04\x00\x00\x00{{{{")
			return append(b, data...)
		}, false},
		{"duplicate JSON member", func() []byte {
			j := `{"o":"` + id + `","o":"` + id + `","i":1,"s":"steam","h":"x","p":"/depot/1/chunk/a","t":` +
				"263144" + `,"z":262144,"c":1,"k":7}`
			b := make([]byte, 8, 8+len(j))
			copy(b, sliceMagic)
			binary.LittleEndian.PutUint32(b[4:], uint32(len(j)))
			return append(append(b, j...), data...)
		}, false},
		{"mismatched object id", func() []byte {
			h := good()
			h.O = ObjectID("steam", "/other")
			h.P = "/other"
			return craft(t, &h, data)
		}, false},
		{"id not derived from service and path", func() []byte { h := good(); h.P = "/evil"; return craft(t, &h, data) }, false},
		{"mismatched index", func() []byte { h := good(); h.I = 0; return craft(t, &h, data) }, false},
		{"wrong slice size", func() []byte { h := good(); h.Z = 1 << 20; return craft(t, &h, data) }, false},
		{"total above 1 TiB", func() []byte { h := good(); h.T = MaxTotal + 1; return craft(t, &h, data) }, false},
		{"zero total", func() []byte { h := good(); h.T = 0; return craft(t, &h, data) }, false},
		{"index beyond total", func() []byte { h := good(); h.T = S; return craft(t, &h, data) }, false},
		{"file one byte short", func() []byte { h := good(); return craft(t, &h, data[:999]) }, false},
		{"file one byte long", func() []byte { h := good(); return craft(t, &h, append(data, 0)) }, false},
		{"too short", func() []byte { return []byte("PCS1") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.file()
			h, off, err := readHeader(bytes.NewReader(b), int64(len(b)))
			if err == nil {
				err = h.validate(id, 1, S, int64(len(b)), off)
			}
			if (err == nil) != tt.wantOK {
				t.Fatalf("err = %v, want ok=%v", err, tt.wantOK)
			}
			if err != nil && !isCorrupt(err) {
				t.Fatalf("validation error %v is not a corruption error", err)
			}
		})
	}
}

// TestReadSliceHostileFiles replaces a cached slice file with hostile
// content: ReadSlice must report the slice missing, delete the file and drop
// it from the index.
func TestReadSliceHostileFiles(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	const total = testSlice + 1000
	tests := []struct {
		name string
		mod  func(h *sliceHeader, data []byte) []byte
	}{
		{"garbage", func(*sliceHeader, []byte) []byte { return []byte("not a slice file at all") }},
		{"other object", func(h *sliceHeader, data []byte) []byte {
			h.P += "x"
			h.O = ObjectID(h.S, h.P)
			return craft(t, h, data)
		}},
		{"other total", func(h *sliceHeader, data []byte) []byte {
			h.T++
			return craft(t, h, append(data, 0))
		}},
		{"truncated", func(h *sliceHeader, data []byte) []byte { return craft(t, h, data[:len(data)-1]) }},
		{"huge header length", func(h *sliceHeader, data []byte) []byte {
			b := craft(t, h, data)
			binary.LittleEndian.PutUint32(b[4:], 1<<31)
			return b
		}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/hostile/" + string(rune('a'+i))
			id, gen := putObject(t, s, "steam", path, "steam:h", total)
			h := sliceHeader{O: id, I: 1, S: "steam", H: "cdn.example.com", P: path, T: total, Z: testSlice, C: 1}
			file := filepath.Join(e.root, filepath.FromSlash(sliceName(id, 1)))
			if err := os.WriteFile(file, tt.mod(&h, payload(id, 1, 1000)), 0o640); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReadSlice(ctx, id, gen, 1); !errors.Is(err, ErrSliceMissing) {
				t.Fatalf("ReadSlice = %v, want ErrSliceMissing", err)
			}
			if fileExists(t, e, id, 1) {
				t.Fatal("corrupt file not deleted")
			}
			if s.HasSlice(ctx, id, gen, 1) || !s.HasSlice(ctx, id, gen, 0) {
				t.Fatal("only the corrupt slice must be dropped")
			}
		})
	}
	checkAggregates(t, s)
}

func TestValidateMeta(t *testing.T) {
	base := func() Meta {
		return Meta{Service: "steam", Host: "cdn.example.com", Path: "/a", GroupKey: "steam:x", Total: 10}
	}
	tests := []struct {
		name string
		mod  func(*Meta)
		ok   bool
	}{
		{"valid", func(*Meta) {}, true},
		{"bad service", func(m *Meta) { m.Service = "Steam!" }, false},
		{"host with slash", func(m *Meta) { m.Host = "a/b" }, false},
		{"relative path", func(m *Meta) { m.Path = "a" }, false},
		{"path with newline", func(m *Meta) { m.Path = "/a\nb" }, false},
		{"path invalid utf8", func(m *Meta) { m.Path = "/\xff" }, false},
		{"empty group", func(m *Meta) { m.GroupKey = "" }, false},
		{"zero total", func(m *Meta) { m.Total = 0 }, false},
		{"too large", func(m *Meta) { m.Total = MaxTotal + 1 }, false},
	}
	for _, tt := range tests {
		m := base()
		tt.mod(&m)
		if err := validateMeta(ObjectID(m.Service, m.Path), &m); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v", tt.name, err)
		}
	}
	m := base()
	if err := validateMeta(ObjectID("steam", "/other"), &m); err == nil {
		t.Error("id not derived from service+path must be rejected")
	}
	if _, _, err := cleanHeader(http.Header{"X-A": {"a\r\nSet-Cookie: x"}}); err == nil {
		t.Error("header value with CRLF must be rejected")
	}
	if _, _, err := cleanHeader(http.Header{"X-A": {strings.Repeat("a", 5000)}}); err == nil {
		t.Error("headers above 4 KiB must be rejected")
	}
	if _, _, err := cleanHeader(http.Header{"Bad Name": {"a"}}); err == nil {
		t.Error("invalid header name must be rejected")
	}
	if got := rebuildHeader(http.Header{"Content-Type": {"a\x00"}, "Last-Modified": {"x"}, "X-Other": {"y"}}); len(got) != 1 || got.Get("Last-Modified") != "x" {
		t.Errorf("rebuildHeader = %v", got)
	}
}

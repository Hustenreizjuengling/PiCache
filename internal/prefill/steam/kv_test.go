package steam

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestTextKV(t *testing.T) {
	in := "\xef\xbb\xbf\"appinfo\"\n{\n\t\"appid\"\t\t\"440\"\n\t// a comment\n\t\"common\"\n\t{\n\t\t\"name\"\t\t\"Team \\\"Fortress\\\" 2\"\n\t\t\"type\" \"Game\" [$WIN32]\n\t}\n\t\"depots\" {\n\t\t\"441\" { \"config\" { \"oslist\" \"windows\" } \"manifests\" { \"public\" { \"gid\" \"7372428733462413430\" } } }\n\t\tbare token\n\t}\n}\n\x00garbage"
	root, err := parseTextKV([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	app := root.child("appinfo")
	if got := app.uint("appid"); got != 440 {
		t.Errorf("appid = %d", got)
	}
	if got := app.str("common", "name"); got != `Team "Fortress" 2` {
		t.Errorf("name = %q", got)
	}
	if got := app.str("Common", "TYPE"); got != "Game" {
		t.Errorf("type = %q (case-insensitive lookup)", got)
	}
	if got := app.uint("depots", "441", "manifests", "public", "gid"); got != 7372428733462413430 {
		t.Errorf("gid = %d", got)
	}
	if got := app.str("depots", "bare"); got != "token" {
		t.Errorf("unquoted = %q", got)
	}
	if app.get("missing", "x") != nil || app.str("missing") != "" || app.uint("common", "name") != 0 {
		t.Error("missing paths must be empty")
	}

	for _, bad := range []string{`"a" {`, `"a" }`, `"a"`, `{ "a" "b" }`, `"a" "b" }`, `"unterminated`} {
		if _, err := parseTextKV([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	deep := strings.Repeat(`"a" {`, kvMaxDepth+2) + strings.Repeat("}", kvMaxDepth+2)
	if _, err := parseTextKV([]byte(deep)); err == nil {
		t.Error("depth limit not enforced")
	}
}

func TestBinaryKV(t *testing.T) {
	var b bytes.Buffer
	cs := func(s string) { b.WriteString(s); b.WriteByte(0) }
	b.WriteByte(kvbSection)
	cs("17906")
	b.WriteByte(kvbInt32)
	cs("packageid")
	_ = binary.Write(&b, binary.LittleEndian, int32(17906))
	b.WriteByte(kvbSection)
	cs("appids")
	b.WriteByte(kvbInt32)
	cs("0")
	_ = binary.Write(&b, binary.LittleEndian, int32(90))
	b.WriteByte(kvbInt32)
	cs("1")
	_ = binary.Write(&b, binary.LittleEndian, int32(-1))
	b.WriteByte(kvbEnd)
	b.WriteByte(kvbString)
	cs("name")
	cs("Anonymous Dedicated Server Comp")
	b.WriteByte(kvbUint64)
	cs("big")
	_ = binary.Write(&b, binary.LittleEndian, uint64(1<<63))
	b.WriteByte(kvbWString)
	cs("w")
	_ = binary.Write(&b, binary.LittleEndian, []uint16{'h', 'i', 0})
	b.WriteByte(kvbEnd)
	b.WriteByte(kvbEnd)

	root, err := parseBinaryKV(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	pkg := root.child("17906")
	if pkg.uint("packageid") != 17906 || pkg.uint("appids", "0") != 90 || pkg.str("appids", "1") != "-1" {
		t.Errorf("package = %+v", pkg)
	}
	if pkg.str("name") != "Anonymous Dedicated Server Comp" || pkg.str("big") != "9223372036854775808" || pkg.str("w") != "hi" {
		t.Errorf("values = %q %q %q", pkg.str("name"), pkg.str("big"), pkg.str("w"))
	}
	// Every truncation fails cleanly or parses a prefix, never panics.
	full := b.Bytes()
	for i := range full {
		_, _ = parseBinaryKV(full[:i])
	}
	if _, err := parseBinaryKV([]byte{0x42, 'k', 0}); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestWire(t *testing.T) {
	var e pb
	e.uint(1, 300)
	e.int32(2, -2)
	e.bool(3, true)
	e.fixed64(4, 0x0110000100000000)
	e.fixed32(5, 0x3f800000)
	e.string(6, "hi")
	var sub pb
	sub.uint(1, 7)
	e.bytes(7, sub.b)
	e.bytes(8, []byte{1, 0x96, 0x01}) // packed 1, 150

	var got []field
	for f, err := range fields(e.b) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, f)
	}
	if len(got) != 8 || got[0].n != 300 || got[1].int32() != -2 || !got[2].bool() ||
		got[3].n != 0x0110000100000000 || got[4].float32() != 1 || got[5].str() != "hi" {
		t.Fatalf("fields = %+v", got)
	}
	for f, err := range fields(got[6].b) {
		if err != nil || f.num != 1 || f.n != 7 {
			t.Errorf("sub = %+v %v", f, err)
		}
	}
	vals, err := packedUints(nil, got[7])
	if err != nil || len(vals) != 2 || vals[0] != 1 || vals[1] != 150 {
		t.Errorf("packed = %v %v", vals, err)
	}
	for i := range e.b {
		for _, err := range fields(e.b[:i]) {
			_ = err
		}
	}
	for _, err := range fields([]byte{0x0B}) { // wire type 3
		if err == nil {
			t.Error("group accepted")
		}
	}
}

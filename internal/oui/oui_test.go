package oui

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestTableParses(t *testing.T) {
	if len(table) > MaxSize {
		t.Fatalf("oui.bin is %d bytes, more than %d", len(table), MaxSize)
	}
	p := load(table)
	if p == nil {
		t.Fatal("oui.bin does not parse")
	}
	for i, r := range p.regs {
		size := r.keySize + OffsetSize
		if len(r.recs)%size != 0 || len(r.recs) == 0 {
			t.Fatalf("registry %d: %d bytes", i, len(r.recs))
		}
		n := len(r.recs) / size
		for j := 1; j < n; j++ {
			if r.key(j-1) >= r.key(j) {
				t.Fatalf("registry %d: record %d is not sorted", i, j)
			}
		}
		for j := range n {
			rec := r.recs[j*size+r.keySize : (j+1)*size]
			off := int(rec[0])<<16 | int(rec[1])<<8 | int(rec[2])
			if p.name(off) == "" {
				t.Fatalf("registry %d: record %d has no name", i, j)
			}
		}
	}
	// Every name is clean (like other untrusted names).
	for off := 0; off < len(p.names); {
		n := int(binary.BigEndian.Uint16(p.names[off:]))
		s := string(p.names[off+2 : off+2+n])
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > MaxNameLen || s != strings.Join(strings.Fields(s), " ") {
			t.Fatalf("name %q is not clean", s)
		}
		for _, r := range s {
			if unicode.In(r, unicode.Cc, unicode.Cf) {
				t.Fatalf("name %q has a control or format character", s)
			}
		}
		off += 2 + n
	}
}

func TestLookup(t *testing.T) {
	for _, tc := range []struct {
		mac, vendor string
		randomized  bool
	}{
		// MA-L
		{"b8:27:eb:12:34:56", "Raspberry Pi Foundation", false},
		{"DC-A6-32-00-00-01", "Raspberry Pi Trading Ltd", false},
		{"dca632000001", "Raspberry Pi Trading Ltd", false},
		// MA-M wins over its MA-L block (the registration authority's).
		{"c8:5c:e2:a0:00:01", "San Telequip (P) Ltd.,", false},
		{"c8:5c:e2:f0:00:01", "IEEE Registration Authority", false}, // no MA-M block C85CE2F
		// MA-S wins over MA-M and MA-L.
		{"8c:1f:64:af:a0:01", "DATA ELECTRONIC DEVICES, INC", false},
		{"8c:1f:64:00:00:01", "Suzhou Xingxiangyi Precision Manufacturing Co.,Ltd.", false},
		{"70:b3:d5:00:10:00", "SOREDI touch systems GmbH", false},
		{"8c:1f:64:ff:f0:00", "IEEE Registration Authority", false}, // no MA-S block 8C1F64FFF
		// Locally administered: randomised, never a vendor.
		{"ba:27:eb:12:34:56", "", true},
		{"da:a6:32:00:00:01", "", true},
		{"02:00:00:00:00:01", "", true},
		// Group addresses: no vendor.
		{"01:00:5e:00:00:01", "", false},
		{"33:33:00:00:00:01", "", true},
		{"ff:ff:ff:ff:ff:ff", "", true},
		// Unknown and malformed.
		{"", "", false},
		{"b8:27:eb:12:34", "", false},
		{"b8:27:eb:12:34:56:78", "", false},
		{"b8:27-eb:12:34:56", "", false},
		{"b8::27:eb:12:34:56", "", false},
		{"b827.eb12.3456", "", false},
		{"zz:27:eb:12:34:56", "", false},
		{"b8:27:eb:12:34:5", "", false},
	} {
		v, r := Lookup(tc.mac)
		if v != tc.vendor || r != tc.randomized {
			t.Errorf("Lookup(%q) = %q, %v; want %q, %v", tc.mac, v, r, tc.vendor, tc.randomized)
		}
	}
}

func TestLoadRejectsMalformed(t *testing.T) {
	if load(nil) != nil || load([]byte("PCOUI")) != nil {
		t.Fatal("a short table must not parse")
	}
	bad := append([]byte{}, table...)
	bad = bad[:len(bad)-1]
	if load(bad) != nil {
		t.Fatal("a table whose size does not match its header must not parse")
	}
	bad = append([]byte{}, table...)
	bad[0] = 'X'
	if load(bad) != nil {
		t.Fatal("a table with another magic must not parse")
	}
	// An empty table (the fallback when the data cannot be shipped) gives
	// no vendor.
	empty := []byte(Magic + strings.Repeat("\x00", 16))
	p := load(empty)
	if p == nil || p.lookup(0xb827eb123456) != "" {
		t.Fatal("an empty table must parse and name nothing")
	}
}

func FuzzLookup(f *testing.F) {
	for _, s := range []string{"b8:27:eb:12:34:56", "8c-1f-64-af-a0-01", "dca632000001", ":::::", "ffffffffffff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, _ := Lookup(s)
		if v != "" && !utf8.ValidString(v) {
			t.Fatalf("invalid UTF-8 vendor %q", v)
		}
	})
}

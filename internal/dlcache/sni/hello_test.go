package sni

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// captureHello returns the raw bytes crypto/tls writes as its first flight.
func captureHello(t testing.TB, cfg *tls.Config) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tls.Client(c1, cfg).Handshake() // fails once c2 is closed
		c1.Close()
	}()
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, _, err := readClientHello(c2)
	c2.Close()
	<-done
	if err != nil {
		t.Fatalf("read ClientHello: %v", err)
	}
	return raw
}

// buildHello builds a ClientHello handshake message with the given
// extensions (already encoded).
func buildHello(exts ...[]byte) []byte {
	var body []byte
	body = append(body, 3, 3)                           // legacy_version
	body = append(body, bytes.Repeat([]byte{7}, 32)...) // random
	body = append(body, 32)
	body = append(body, bytes.Repeat([]byte{9}, 32)...) // session id
	body = append(body, 0, 2, 0x13, 0x01)               // cipher suites
	body = append(body, 1, 0)                           // compression
	all := bytes.Join(exts, nil)
	body = binary.BigEndian.AppendUint16(body, uint16(len(all)))
	body = append(body, all...)
	msg := []byte{handshakeClientHello, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}

func ext(typ uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

func sniExt(nameType byte, name string) []byte {
	entry := append([]byte{nameType}, binary.BigEndian.AppendUint16(nil, uint16(len(name)))...)
	entry = append(entry, name...)
	list := binary.BigEndian.AppendUint16(nil, uint16(len(entry)))
	return ext(extServerName, append(list, entry...))
}

func padExt(n int) []byte { return ext(21, make([]byte, n)) }

// records splits a handshake message into TLS records of at most frag bytes.
func records(msg []byte, frag int) []byte {
	var out []byte
	for len(msg) > 0 {
		n := min(frag, len(msg))
		out = append(out, recordTypeHandshake, 3, 1, byte(n>>8), byte(n))
		out = append(out, msg[:n]...)
		msg = msg[n:]
	}
	return out
}

func TestCryptoTLSClientHello(t *testing.T) {
	cfgs := map[string]*tls.Config{
		"default (post-quantum key share)": {ServerName: "Lancache.SteamContent.com"},
		"many ALPN protocols": {ServerName: "cdn.example.org", NextProtos: func() []string {
			var p []string
			for i := range 200 {
				p = append(p, strings.Repeat("h", 40)+string(rune('a'+i%26)))
			}
			return p
		}()},
	}
	for name, cfg := range cfgs {
		t.Run(name, func(t *testing.T) {
			raw := captureHello(t, cfg)
			if len(raw) < 1000 {
				t.Fatalf("unexpectedly small ClientHello: %d bytes", len(raw))
			}
			want := strings.ToLower(cfg.ServerName)
			// As written, and re-fragmented into many records read one byte
			// at a time (a large hello split across TCP segments/records).
			_, hello, err := readClientHello(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			for _, frag := range []int{len(hello), 1000, 333, 7} {
				if (len(hello)+frag-1)/frag > maxHelloRecords {
					continue
				}
				in := records(hello, frag)
				gotRaw, gotHello, err := readClientHello(iotest.OneByteReader(bytes.NewReader(append(in, "APPDATA"...))))
				if err != nil {
					t.Fatalf("frag %d: %v", frag, err)
				}
				if !bytes.Equal(gotRaw, in) || !bytes.Equal(gotHello, hello) {
					t.Fatalf("frag %d: replay bytes differ", frag)
				}
				sni, err := serverName(gotHello)
				if err != nil || sni != want {
					t.Fatalf("frag %d: serverName = %q, %v", frag, sni, err)
				}
			}
		})
	}
}

func TestLargeSyntheticHello(t *testing.T) {
	hello := buildHello(padExt(9000), sniExt(0, "big.example.com"), padExt(3000))
	raw, got, err := readClientHello(bytes.NewReader(records(hello, 4096)))
	if err != nil || !bytes.Equal(got, hello) || len(raw) != len(hello)+(len(hello)+4095)/4096*recordHeaderLen {
		t.Fatalf("readClientHello: %v (raw %d)", err, len(raw))
	}
	if sni, err := serverName(got); err != nil || sni != "big.example.com" {
		t.Fatalf("serverName = %q, %v", sni, err)
	}
}

func TestReadClientHelloErrors(t *testing.T) {
	valid := buildHello(sniExt(0, "a.example.com"))
	tooBig := buildHello(padExt(maxHelloLen), sniExt(0, "a.example.com"))
	manyRecords := records(buildHello(padExt(200), sniExt(0, "a.example.com")), 1)
	tests := map[string][]byte{
		"empty":              nil,
		"http":               []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"alert record":       {0x15, 3, 1, 0, 2, 2, 40},
		"ssl2 version":       {recordTypeHandshake, 2, 0, 0, 4, 1, 0, 0, 0},
		"zero-length record": {recordTypeHandshake, 3, 1, 0, 0},
		"record too long":    {recordTypeHandshake, 3, 1, 0x40, 0x01},
		"truncated record":   records(valid, len(valid))[:20],
		"truncated message":  records(valid[:len(valid)-5], len(valid)),
		"server hello":       records(append([]byte{2}, valid[1:]...), len(valid)),
		"hello too large":    records(tooBig, maxRecordLen),
		"too many records":   manyRecords,
		"interleaved alert":  append(records(valid[:10], 10), 0x15, 3, 1, 0, 2, 2, 40),
	}
	for name, in := range tests {
		if _, _, err := readClientHello(bytes.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServerNameErrors(t *testing.T) {
	valid := buildHello(sniExt(0, "a.example.com"))
	tests := map[string]struct {
		hello []byte
		want  error
	}{
		"no extensions":      {valid[:4+2+32+33+4+2], errNoSNI},
		"no sni":             {buildHello(padExt(10)), errNoSNI},
		"other name type":    {buildHello(sniExt(1, "a.example.com")), errNoSNI},
		"ip literal":         {buildHello(sniExt(0, "192.0.2.1")), nil},
		"bad chars":          {buildHello(sniExt(0, "a b.example.com")), nil},
		"empty label":        {buildHello(sniExt(0, "a..example.com")), nil},
		"empty name":         {buildHello(sniExt(0, "")), nil},
		"nul":                {buildHello(sniExt(0, "a\x00.example.com")), nil},
		"long label":         {buildHello(sniExt(0, strings.Repeat("a", 64)+".com")), nil},
		"trailing garbage":   {append(valid, 0), errHello},
		"odd cipher suites":  {bytes.Replace(valid, []byte{0, 2, 0x13, 0x01}, []byte{0, 1, 0x13}, 1), errHello},
		"truncated":          {valid[:len(valid)-3], errHello},
		"not a client hello": {append([]byte{2}, valid[1:]...), errHello},
		"empty sni list":     {buildHello(ext(extServerName, []byte{0, 0})), errHello},
		"sni list overrun":   {buildHello(ext(extServerName, []byte{0, 9, 0, 0, 1, 'a'})), errHello},
	}
	for name, tt := range tests {
		sni, err := serverName(tt.hello)
		if err == nil {
			t.Errorf("%s: accepted %q", name, sni)
			continue
		}
		if tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tt.want)
		}
	}
	for in, want := range map[string]string{
		"Example.COM.":       "example.com",
		"_srv.example.com":   "_srv.example.com",
		"xn--bcher-kva.test": "xn--bcher-kva.test",
	} {
		if got, err := normalizeServerName(in); err != nil || got != want {
			t.Errorf("normalizeServerName(%q) = %q, %v", in, got, err)
		}
	}
}

func FuzzServerName(f *testing.F) {
	f.Add(buildHello(sniExt(0, "a.example.com")))
	f.Add(buildHello(padExt(100), sniExt(0, "cdn.example.org")))
	f.Add(buildHello(ext(extServerName, []byte{0, 3, 0, 0, 0})))
	f.Add([]byte{1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, hello []byte) {
		sni, err := serverName(hello)
		if err != nil {
			return
		}
		if again, err := normalizeServerName(sni); err != nil || again != sni {
			t.Fatalf("accepted a non-normalised name %q", sni)
		}
	})
}

func FuzzReadClientHello(f *testing.F) {
	hello := buildHello(sniExt(0, "a.example.com"))
	f.Add(records(hello, len(hello)))
	f.Add(records(hello, 5))
	f.Add([]byte{recordTypeHandshake, 3, 1, 0, 1, 1})
	f.Fuzz(func(t *testing.T, in []byte) {
		raw, msg, err := readClientHello(bytes.NewReader(in))
		if err != nil {
			if raw != nil || msg != nil {
				t.Fatal("data returned with an error")
			}
			return
		}
		if !bytes.HasPrefix(in, raw) {
			t.Fatal("raw is not the consumed input")
		}
		if len(msg) > maxHelloLen || len(raw) > maxHelloRecords*recordHeaderLen+maxHelloLen+maxRecordLen {
			t.Fatalf("bounds exceeded: msg %d raw %d", len(msg), len(raw))
		}
		_, _ = serverName(msg)
	})
}

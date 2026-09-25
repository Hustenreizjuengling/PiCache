package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSumsSize  = 64 << 10 // SHA256SUMS
	maxSigSize   = 64 << 10 // SHA256SUMS.sig
	maxSumsLines = 256
	maxMessage   = 500 // runes of a status message
)

// verifySignature checks sig (SHA256SUMS.sig: one line of base64, the
// Ed25519 signature over the exact bytes of sums) against keys (base64 raw
// public keys).
func verifySignature(sums, sig []byte, keys []string) error {
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("SHA256SUMS.sig is not a base64-encoded Ed25519 signature")
	}
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(pub, sums, raw) {
			return nil
		}
	}
	return errors.New("the signature of SHA256SUMS does not match any trusted release key")
}

// parseSums parses sha256sum output: one "<64 hex>  <name>" line per file
// ("<hex> *<name>" in binary mode). Names are plain file names.
func parseSums(b []byte) (map[string][32]byte, error) {
	out := map[string][32]byte{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(out) >= maxSumsLines {
			return nil, errors.New("SHA256SUMS has too many lines")
		}
		sum, name, ok := strings.Cut(line, " ")
		name, bin := strings.CutPrefix(name, "*")
		if !bin {
			name, ok = strings.CutPrefix(name, " ")
		}
		var h [32]byte
		if n, err := hex.Decode(h[:], []byte(sum)); !ok || err != nil || n != 32 || len(sum) != 64 {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed", i+1)
		}
		if name == "" || strings.ContainsAny(name, "/\\ ") || name == "." || name == ".." {
			return nil, fmt.Errorf("SHA256SUMS line %d has an invalid file name", i+1)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("SHA256SUMS lists %s twice", name)
		}
		out[name] = h
	}
	return out, nil
}

// clip shortens s to at most n runes (valid UTF-8 in, valid UTF-8 out).
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// sanitizeMessage makes a message single-line, valid UTF-8, printable and
// short (status files are read back and shown by the service).
func sanitizeMessage(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), maxMessage)
}

package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The compiled-in key is the one published as docs/release-key.pem.
func TestTrustedKeyMatchesPublishedKey(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "release-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatal("docs/release-key.pem is not a PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ek, ok := pub.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("docs/release-key.pem is a %T, not Ed25519", pub)
	}
	if len(trustedKeys) == 0 || trustedKeys[0] != base64.StdEncoding.EncodeToString(ek) {
		t.Fatalf("trustedKeys[0] = %v, docs/release-key.pem has %s", trustedKeys, base64.StdEncoding.EncodeToString(ek))
	}
}

func TestVerifySignature(t *testing.T) {
	files := releaseFiles(fakeBinary("v1.0.0"))
	sums, sig := files[SumsFile], files[SigFile]
	keys := []string{base64.StdEncoding.EncodeToString(testKey.Public().(ed25519.PublicKey))}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	otherKey := base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey))

	if err := verifySignature(sums, sig, keys); err != nil {
		t.Fatalf("valid signature: %v", err)
	}
	// Rotation: any key of the list may sign.
	if err := verifySignature(sums, sig, []string{"not base64!", otherKey, keys[0]}); err != nil {
		t.Fatalf("second key of the list: %v", err)
	}
	for name, tc := range map[string]struct {
		sums, sig []byte
		keys      []string
		want      string
	}{
		"wrong key":       {sums, sig, []string{otherKey}, "does not match any trusted release key"},
		"no keys":         {sums, sig, nil, "does not match any trusted release key"},
		"tampered sums":   {flipFirst(sums), sig, keys, "does not match"},
		"appended line":   {append(bytes.Clone(sums), "00  x\n"...), sig, keys, "does not match"},
		"signed by other": {sums, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(other, sums))), keys, "does not match"},
		"not base64":      {sums, []byte("%%%"), keys, "not a base64-encoded Ed25519 signature"},
		"short signature": {sums, []byte(base64.StdEncoding.EncodeToString([]byte("short"))), keys, "not a base64-encoded"},
		"empty signature": {sums, nil, keys, "not a base64-encoded"},
	} {
		if err := verifySignature(tc.sums, tc.sig, tc.keys); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseSums(t *testing.T) {
	h := strings.Repeat("ab", 32)
	got, err := parseSums([]byte(h + "  picache-linux-amd64\n" + strings.Repeat("cd", 32) + " *SHA256SUMS.sig\r\n\n"))
	if err != nil || len(got) != 2 || got["picache-linux-amd64"][0] != 0xab || got["SHA256SUMS.sig"][0] != 0xcd {
		t.Fatalf("parseSums = %v, %v", got, err)
	}
	for _, bad := range []string{
		h + " picache-linux-amd64\n",                         // one space, no '*'
		h[:62] + "  picache-linux-amd64\n",                   // short hash
		strings.Repeat("zz", 32) + "  picache-linux-amd64\n", // not hex
		h + "  ../picache\n",                                 // path
		h + "  a b\n",                                        // space in the name
		h + "  \n",                                           // no name
		h + "  x\n" + h + "  x\n",                            // duplicate
		h + "  x\njunk\n",                                    // garbage line
	} {
		if _, err := parseSums([]byte(bad)); err == nil {
			t.Errorf("parseSums(%q) succeeded", bad)
		}
	}
}

func TestSanitizeMessage(t *testing.T) {
	if got := sanitizeMessage(" a\nb\x00c\xff "); got != "a b c?" {
		t.Errorf("sanitizeMessage = %q", got)
	}
	if got := sanitizeMessage(strings.Repeat("ä", 600)); len([]rune(got)) != maxMessage+1 {
		t.Errorf("long message: %d runes", len([]rune(got)))
	}
}

// flipFirst returns a copy of b with the first byte changed.
func flipFirst(b []byte) []byte {
	c := bytes.Clone(b)
	c[0] ^= 1
	return c
}

// Package battlenet prefills Blizzard's Battle.net games (TACT/NGDP)
// without a sign-in: the current build of a product is public. It reads
// the build's configs and manifests, chooses the files a Windows install
// in the chosen language downloads (the manifests' tags) and maps them to
// archive ranges and loose files on the CDN (docs/ARCHITECTURE.md 20.8).
package battlenet

import (
	"encoding/hex"
	"fmt"
)

// Key is a content or encoding key (an MD5).
type Key [16]byte

// ParseKey reads 32 hex digits.
func ParseKey(s string) (Key, error) {
	var k Key
	if len(s) != 32 {
		return k, fmt.Errorf("battlenet: bad key %q", s)
	}
	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return k, fmt.Errorf("battlenet: bad key %q", s)
	}
	return k, nil
}

func (k Key) String() string { return hex.EncodeToString(k[:]) }

// IsZero reports an unset key.
func (k Key) IsZero() bool { return k == Key{} }

// hashPath is "xx/yy/<key>", the CDN layout of every file.
func hashPath(k string) string { return k[0:2] + "/" + k[2:4] + "/" + k }

func cmpKey(a, b *Key) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

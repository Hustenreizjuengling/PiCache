// Package secrets seals small secrets (NAS passwords) at rest with an AEAD
// under a 32-byte master key.
//
// This protects secrets in database copies and backups; it does not protect
// against a compromised PiCache process (which necessarily holds the key).
// Key sources, first match wins:
//  1. $CREDENTIALS_DIRECTORY/picache-master-key (systemd LoadCredentialEncrypted)
//  2. /run/secrets/picache_master_key (Docker secret)
//  3. the configured key file (PICACHE_MASTER_KEY_FILE or <data>/keys/master.key),
//     generated with mode 0600 if missing.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

const prefix = "v1:"

// Box seals and opens secrets. It is safe for concurrent use.
type Box struct {
	key    []byte
	Source string // where the key came from (for the UI; never the key itself)
}

// Open loads the master key, generating keyFile if no key exists anywhere.
func Open(keyFile string) (*Box, error) {
	candidates := []string{}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "picache-master-key"))
	}
	candidates = append(candidates, "/run/secrets/picache_master_key")
	for _, c := range candidates {
		if key, err := readKey(c, true); err == nil {
			return &Box{key: key, Source: c}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: %s: %w", c, err)
		}
	}
	key, err := readKey(keyFile, true)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, chacha20poly1305.KeySize)
		rand.Read(key)
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
			return nil, fmt.Errorf("secrets: create key dir: %w", err)
		}
		enc := hex.EncodeToString(key) + "\n"
		f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("secrets: create key file: %w", err)
		}
		if _, err := f.WriteString(enc); err != nil {
			f.Close()
			return nil, fmt.Errorf("secrets: write key file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("secrets: write key file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("secrets: %s: %w", keyFile, err)
	}
	return &Box{key: key, Source: keyFile}, nil
}

// Load loads the master key like Open but never generates one. It is used
// by the root helper, which must not trust the data directory the service
// owns: the key file must be a regular file and is never reached through a
// symbolic link (a link to /dev/zero, a FIFO or a root-only file is
// refused), and at most maxKeyFileSize bytes are read.
func Load(keyFile string) (*Box, error) {
	candidates := []string{}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "picache-master-key"))
	}
	candidates = append(candidates, "/run/secrets/picache_master_key", keyFile)
	for _, c := range candidates {
		key, err := readKey(c, false)
		if err == nil {
			return &Box{key: key, Source: c}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secrets: %s: %w", c, err)
		}
	}
	return nil, fmt.Errorf("secrets: no master key found (looked at %s)", strings.Join(candidates, ", "))
}

// New returns a Box for a raw 32-byte key (tests).
func New(key []byte) (*Box, error) {
	if len(key) != chacha20poly1305.KeySize {
		return nil, errors.New("secrets: key must be 32 bytes")
	}
	return &Box{key: append([]byte(nil), key...), Source: "memory"}, nil
}

// maxKeyFileSize bounds a key file read: 64 hex characters plus a line
// ending and some whitespace.
const maxKeyFileSize = 1 << 10

// readKey accepts 64 hex characters or 32 raw bytes. The file must be a
// regular file of at most maxKeyFileSize bytes; with follow=false a symbolic
// link as the last path element is refused.
func readKey(path string, follow bool) ([]byte, error) {
	b, err := readKeyFile(path, follow)
	if err != nil {
		return nil, err
	}
	if s := strings.TrimSpace(string(b)); len(s) == 64 {
		if k, err := hex.DecodeString(s); err == nil {
			return k, nil
		}
	}
	if len(b) == chacha20poly1305.KeySize {
		return b, nil
	}
	return nil, errors.New("key must be 32 raw bytes or 64 hex characters")
}

// readKeyFile reads a small regular file without blocking on a FIFO (see
// openKeyFile) and without reading more than maxKeyFileSize+1 bytes.
func readKeyFile(path string, follow bool) ([]byte, error) {
	f, err := openKeyFile(path, follow)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", fi.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxKeyFileSize {
		return nil, fmt.Errorf("key file is larger than %d bytes", maxKeyFileSize)
	}
	return b, nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its purpose/record
// (e.g. "picache/storage/<id>/password"); Open must be given the same aad.
func (b *Box) Seal(plaintext []byte, aad string) (string, error) {
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	out := aead.Seal(nonce, nonce, plaintext, []byte(aad))
	return prefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// Open decrypts a value produced by Seal.
func (b *Box) Open(sealed, aad string) ([]byte, error) {
	if !strings.HasPrefix(sealed, prefix) {
		return nil, errors.New("secrets: unknown format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(sealed[len(prefix):])
	if err != nil {
		return nil, errors.New("secrets: corrupt value")
	}
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("secrets: corrupt value")
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(aad))
	if err != nil {
		return nil, errors.New("secrets: cannot decrypt (wrong key or tampered value)")
	}
	return pt, nil
}

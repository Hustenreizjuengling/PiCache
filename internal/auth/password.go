package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// argon2id parameters (OWASP minimum: m=19 MiB, t=2, p=1).
const (
	argonMemory  = 19456 // KiB
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16

	maxConcurrentHashes = 2

	minPasswordRunes = 10
	maxPasswordBytes = 1024
	maxUsernameLen   = 64
)

var phcB64 = base64.RawStdEncoding

// argonParams are the cost parameters of a stored hash.
type argonParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

var currentParams = argonParams{memory: argonMemory, time: argonTime, threads: argonThreads}

// hashPassword returns the PHC string of an argon2id hash with a random salt.
func hashPassword(pw string) string {
	salt := make([]byte, argonSaltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, phcB64.EncodeToString(salt), phcB64.EncodeToString(key))
}

// verifyPassword checks pw against a PHC string. rehash reports that the
// hash uses outdated parameters and should be replaced.
func verifyPassword(encoded, pw string) (ok, rehash bool, err error) {
	p, salt, key, err := parsePHC(encoded)
	if err != nil {
		return false, false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.time, p.memory, p.threads, uint32(len(key)))
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != currentParams || len(salt) != argonSaltLen || len(key) != argonKeyLen, nil
}

// parsePHC parses "$argon2id$v=19$m=…,t=…,p=…$salt$hash" with bounded
// parameters (a restored backup must not be able to make a login allocate
// gigabytes).
func parsePHC(s string) (argonParams, []byte, []byte, error) {
	var p argonParams
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, nil, nil, errors.New("auth: unsupported password hash format")
	}
	seen := 0
	for kv := range strings.SplitSeq(parts[3], ",") {
		k, v, _ := strings.Cut(kv, "=")
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return p, nil, nil, errors.New("auth: invalid password hash parameters")
		}
		switch k {
		case "m":
			p.memory = uint32(n)
		case "t":
			p.time = uint32(n)
		case "p":
			if n > 255 {
				return p, nil, nil, errors.New("auth: invalid password hash parameters")
			}
			p.threads = uint8(n)
		default:
			return p, nil, nil, errors.New("auth: invalid password hash parameters")
		}
		seen++
	}
	if seen != 3 || p.threads < 1 || p.threads > 16 || p.time < 1 || p.time > 16 ||
		p.memory < 8*uint32(p.threads) || p.memory > 1<<20 {
		return p, nil, nil, errors.New("auth: password hash parameters out of range")
	}
	salt, err1 := phcB64.DecodeString(parts[4])
	key, err2 := phcB64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) < 8 || len(salt) > 64 || len(key) < 16 || len(key) > 64 {
		return p, nil, nil, errors.New("auth: corrupt password hash")
	}
	return p, salt, key, nil
}

// dummyHash is verified for unknown users so that a login takes the same
// time whether or not the username exists.
var dummyHash = sync.OnceValue(func() string { return hashPassword(rand.Text()) })

// acquireHash waits for a hash slot (at most maxConcurrentHashes argon2id
// computations run at once: each needs 19 MiB).
func (a *Service) acquireHash(ctx context.Context) error {
	select {
	case a.hashSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Service) releaseHash() { <-a.hashSem }

// checkPassword verifies pw against encoded under the hash semaphore. If the
// password is right but the hash is outdated, newHash is a fresh hash.
func (a *Service) checkPassword(ctx context.Context, encoded, pw string) (ok bool, newHash string, err error) {
	if err := a.acquireHash(ctx); err != nil {
		return false, "", err
	}
	defer a.releaseHash()
	ok, rehash, err := verifyPassword(encoded, pw)
	if err != nil || !ok {
		return false, "", err
	}
	if rehash {
		newHash = hashPassword(pw)
	}
	return true, newHash, nil
}

// newHash hashes pw under the hash semaphore.
func (a *Service) newHash(ctx context.Context, pw string) (string, error) {
	if err := a.acquireHash(ctx); err != nil {
		return "", err
	}
	defer a.releaseHash()
	return hashPassword(pw), nil
}

// validatePassword enforces the password policy for new passwords.
func validatePassword(field, pw string) error {
	switch {
	case !utf8.ValidString(pw):
		return apperr.Invalid(field, "must be valid UTF-8")
	case utf8.RuneCountInString(pw) < minPasswordRunes:
		return apperr.Invalid(field, "must be at least %d characters", minPasswordRunes)
	case len(pw) > maxPasswordBytes:
		return apperr.Invalid(field, "must be at most %d bytes", maxPasswordBytes)
	case strings.ContainsRune(pw, 0):
		return apperr.Invalid(field, "must not contain NUL characters")
	}
	return nil
}

// validateUsername allows 1–64 characters [A-Za-z0-9._@-], starting with a
// letter or digit.
func validateUsername(u string) error {
	if u == "" || len(u) > maxUsernameLen {
		return apperr.Invalid("username", "must be 1 to %d characters", maxUsernameLen)
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '@' && c != '-')) {
			return apperr.Invalid("username", "may only contain letters, digits and . _ @ - (starting with a letter or digit)")
		}
	}
	return nil
}

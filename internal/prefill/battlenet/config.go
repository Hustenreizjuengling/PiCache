package battlenet

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Config is a build or CDN config: "key = value value …" lines.
type Config map[string][]string

// ParseConfig reads a config; kind is its first line ("# Build
// Configuration", "# CDN Configuration"). A config that is not text is
// encrypted (Sea of Thieves, The Witcher 3).
func ParseConfig(b []byte, kind string) (Config, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(b, "\xef\xbb\xbf"), []byte("# "+kind)) {
		return nil, fmt.Errorf("%w: the %s config is not readable", ErrEncrypted, strings.ToLower(kind))
	}
	c := Config{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		c[strings.TrimSpace(k)] = strings.Fields(v)
	}
	return c, nil
}

// Key returns the i-th value of name as a key.
func (c Config) Key(name string, i int) (Key, bool) {
	v := c[name]
	if i >= len(v) {
		return Key{}, false
	}
	k, err := ParseKey(v[i])
	return k, err == nil
}

// Int returns the i-th value of name as a number.
func (c Config) Int(name string, i int) (int64, bool) {
	v := c[name]
	if i >= len(v) {
		return 0, false
	}
	n, err := strconv.ParseInt(v[i], 10, 64)
	return n, err == nil && n >= 0
}

// Keys returns every value of name that is a key.
func (c Config) Keys(name string) []Key {
	var out []Key
	for _, s := range c[name] {
		if k, err := ParseKey(s); err == nil {
			out = append(out, k)
		}
	}
	return out
}

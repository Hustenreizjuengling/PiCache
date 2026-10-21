package steam

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Steam describes apps and packages in KeyValues: app info as text ("key"
// "value", "key" { … }), package info in the binary form. Both come from
// Steam's servers and are parsed with bounds: depth, node count and string
// length.

const (
	kvMaxDepth  = 32
	kvMaxNodes  = 1 << 20
	kvMaxString = 1 << 20
)

var errKV = errors.New("steam: malformed KeyValues")

// kv is one KeyValues node: a leaf with a value or a section with
// children. Binary numbers are kept as their decimal text.
type kv struct {
	key      string
	value    string
	children []*kv
}

// child returns the first child named key (case-insensitive, like Steam),
// or nil.
func (k *kv) child(key string) *kv {
	if k == nil {
		return nil
	}
	for _, c := range k.children {
		if strings.EqualFold(c.key, key) {
			return c
		}
	}
	return nil
}

// get follows a path of child names.
func (k *kv) get(path ...string) *kv {
	for _, p := range path {
		k = k.child(p)
	}
	return k
}

// str is the value at path, "" when it is missing.
func (k *kv) str(path ...string) string {
	if n := k.get(path...); n != nil {
		return n.value
	}
	return ""
}

// uint is the unsigned number at path, 0 when it is missing or not one.
func (k *kv) uint(path ...string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(k.str(path...)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseTextKV parses text KeyValues. A NUL ends the input (PICS app info
// carries one at its end).
func parseTextKV(b []byte) (*kv, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	p := &kvText{b: bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))}
	root := &kv{}
	if err := p.section(root, 0, false); err != nil {
		return nil, err
	}
	return root, nil
}

type kvText struct {
	b     []byte
	nodes int
}

// token returns the next token: a string (quoted or not) or one of the
// braces (brace set). ok is false at the end of the input.
func (p *kvText) token() (tok string, brace byte, ok bool, err error) {
	for {
		for len(p.b) > 0 && (p.b[0] == ' ' || p.b[0] == '\t' || p.b[0] == '\r' || p.b[0] == '\n') {
			p.b = p.b[1:]
		}
		if len(p.b) >= 2 && p.b[0] == '/' && p.b[1] == '/' {
			if i := bytes.IndexByte(p.b, '\n'); i >= 0 {
				p.b = p.b[i+1:]
			} else {
				p.b = nil
			}
			continue
		}
		break
	}
	if len(p.b) == 0 {
		return "", 0, false, nil
	}
	switch c := p.b[0]; c {
	case '{', '}':
		p.b = p.b[1:]
		return "", c, true, nil
	case '"':
		var sb strings.Builder
		i := 1
		for ; i < len(p.b) && p.b[i] != '"'; i++ {
			c := p.b[i]
			if c == '\\' && i+1 < len(p.b) {
				i++
				switch p.b[i] {
				case 'n':
					c = '\n'
				case 't':
					c = '\t'
				case '\\', '"':
					c = p.b[i]
				default:
					sb.WriteByte('\\')
					c = p.b[i]
				}
			}
			if sb.Len() >= kvMaxString {
				return "", 0, false, errKV
			}
			sb.WriteByte(c)
		}
		if i >= len(p.b) {
			return "", 0, false, errKV
		}
		p.b = p.b[i+1:]
		return sb.String(), 0, true, nil
	default:
		i := 0
		for i < len(p.b) && !strings.ContainsRune(" \t\r\n{}\"", rune(p.b[i])) {
			i++
		}
		if i > kvMaxString {
			return "", 0, false, errKV
		}
		tok = string(p.b[:i])
		p.b = p.b[i:]
		return tok, 0, true, nil
	}
}

// section reads key/value pairs into parent until "}" (inner) or the end
// of the input (the root).
func (p *kvText) section(parent *kv, depth int, inner bool) error {
	if depth > kvMaxDepth {
		return errKV
	}
	for {
		key, brace, ok, err := p.token()
		if err != nil {
			return err
		}
		if !ok {
			if inner {
				return errKV
			}
			return nil
		}
		if brace == '}' {
			if !inner {
				return errKV
			}
			return nil
		}
		if brace == '{' {
			return errKV
		}
		if p.nodes++; p.nodes > kvMaxNodes {
			return errKV
		}
		n := &kv{key: key}
		parent.children = append(parent.children, n)
		val, brace, ok, err := p.token()
		if err != nil {
			return err
		}
		if !ok {
			return errKV
		}
		switch brace {
		case '{':
			if err := p.section(n, depth+1, true); err != nil {
				return err
			}
		case '}':
			return errKV
		default:
			n.value = val
		}
		p.skipCondition()
	}
}

// skipCondition drops a platform condition such as [$WIN32] after a value.
func (p *kvText) skipCondition() {
	b := bytes.TrimLeft(p.b, " \t")
	if len(b) > 0 && b[0] == '[' {
		if i := bytes.IndexByte(b, ']'); i >= 0 {
			p.b = b[i+1:]
		}
	}
}

// Binary KeyValues types.
const (
	kvbSection = 0x00
	kvbString  = 0x01
	kvbInt32   = 0x02
	kvbFloat32 = 0x03
	kvbPointer = 0x04
	kvbWString = 0x05
	kvbColor   = 0x06
	kvbUint64  = 0x07
	kvbEnd     = 0x08
	kvbInt64   = 0x0A
	kvbEndAlt  = 0x0B
)

// parseBinaryKV parses binary KeyValues: entries up to an end marker or
// the end of the input form the root's children.
func parseBinaryKV(b []byte) (*kv, error) {
	p := &kvBinary{b: b}
	root := &kv{}
	if err := p.section(root, 0); err != nil {
		return nil, err
	}
	return root, nil
}

type kvBinary struct {
	b     []byte
	nodes int
}

func (p *kvBinary) cstring() (string, error) {
	i := bytes.IndexByte(p.b, 0)
	if i < 0 || i > kvMaxString {
		return "", errKV
	}
	s := string(p.b[:i])
	p.b = p.b[i+1:]
	return s, nil
}

func (p *kvBinary) fixed(n int) ([]byte, error) {
	if len(p.b) < n {
		return nil, errKV
	}
	v := p.b[:n]
	p.b = p.b[n:]
	return v, nil
}

func (p *kvBinary) section(parent *kv, depth int) error {
	if depth > kvMaxDepth {
		return errKV
	}
	for len(p.b) > 0 {
		t := p.b[0]
		p.b = p.b[1:]
		if t == kvbEnd || t == kvbEndAlt {
			return nil
		}
		if p.nodes++; p.nodes > kvMaxNodes {
			return errKV
		}
		key, err := p.cstring()
		if err != nil {
			return err
		}
		n := &kv{key: key}
		parent.children = append(parent.children, n)
		switch t {
		case kvbSection:
			if err := p.section(n, depth+1); err != nil {
				return err
			}
		case kvbString:
			if n.value, err = p.cstring(); err != nil {
				return err
			}
		case kvbInt32, kvbPointer, kvbColor:
			v, err := p.fixed(4)
			if err != nil {
				return err
			}
			if t == kvbInt32 {
				n.value = strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(v))), 10)
			} else {
				n.value = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(v)), 10)
			}
		case kvbFloat32:
			v, err := p.fixed(4)
			if err != nil {
				return err
			}
			n.value = strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(v))), 'g', -1, 32)
		case kvbUint64:
			v, err := p.fixed(8)
			if err != nil {
				return err
			}
			n.value = strconv.FormatUint(binary.LittleEndian.Uint64(v), 10)
		case kvbInt64:
			v, err := p.fixed(8)
			if err != nil {
				return err
			}
			n.value = strconv.FormatInt(int64(binary.LittleEndian.Uint64(v)), 10)
		case kvbWString:
			var u []uint16
			for {
				v, err := p.fixed(2)
				if err != nil {
					return err
				}
				c := binary.LittleEndian.Uint16(v)
				if c == 0 {
					break
				}
				if len(u) >= kvMaxString/2 {
					return errKV
				}
				u = append(u, c)
			}
			n.value = string(utf16.Decode(u))
		default:
			return errKV
		}
	}
	return nil
}

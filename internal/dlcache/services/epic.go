package services

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Epic manifests (the .manifest of a build, cached like its chunks) name
// the build's app and executable. Two formats: binary (magic 0x44BEC00C, a
// fixed header, then the body, zlib-compressed when bit 0 of "stored as" is
// set, starting with the manifest meta: size, data version, feature level,
// is-file-data, app ID, then the strings AppName, BuildVersion, LaunchExe,
// …) and JSON (older builds: "AppNameString", "LaunchExeString"). Only the
// start is read; the input is untrusted and may be cut off anywhere after
// the strings needed.

const (
	epicManifestMagic = 0x44BEC00C
	epicMaxString     = 1024 // characters of an FString
	epicMaxMeta       = 64 << 10
)

var (
	errEpicManifest = errors.New("not an Epic manifest")

	epicJSONAppRE = regexp.MustCompile(`"AppNameString"\s*:\s*"([^"\\]{0,256})"`)
	epicJSONExeRE = regexp.MustCompile(`"LaunchExeString"\s*:\s*"([^"]{0,512})"`)
	// epicExeSuffixRE: build suffixes of Unreal executables
	// ("FortniteClient-Win64-Shipping", "Control_DX12").
	epicExeSuffixRE = regexp.MustCompile(`(?i)(?:[-_](?:win(?:64|32|gdk)|shipping|test|dx1[12]|vulkan|launcher|eac|be|x64))+$`)
	epicHexRE       = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
	epicNumberRE    = regexp.MustCompile(`([a-z])(\d+)$`) // "AlanWake2" → "AlanWake 2"
)

// parseEpicManifest returns the AppName and LaunchExe of a manifest.
func parseEpicManifest(b []byte) (app, exe string, err error) {
	if t := bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n"); len(t) > 0 && t[0] == '{' {
		a, e := epicJSONAppRE.FindSubmatch(t), epicJSONExeRE.FindSubmatch(t)
		if a == nil && e == nil {
			return "", "", errEpicManifest
		}
		if a != nil {
			app = string(a[1])
		}
		if e != nil {
			exe = strings.ReplaceAll(string(e[1]), `\\`, `/`)
		}
		return app, exe, nil
	}
	const fixed = 4 + 4 + 4 + 4 + 20 + 1 + 4 // magic … version
	if len(b) < fixed || binary.LittleEndian.Uint32(b) != epicManifestMagic {
		return "", "", errEpicManifest
	}
	headerSize := binary.LittleEndian.Uint32(b[4:])
	storedAs := b[36]
	if headerSize < fixed || int64(headerSize) > int64(len(b)) {
		return "", "", fmt.Errorf("%w: header size %d", errEpicManifest, headerSize)
	}
	var body io.Reader = bytes.NewReader(b[headerSize:])
	if storedAs&1 != 0 {
		z, err := zlib.NewReader(body)
		if err != nil {
			return "", "", fmt.Errorf("%w: %v", errEpicManifest, err)
		}
		defer z.Close()
		body = z
	}
	r := &epicReader{r: io.LimitReader(body, epicMaxMeta)}
	r.skip(4 + 1 + 4 + 1 + 4) // meta size, data version, feature level, is file data, app ID
	app = r.fstring()
	r.fstring() // build version
	exe = r.fstring()
	if r.err != nil {
		return "", "", fmt.Errorf("%w: %v", errEpicManifest, r.err)
	}
	return app, exe, nil
}

// epicReader reads the little-endian fields of a manifest body; the first
// error sticks.
type epicReader struct {
	r   io.Reader
	err error
}

func (r *epicReader) skip(n int64) {
	if r.err == nil {
		_, r.err = io.CopyN(io.Discard, r.r, n)
	}
}

// fstring reads an Unreal FString: an int32 length with the terminating
// NUL, negative for UTF-16.
func (r *epicReader) fstring() string {
	if r.err != nil {
		return ""
	}
	var n int32
	if r.err = binary.Read(r.r, binary.LittleEndian, &n); r.err != nil {
		return ""
	}
	switch {
	case n == 0:
		return ""
	case n > epicMaxString || n < -epicMaxString:
		r.err = fmt.Errorf("string of %d characters", n)
		return ""
	case n > 0:
		b := make([]byte, n)
		if _, r.err = io.ReadFull(r.r, b); r.err != nil {
			return ""
		}
		return strings.ToValidUTF8(string(bytes.TrimRight(b, "\x00")), "")
	}
	u := make([]uint16, -n)
	if r.err = binary.Read(r.r, binary.LittleEndian, u); r.err != nil {
		return ""
	}
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	return string(utf16.Decode(u))
}

// epicName derives a display name from a manifest's executable ("Binaries/
// Win64/RocketLeague.exe" → "Rocket League"), else its AppName unless that
// is an opaque ID.
func epicName(app, exe string) string {
	exe = exe[strings.LastIndexAny(exe, `/\`)+1:]
	if i := strings.LastIndexByte(exe, '.'); i > 0 {
		exe = exe[:i]
	}
	exe = epicExeSuffixRE.ReplaceAllString(exe, "")
	if exe != "" && utf8.ValidString(exe) {
		return humanise(epicNumberRE.ReplaceAllString(exe, "$1 $2"))
	}
	if app != "" && !epicHexRE.MatchString(app) {
		return humanise(app)
	}
	return ""
}

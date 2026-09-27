//go:build ignore

// Command gen writes internal/oui/oui.bin from the CSV exports of the IEEE
// registries MA-L (oui.csv), MA-M (mam.csv) and MA-S (oui36.csv) in the
// directory given as its argument (scripts/oui-update.sh downloads them):
//
//	go run internal/oui/gen/main.go <dir>
//
// Vendor names are cleaned like other untrusted names (invalid UTF-8 and
// the Unicode categories Cc and Cf removed, white space collapsed, at most
// 100 characters); rows with an invalid assignment or an empty name are
// skipped, a repeated assignment keeps its first row. The layout constants
// are those of package oui.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/oui"
)

type record struct {
	key  uint64
	name string
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run internal/oui/gen/main.go <dir with oui.csv, mam.csv, oui36.csv>")
		os.Exit(2)
	}
	dir := os.Args[1]
	var regs [3][]record
	for i, f := range []struct {
		file   string
		prefix string
		digits int
	}{{"oui.csv", "MA-L", oui.BitsL / 4}, {"mam.csv", "MA-M", oui.BitsM / 4}, {"oui36.csv", "MA-S", oui.BitsS / 4}} {
		recs, err := readCSV(filepath.Join(dir, f.file), f.prefix, f.digits)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gen:", err)
			os.Exit(1)
		}
		if len(recs) == 0 {
			fmt.Fprintf(os.Stderr, "gen: %s has no records\n", f.file)
			os.Exit(1)
		}
		regs[i] = recs
	}
	out, err := build(regs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if len(out) > oui.MaxSize {
		fmt.Fprintf(os.Stderr, "gen: oui.bin would be %d bytes, more than %d\n", len(out), oui.MaxSize)
		os.Exit(1)
	}
	dst := filepath.Join("internal", "oui", "oui.bin")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, dst); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s: %d MA-L, %d MA-M, %d MA-S records, %d bytes\n", dst, len(regs[0]), len(regs[1]), len(regs[2]), len(out))
}

// readCSV reads one registry export: Registry,Assignment,Organization
// Name,Organization Address.
func readCSV(path, registry string, digits int) ([]record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(io.LimitReader(f, 64<<20))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	seen := map[uint64]bool{}
	var out []record
	for line := 0; ; line++ {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if line == 0 || len(row) < 3 || strings.TrimSpace(row[0]) != registry {
			continue
		}
		a := strings.TrimSpace(row[1])
		if len(a) != digits {
			continue
		}
		key, err := strconv.ParseUint(a, 16, 64)
		if err != nil || seen[key] {
			continue
		}
		name := cleanName(row[2])
		if name == "" {
			continue
		}
		seen[key] = true
		out = append(out, record{key: key, name: name})
	}
	slices.SortFunc(out, func(a, b record) int { return compare(a.key, b.key) })
	return out, nil
}

func compare(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cleanName cleans an untrusted name: invalid UTF-8 and every character of
// the Unicode categories Cc and Cf removed, white space collapsed, trimmed,
// cut to oui.MaxNameLen characters.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > oui.MaxNameLen {
		s = strings.TrimSpace(string([]rune(s)[:oui.MaxNameLen]))
	}
	return s
}

// build encodes the registries (MA-L, MA-M, MA-S) and the de-duplicated
// name table.
func build(regs [3][]record) ([]byte, error) {
	var names bytes.Buffer
	offsets := map[string]int{}
	offsetOf := func(name string) (int, error) {
		if off, ok := offsets[name]; ok {
			return off, nil
		}
		off := names.Len()
		if off >= 1<<(8*oui.OffsetSize) {
			return 0, errors.New("the name table is too large")
		}
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(name)))
		names.Write(l[:])
		names.WriteString(name)
		offsets[name] = off
		return off, nil
	}
	var body bytes.Buffer
	for i, keySize := range []int{oui.KeyL, oui.KeyM, oui.KeyS} {
		for _, rec := range regs[i] {
			off, err := offsetOf(rec.name)
			if err != nil {
				return nil, err
			}
			for j := keySize - 1; j >= 0; j-- {
				body.WriteByte(byte(rec.key >> (8 * j)))
			}
			for j := oui.OffsetSize - 1; j >= 0; j-- {
				body.WriteByte(byte(off >> (8 * j)))
			}
		}
	}
	out := make([]byte, 0, oui.HeaderSize+body.Len()+names.Len())
	out = append(out, oui.Magic...)
	for _, n := range []int{len(regs[0]), len(regs[1]), len(regs[2]), names.Len()} {
		out = binary.BigEndian.AppendUint32(out, uint32(n))
	}
	out = append(out, body.Bytes()...)
	out = append(out, names.Bytes()...)
	return out, nil
}

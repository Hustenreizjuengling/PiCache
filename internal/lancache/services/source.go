package services

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Ingestion limits for the untrusted cache-domains source (ARCHITECTURE 8.1)
// plus the bounds this package adds on top.
const (
	maxIndexBytes      = 1 << 20 // cache_domains.json
	maxIndexServices   = 128
	maxFileBytes       = 4 << 20 // each domain file
	maxFileLines       = 50_000
	maxFilesPerService = 16
	maxSourcePatterns  = 100_000 // all services together
	maxSkipped         = 100     // reported rejections (the rest is counted)
	maxSnapshotBytes   = 32 << 20
	maxTextLen         = 1024 // description / notes

	snapshotFile   = "snapshot.json"
	snapshotTemp   = "snapshot.json.tmp"
	snapshotFormat = 1
)

// domainFileRE is the allowed form of domain_files entries.
var domainFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.txt$`)

// sourceService is one service of the cache-domains source (also the
// snapshot format).
type sourceService struct {
	ID           string   `json:"id"`
	Description  string   `json:"description,omitempty"`
	Notes        string   `json:"notes,omitempty"`
	MixedContent bool     `json:"mixedContent,omitempty"`
	Domains      []string `json:"domains"`
}

// sourceSnapshot is the parsed source as stored in <dir>/snapshot.json.
type sourceSnapshot struct {
	Format    int             `json:"format"`
	Source    string          `json:"source"`
	FetchedAt time.Time       `json:"fetchedAt"`
	Services  []sourceService `json:"services"`
	Skipped   []string        `json:"skipped,omitempty"`
}

// domainCount returns the number of source patterns.
func (s *sourceSnapshot) domainCount() int {
	n := 0
	for _, sv := range s.Services {
		n += len(sv.Domains)
	}
	return n
}

// indexDoc is cache_domains.json.
type indexDoc struct {
	CacheDomains []indexEntry `json:"cache_domains"`
}

type indexEntry struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	DomainFiles  []string `json:"domain_files"`
	Notes        string   `json:"notes"`
	MixedContent bool     `json:"mixed_content"`
}

// skipList collects rejected entries; it keeps the first maxSkipped and
// counts the rest.
type skipList struct {
	items []string
	more  int
}

func (s *skipList) add(format string, args ...any) {
	if len(s.items) < maxSkipped {
		s.items = append(s.items, fmt.Sprintf(format, args...))
		return
	}
	s.more++
}

func (s *skipList) list() []string {
	out := slices.Clone(s.items)
	if s.more > 0 {
		out = append(out, fmt.Sprintf("… and %d more", s.more))
	}
	return out
}

// statusError is an unexpected HTTP status of a source download.
type statusError struct {
	url  string // redacted
	code int
}

func (e *statusError) Error() string {
	if e.code >= 300 && e.code < 400 {
		return fmt.Sprintf("GET %s: HTTP %d (redirects are not followed; point the source at the final URL)", e.url, e.code)
	}
	return fmt.Sprintf("GET %s: HTTP %d", e.url, e.code)
}

// fetchSource downloads and validates cache_domains.json and every domain
// file it references.
func fetchSource(ctx context.Context, client *http.Client, base string) (*sourceSnapshot, error) {
	if client == nil {
		return nil, errors.New("no HTTP client configured")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the cache-domains source must be an https URL")
	}
	indexURL, err := url.JoinPath(base, "cache_domains.json")
	if err != nil {
		return nil, fmt.Errorf("invalid source URL: %w", err)
	}
	body, err := download(ctx, client, indexURL, maxIndexBytes)
	if err != nil {
		return nil, err
	}
	var skipped skipList
	entries, err := parseIndex(body, &skipped)
	if err != nil {
		return nil, err
	}

	files := map[string][]string{} // domain file → patterns
	failed := map[string]bool{}
	snap := &sourceSnapshot{Format: snapshotFormat, Source: base}
	total := 0
	for _, e := range entries {
		var domains []string
		seen := map[string]bool{}
		for _, name := range e.DomainFiles {
			patterns, ok := files[name]
			if !ok && !failed[name] {
				patterns, err = fetchDomainFile(ctx, client, base, name, &skipped)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if !skippableFileError(err) {
						return nil, err // transient: keep the last good snapshot
					}
					skipped.add("%s: %v", name, err)
					failed[name] = true
					continue
				}
				files[name] = patterns
			}
			for _, p := range patterns {
				if seen[p] {
					continue
				}
				if total >= maxSourcePatterns {
					skipped.add("%s: more than %d patterns in total; the rest is ignored", e.Name, maxSourcePatterns)
					break
				}
				seen[p] = true
				domains = append(domains, p)
				total++
			}
		}
		if len(domains) == 0 {
			skipped.add("service %s: no valid domains", e.Name)
			continue
		}
		snap.Services = append(snap.Services, sourceService{
			ID:           e.Name,
			Description:  cleanText(e.Description, maxTextLen),
			Notes:        cleanText(e.Notes, maxTextLen),
			MixedContent: e.MixedContent,
			Domains:      domains,
		})
	}
	if len(snap.Services) == 0 {
		return nil, errors.New("the source contains no usable services")
	}
	snap.Skipped = skipped.list()
	return snap, nil
}

// contentError marks a domain file that was downloaded but rejected.
type contentError struct{ msg string }

func (e *contentError) Error() string { return e.msg }

// skippableFileError reports whether a domain file error concerns only that
// file (rejected content, 404/410) so the rest of the source can still be
// used. Other errors (network, 5xx) abort the refresh.
func skippableFileError(err error) bool {
	var ce *contentError
	if errors.As(err, &ce) {
		return true
	}
	var se *statusError
	return errors.As(err, &se) && (se.code == http.StatusNotFound || se.code == http.StatusGone)
}

func fetchDomainFile(ctx context.Context, client *http.Client, base, name string, skipped *skipList) ([]string, error) {
	fileURL, err := url.JoinPath(base, name)
	if err != nil {
		return nil, &contentError{"invalid file URL"}
	}
	body, err := download(ctx, client, fileURL, maxFileBytes)
	if err != nil {
		return nil, err
	}
	return parseDomainFile(name, body, skipped)
}

// download GETs rawURL without following redirects and reads at most limit
// bytes. Errors never contain the query string of the URL.
func download(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %s", redactURL(rawURL))
	}
	req.Header.Set("User-Agent", "PiCache")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("GET %s: %w", redactURL(rawURL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{url: redactURL(rawURL), code: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", redactURL(rawURL), err)
	}
	if int64(len(b)) > limit {
		return nil, &contentError{fmt.Sprintf("larger than %d bytes", limit)}
	}
	return b, nil
}

// redactURL drops credentials, query and fragment for logs and messages.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// parseIndex validates cache_domains.json. Invalid services and file names
// are skipped (reported in skipped); structural problems are errors.
func parseIndex(b []byte, skipped *skipList) ([]indexEntry, error) {
	if len(b) > maxIndexBytes {
		return nil, fmt.Errorf("cache_domains.json is larger than %d bytes", maxIndexBytes)
	}
	var doc indexDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("cache_domains.json: invalid JSON: %s", clip(err.Error(), 200))
	}
	if len(doc.CacheDomains) == 0 {
		return nil, errors.New("cache_domains.json lists no services")
	}
	if len(doc.CacheDomains) > maxIndexServices {
		return nil, fmt.Errorf("cache_domains.json lists %d services (at most %d are accepted)", len(doc.CacheDomains), maxIndexServices)
	}
	seen := map[string]bool{}
	var out []indexEntry
	for i, e := range doc.CacheDomains {
		switch {
		case !ValidServiceID(e.Name):
			skipped.add("service #%d: invalid id %q", i+1, clip(e.Name, 64))
			continue
		case strings.HasPrefix(e.Name, customPrefix):
			skipped.add("service %s: the prefix %q is reserved for custom services", e.Name, customPrefix)
			continue
		case seen[e.Name]:
			skipped.add("service %s: duplicate id", e.Name)
			continue
		}
		seen[e.Name] = true
		var files []string
		for _, f := range e.DomainFiles {
			switch {
			case !domainFileRE.MatchString(f):
				skipped.add("service %s: invalid file name %q", e.Name, clip(f, 80))
			case slices.Contains(files, f):
			case len(files) >= maxFilesPerService:
				skipped.add("service %s: more than %d domain files", e.Name, maxFilesPerService)
			default:
				files = append(files, f)
			}
		}
		if len(files) == 0 {
			skipped.add("service %s: no valid domain files", e.Name)
			continue
		}
		e.DomainFiles = files
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("cache_domains.json contains no valid services")
	}
	return out, nil
}

// parseDomainFile parses one domain file: one pattern per line, "#"
// comments, CRLF accepted, trailing dots stripped. Invalid patterns are
// skipped (reported), too many lines reject the whole file.
func parseDomainFile(name string, b []byte, skipped *skipList) ([]string, error) {
	if len(b) > maxFileBytes {
		return nil, &contentError{fmt.Sprintf("larger than %d bytes", maxFileBytes)}
	}
	var out []string
	seen := map[string]bool{}
	lines := 0
	for line := range strings.Lines(string(b)) {
		lines++
		if lines > maxFileLines {
			return nil, &contentError{fmt.Sprintf("more than %d lines", maxFileLines)}
		}
		p := NormalizePattern(line)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if err := ValidatePattern(p); err != nil {
			skipped.add("%s: %q: %v", name, clip(p, 80), err)
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// cleanText makes untrusted display text safe and bounded: valid UTF-8, no
// control characters, trimmed, at most n bytes.
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// saveSnapshot writes the snapshot atomically (temp + rename) through
// os.Root so it can never escape dir.
func saveSnapshot(dir string, s *sourceSnapshot) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(snapshotTemp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(snapshotTemp, snapshotFile)
	}
	if err != nil {
		_ = root.Remove(snapshotTemp)
	}
	return err
}

// loadSnapshot reads the snapshot (nil, nil if there is none). The content
// is validated again: it lives on disk and could have been edited.
func loadSnapshot(dir string) (*sourceSnapshot, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(snapshotFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSnapshotBytes {
		return nil, errors.New("snapshot is too large")
	}
	var s sourceSnapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	if s.Format != snapshotFormat {
		return nil, fmt.Errorf("snapshot has unknown format %d", s.Format)
	}
	var skipped skipList
	seen := map[string]bool{}
	total := 0
	valid := s.Services[:0]
	for _, sv := range s.Services {
		if !ValidServiceID(sv.ID) || strings.HasPrefix(sv.ID, customPrefix) || seen[sv.ID] || len(valid) >= maxIndexServices {
			skipped.add("snapshot: invalid service %q", clip(sv.ID, 64))
			continue
		}
		seen[sv.ID] = true
		domains := sv.Domains[:0]
		dup := map[string]bool{}
		for _, p := range sv.Domains {
			if dup[p] {
				continue
			}
			if p != NormalizePattern(p) || ValidatePattern(p) != nil || total >= maxSourcePatterns {
				skipped.add("snapshot: invalid pattern %q", clip(p, 80))
				continue
			}
			dup[p] = true
			domains = append(domains, p)
			total++
		}
		sv.Domains = domains
		sv.Description = cleanText(sv.Description, maxTextLen)
		sv.Notes = cleanText(sv.Notes, maxTextLen)
		valid = append(valid, sv)
	}
	if len(valid) == 0 {
		return nil, errors.New("snapshot contains no valid services")
	}
	s.Services = valid
	if len(s.Skipped) > maxSkipped+1 {
		s.Skipped = s.Skipped[:maxSkipped+1]
	}
	for i := range s.Skipped {
		s.Skipped[i] = cleanText(s.Skipped[i], 512)
	}
	s.Skipped = append(s.Skipped, skipped.list()...)
	return &s, nil
}

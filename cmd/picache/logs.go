package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const logsUsage = `usage: picache logs tail [--client ADDR]... [--status S]... [--json] [--url URL] [--token-file FILE]
       picache logs export --format ndjson|csv [--range R | --from T --to T] [--client ADDR]... [--status S]...
                           [--domain D] [--qtype T] [--rcode R]... [--dnssec true|false] [--upstream U]
                           --out FILE|- [--url URL] [--token-file FILE]`

// Reconnect backoff of logs tail.
const (
	tailBackoffMin = time.Second
	tailBackoffMax = 30 * time.Second
)

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func logsCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, logsUsage)
		return 2
	}
	switch args[0] {
	case "tail":
		return logsTail(args[1:])
	case "export":
		return logsExport(args[1:])
	}
	fmt.Fprintln(os.Stderr, logsUsage)
	return 2
}

// newFlags returns a flag set that reports errors with the logs usage.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func usageError(err error) int {
	fmt.Fprintf(os.Stderr, "picache: %v\n%s\n", err, logsUsage)
	return 2
}

// clientFor builds the API client or reports why not (exit code).
func clientFor(flagURL, tokenFile string) (*apiClient, int) {
	c, usage, err := newAPIClient(flagURL, tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache:", err)
		if usage {
			return nil, 2
		}
		return nil, 1
	}
	return c, 0
}

func logsTail(args []string) int {
	fs := newFlags("logs tail")
	var clients, statuses multiFlag
	fs.Var(&clients, "client", "client address or name (repeatable)")
	fs.Var(&statuses, "status", "status or class (repeatable)")
	asJSON := fs.Bool("json", false, "print the events as NDJSON")
	flagURL := fs.String("url", "", "PiCache URL")
	tokenFile := fs.String("token-file", "", "file with the API token")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	c, code := clientFor(*flagURL, *tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	q := neturl.Values{}
	for _, v := range clients {
		q.Add("client", v)
	}
	for _, v := range statuses {
		q.Add("status", v)
	}
	return runTail(ctx, c, q, *asJSON, os.Stdout, sleepCtx)
}

// sleepCtx waits d; false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// runTail follows the query stream until ctx ends (exit 0). The stream is
// opened again with a backoff (1 s, doubling to 30 s) after it ended, on
// 429, 5xx and network errors; 401 and 403 end it with the server's
// message (exit 1), as do other client errors.
func runTail(ctx context.Context, c *apiClient, q neturl.Values, asJSON bool, out io.Writer, sleep func(context.Context, time.Duration) bool) int {
	backoff := tailBackoffMin
	for {
		resp, err := c.get(ctx, "/api/v1/stream/queries", q, "text/event-stream")
		switch {
		case ctx.Err() != nil:
			return 0
		case err != nil:
			fmt.Fprintln(os.Stderr, "picache: stream:", escapeControls(err.Error()))
		case resp.StatusCode == http.StatusOK:
			backoff = tailBackoffMin
			err := readStream(resp.Body, asJSON, out)
			resp.Body.Close()
			if ctx.Err() != nil {
				return 0
			}
			if err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintln(os.Stderr, "picache: stream:", escapeControls(err.Error()))
			}
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			err := apiError(resp)
			resp.Body.Close()
			fmt.Fprintln(os.Stderr, "picache:", err)
		default: // 401, 403 and other client errors: retrying cannot help
			err := apiError(resp)
			resp.Body.Close()
			fmt.Fprintln(os.Stderr, "picache:", err)
			return 1
		}
		if !sleep(ctx, backoff) {
			return 0
		}
		backoff = min(backoff*2, tailBackoffMax)
	}
}

// readStream reads Server-Sent Events of kind "query" and prints them.
func readStream(r io.Reader, asJSON bool, out io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var data []byte
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			if len(data) > 0 {
				printQuery(out, data, asJSON)
				data = data[:0]
			}
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// tailEvent are the members of a query event that tail prints.
type tailEvent struct {
	Time       time.Time `json:"time"`
	ClientIP   string    `json:"clientIp"`
	ClientName string    `json:"clientName"`
	QName      string    `json:"qname"`
	QType      string    `json:"qtype"`
	Status     string    `json:"status"`
	RCode      string    `json:"rcode"`
	DurationUs int64     `json:"durationUs"`
}

// printQuery prints one event: the data as received (--json) or one line
// (local time, client, qtype, qname, status, rcode, duration) with control
// and bidi characters escaped.
func printQuery(out io.Writer, data []byte, asJSON bool) {
	if asJSON {
		fmt.Fprintf(out, "%s\n", escapeControls(string(data)))
		return
	}
	var e tailEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return
	}
	client := e.ClientIP
	if e.ClientName != "" {
		client += " (" + e.ClientName + ")"
	}
	fmt.Fprintln(out, escapeControls(fmt.Sprintf("%s  %s  %s  %s  %s  %s  %s", e.Time.Local().Format("2006-01-02 15:04:05"),
		client, e.QType, e.QName, e.Status, e.RCode, strconv.FormatFloat(float64(e.DurationUs)/1000, 'f', 1, 64)+" ms")))
}

// escapeControls escapes C0 and C1 controls, DEL and the bidi controls as
// \uXXXX (a terminal must not run what a client put in a query name).
func escapeControls(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func logsExport(args []string) int {
	fs := newFlags("logs export")
	var clients, statuses, rcodes multiFlag
	format := fs.String("format", "", "ndjson or csv")
	rng := fs.String("range", "", "range such as 1h, 24h, 7d")
	from := fs.String("from", "", "start (RFC 3339 or unix seconds)")
	to := fs.String("to", "", "end (RFC 3339 or unix seconds)")
	fs.Var(&clients, "client", "client address or name (repeatable)")
	fs.Var(&statuses, "status", "status or class (repeatable)")
	domain := fs.String("domain", "", "domain substring, or \"exact\"")
	qtype := fs.String("qtype", "", "query type")
	fs.Var(&rcodes, "rcode", "response code (repeatable)")
	dnssec := fs.String("dnssec", "", "true or false")
	upstream := fs.String("upstream", "", "upstream")
	outPath := fs.String("out", "", "output file, - for stdout")
	flagURL := fs.String("url", "", "PiCache URL")
	tokenFile := fs.String("token-file", "", "file with the API token")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	switch {
	case fs.NArg() != 0:
		return usageError(fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	case *format != "ndjson" && *format != "csv":
		return usageError(errors.New("--format must be ndjson or csv"))
	case *outPath == "":
		return usageError(errors.New("--out is required (a new file, or - for stdout)"))
	case *rng != "" && (*from != "" || *to != ""):
		return usageError(errors.New("use --range or --from/--to"))
	case *dnssec != "" && *dnssec != "true" && *dnssec != "false":
		return usageError(errors.New("--dnssec must be true or false"))
	}
	c, code := clientFor(*flagURL, *tokenFile)
	if c == nil {
		return code
	}
	q := neturl.Values{"format": {*format}}
	for k, v := range map[string]string{"range": *rng, "from": *from, "to": *to, "domain": *domain, "qtype": *qtype,
		"dnssec": *dnssec, "upstream": *upstream} {
		if v != "" {
			q.Set(k, v)
		}
	}
	for k, vs := range map[string]multiFlag{"client": clients, "status": statuses, "rcode": rcodes} {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	var out io.Writer = os.Stdout
	var file *os.File
	if *outPath != "-" {
		f, err := createExclusive(*outPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "picache:", err)
			return 1
		}
		file, out = f, f
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	marker, err := runExport(ctx, c, q, out)
	if file != nil {
		if cerr := file.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(file.Name()) // an incomplete export is not kept
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "picache:", err)
		return 1
	}
	if marker != "" {
		fmt.Fprintf(os.Stderr, "picache: the export was truncated (%s); narrow the range or the filters\n", marker)
	}
	return 0
}

// runExport copies the export to out and returns the reason of a
// truncation marker at its end ("rows", "time"; "" if none).
func runExport(ctx context.Context, c *apiClient, q neturl.Values, out io.Writer) (string, error) {
	resp, err := c.get(ctx, "/api/v1/logs/queries/export", q, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	tail := &tailBuffer{}
	if _, err := io.Copy(io.MultiWriter(out, tail), resp.Body); err != nil {
		return "", err
	}
	return truncationMarker(tail.last()), nil
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 256 {
		t.b = t.b[len(t.b)-256:]
	}
	return len(p), nil
}

// last returns the last line.
func (t *tailBuffer) last() string {
	s := strings.TrimRight(string(t.b), "\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// truncationMarker parses the marker line of an export ("" if the line is
// none): {"truncated":true,"reason":"rows"} or # truncated: time.
func truncationMarker(line string) string {
	if r, ok := strings.CutPrefix(line, "# truncated: "); ok {
		return r
	}
	var m struct {
		Truncated bool   `json:"truncated"`
		Reason    string `json:"reason"`
	}
	if strings.HasPrefix(line, `{"truncated"`) && json.Unmarshal([]byte(line), &m) == nil && m.Truncated {
		return m.Reason
	}
	return ""
}

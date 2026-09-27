package main

import (
	"context"
	"encoding/json/jsontext"
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

// The API commands of the CLI (docs/DEPLOYMENT.md "Command line"): thin
// wrappers around the local API (apiclient.go: --url, else PICACHE_URL,
// else the effective local web listener; the token from PICACHE_TOKEN or
// --token-file). Every string from the API is printed through
// escapeControls; --json prints the API's answer as it is (JSON escapes
// control characters). Exit codes: 0 ok, 1 error, 2 usage.

const apiUsage = `usage: picache status [--watch] [--interval 5s] [--json]
       picache pause <duration>            (e.g. 30m, 2h, 1d; 1 s to 7 days)
       picache resume
       picache explain <domain> [--client IP] [--type QTYPE] [--json]
       picache lists update
       picache allow|deny <domain> [--group NAME|ID]... [--comment TEXT]
       picache query <name> [type] [--client IP] [--json]
       picache config get [section] | set <section> <file|-> [--dry-run] | apply <file|-> [--dry-run]
all of them: [--url URL] [--token-file FILE] (a read token is enough for status, explain, query and config get)`

// apiFlags are the flags of every API command.
type apiFlags struct {
	fs        *flag.FlagSet
	url       *string
	tokenFile *string
}

func newAPIFlags(name string) *apiFlags {
	fs := newFlags(name)
	return &apiFlags{fs: fs, url: fs.String("url", "", "PiCache URL"), tokenFile: fs.String("token-file", "", "file with the API token")}
}

// parse parses flags and positional arguments in any order.
func (f *apiFlags) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := f.fs.Parse(args); err != nil {
			return nil, err
		}
		if f.fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, f.fs.Arg(0))
		args = f.fs.Args()[1:]
	}
}

func apiUsageError(err error) int {
	fmt.Fprintf(os.Stderr, "picache: %v\n%s\n", err, apiUsage)
	return 2
}

// fail prints an error of a command (exit 1).
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "picache:", escapeControls(err.Error()))
	return 1
}

// commandContext ends on SIGINT and SIGTERM.
func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// jsonBody encodes a request body.
func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only the CLI's own request types
	}
	return b
}

// printRaw prints a JSON answer (--json). JSON escapes the C0 controls;
// the C1 and bidi controls, which may appear raw in JSON strings, are
// escaped too (only strings can hold them, so the JSON stays valid).
func printRaw(w io.Writer, raw []byte) {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(string(raw), "�") {
		switch {
		case r == 0x7f, r >= 0x80 && r <= 0x9f, r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e,
			r >= 0x2066 && r <= 0x2069:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	fmt.Fprintf(w, "%s\n", b.String())
}

// --- status ---

// statusData is what status shows.
type statusData struct {
	Overview struct {
		Blocking struct {
			Enabled     bool       `json:"enabled"`
			PausedUntil *time.Time `json:"pausedUntil"`
			Permanent   bool       `json:"permanent"`
		} `json:"blocking"`
		Proxy struct {
			BytesHit int64 `json:"bytesHit"`
			BytesWAN int64 `json:"bytesWan"`
		} `json:"proxy"`
		DownloadCacheEnabled bool `json:"downloadCacheEnabled"`
	}
	Health struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	StatsEnabled bool
	Summary      struct {
		DNSQueries     int64   `json:"dnsQueries"`
		DNSBlocked     int64   `json:"dnsBlocked"`
		BlockedPercent float64 `json:"blockedPercent"`
	}
	Top []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		Count int64  `json:"count"`
	}
}

// statusRaw is status --json: the answers of the routes as they are.
type statusRaw struct {
	Overview jsontext.Value `json:"overview"`
	Health   jsontext.Value `json:"health"`
	Summary  jsontext.Value `json:"summary,omitzero"`
	Top      jsontext.Value `json:"topClients,omitzero"`
}

func statusCmd(args []string) int {
	f := newAPIFlags("status")
	watch := f.fs.Bool("watch", false, "redraw every interval")
	interval := f.fs.Duration("interval", 5*time.Second, "redraw interval (2s to 60s)")
	asJSON := f.fs.Bool("json", false, "print the API answers")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 0 {
		return apiUsageError(fmt.Errorf("unexpected argument %q", pos[0]))
	}
	if *interval < 2*time.Second || *interval > 60*time.Second {
		return apiUsageError(errors.New("--interval must be between 2s and 60s"))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	if !*watch {
		return statusOnce(ctx, c, *asJSON, os.Stdout)
	}
	return statusWatch(ctx, c, *interval, isTerminal(os.Stdout), os.Stdout, sleepCtx)
}

// isTerminal reports whether f is a character device (a terminal).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// statusOnce prints the status once.
func statusOnce(ctx context.Context, c *apiClient, asJSON bool, out io.Writer) int {
	if asJSON {
		var raw statusRaw
		if err := fetchStatusRaw(ctx, c, &raw); err != nil {
			return fail(err)
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return fail(err)
		}
		printRaw(out, b)
		return 0
	}
	d, err := fetchStatus(ctx, c)
	if err != nil {
		return fail(err)
	}
	fmt.Fprint(out, renderStatus(d, time.Now()))
	return 0
}

// statusWatch redraws the status every interval until ctx ends (exit 0):
// on a terminal the screen is cleared first, otherwise a block is printed
// per interval. A failed fetch is shown and tried again; a refused token
// ends it (exit 1).
func statusWatch(ctx context.Context, c *apiClient, interval time.Duration, term bool, out io.Writer,
	sleep func(context.Context, time.Duration) bool) int {
	for {
		d, err := fetchStatus(ctx, c)
		if ctx.Err() != nil {
			return 0
		}
		var se *statusError
		if errors.As(err, &se) && (se.status == http.StatusUnauthorized || se.status == http.StatusForbidden) {
			return fail(err)
		}
		block := ""
		if err != nil {
			block = "PiCache  " + time.Now().Format("15:04:05") + "\nerror: " + escapeControls(err.Error()) + "\n"
		} else {
			block = renderStatus(d, time.Now())
		}
		if term {
			fmt.Fprint(out, "\x1b[H\x1b[2J")
		}
		fmt.Fprint(out, block)
		if !term {
			fmt.Fprintln(out)
		}
		if !sleep(ctx, interval) {
			return 0
		}
	}
}

// fetchStatus reads the routes of status (read permission only).
func fetchStatus(ctx context.Context, c *apiClient) (*statusData, error) {
	d := &statusData{}
	if err := c.call(ctx, http.MethodGet, "/api/v1/system/overview", nil, nil, false, &d.Overview); err != nil {
		return nil, err
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/system/health", nil, nil, false, &d.Health); err != nil {
		return nil, err
	}
	var set struct {
		Logs struct {
			StatsEnabled bool `json:"statsEnabled"`
		} `json:"logs"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/settings", nil, nil, false, &set); err != nil {
		return nil, err
	}
	d.StatsEnabled = set.Logs.StatsEnabled
	if !d.StatsEnabled {
		return d, nil
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/stats/summary", neturl.Values{"range": {"24h"}}, nil, false, &d.Summary); err != nil {
		return nil, err
	}
	q := neturl.Values{"kind": {"clients"}, "range": {"24h"}, "limit": {"5"}}
	if err := c.call(ctx, http.MethodGet, "/api/v1/stats/top", q, nil, false, &d.Top); err != nil {
		return nil, err
	}
	return d, nil
}

// fetchStatusRaw reads the same routes for --json.
func fetchStatusRaw(ctx context.Context, c *apiClient, raw *statusRaw) error {
	if err := c.call(ctx, http.MethodGet, "/api/v1/system/overview", nil, nil, false, &raw.Overview); err != nil {
		return err
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/system/health", nil, nil, false, &raw.Health); err != nil {
		return err
	}
	var set struct {
		Logs struct {
			StatsEnabled bool `json:"statsEnabled"`
		} `json:"logs"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/settings", nil, nil, false, &set); err != nil {
		return err
	}
	if !set.Logs.StatsEnabled {
		return nil
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/stats/summary", neturl.Values{"range": {"24h"}}, nil, false, &raw.Summary); err != nil {
		return err
	}
	q := neturl.Values{"kind": {"clients"}, "range": {"24h"}, "limit": {"5"}}
	return c.call(ctx, http.MethodGet, "/api/v1/stats/top", q, nil, false, &raw.Top)
}

// renderStatus is the status block: about 40 columns and 12 lines (a small
// screen), every API string escaped.
func renderStatus(d *statusData, now time.Time) string {
	var b strings.Builder
	line := func(label, value string) { fmt.Fprintf(&b, "%-10s %s\n", label, value) }
	fmt.Fprintf(&b, "PiCache  %s\n", now.Format("15:04:05"))
	bl := d.Overview.Blocking
	switch {
	case bl.Enabled:
		line("Blocking", "on")
	case bl.PausedUntil != nil && !bl.Permanent:
		line("Blocking", "paused until "+bl.PausedUntil.Local().Format("Jan 2 15:04"))
	default:
		line("Blocking", "off")
	}
	if d.StatsEnabled {
		line("Queries", fmt.Sprintf("%d (24 h)", d.Summary.DNSQueries))
		line("Blocked", fmt.Sprintf("%d (%.1f %%)", d.Summary.DNSBlocked, d.Summary.BlockedPercent))
	} else {
		line("Queries", "statistics off")
		line("Blocked", "statistics off")
	}
	if p := d.Overview.Proxy; d.Overview.DownloadCacheEnabled || p.BytesHit+p.BytesWAN > 0 {
		rate := "-"
		if total := p.BytesHit + p.BytesWAN; total > 0 {
			rate = fmt.Sprintf("%.1f %%", float64(p.BytesHit)*100/float64(total))
		}
		line("Cache hits", rate)
	}
	var fails, warns []string
	for _, c := range d.Health.Checks {
		switch c.Status {
		case "fail":
			fails = append(fails, escapeControls(c.Name))
		case "warn":
			warns = append(warns, escapeControls(c.Name))
		}
	}
	switch {
	case len(fails) == 0 && len(warns) == 0:
		line("Health", "ok")
	default:
		line("Health", fmt.Sprintf("%d failing, %d warnings", len(fails), len(warns)))
		if len(fails) > 0 {
			line("", "fail: "+strings.Join(fails, ", "))
		}
		if len(warns) > 0 {
			line("", "warn: "+strings.Join(warns, ", "))
		}
	}
	if d.StatsEnabled && len(d.Top) > 0 {
		b.WriteString("Top clients (24 h)\n")
		for i, t := range d.Top {
			name := t.Key
			if t.Label != "" {
				name = t.Label
			}
			fmt.Fprintf(&b, "%d. %-26s %d\n", i+1, cut(escapeControls(name), 26), t.Count)
		}
	}
	return b.String()
}

// cut shortens s to n runes.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// --- pause / resume ---

// parsePause parses the duration of pause: a Go duration or <n>d, 1 s to
// 7 days.
func parsePause(s string) (time.Duration, error) {
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid duration %q (e.g. 30m, 2h, 1d)", s)
		}
	}
	if d < time.Second || d > 7*24*time.Hour {
		return 0, errors.New("the pause must be between 1 s and 7 days")
	}
	return d, nil
}

// blockingAnswer is the answer of POST /dns/blocking.
type blockingAnswer struct {
	Enabled     bool       `json:"enabled"`
	PausedUntil *time.Time `json:"pausedUntil"`
}

func pauseCmd(args []string) int {
	f := newAPIFlags("pause")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 1 {
		return apiUsageError(errors.New("give the duration of the pause"))
	}
	d, err := parsePause(pos[0])
	if err != nil {
		return apiUsageError(err)
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	var st blockingAnswer
	body := jsonBody(struct {
		Enabled      bool `json:"enabled"`
		PauseSeconds int  `json:"pauseSeconds"`
	}{false, int(d / time.Second)})
	if err := c.call(ctx, http.MethodPost, "/api/v1/dns/blocking", nil, body, true, &st); err != nil {
		return fail(err)
	}
	if st.PausedUntil != nil {
		fmt.Println("blocking paused until", st.PausedUntil.Local().Format("2006-01-02 15:04:05"))
	} else {
		fmt.Println("blocking paused")
	}
	return 0
}

func resumeCmd(args []string) int {
	f := newAPIFlags("resume")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 0 {
		return apiUsageError(fmt.Errorf("unexpected argument %q", pos[0]))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	if err := c.call(ctx, http.MethodPost, "/api/v1/dns/blocking", nil, jsonBody(map[string]any{"enabled": true}), true, nil); err != nil {
		return fail(err)
	}
	fmt.Println("blocking resumed")
	return 0
}

// --- explain ---

type explainAnswer struct {
	Domain   string `json:"domain"`
	QType    string `json:"qtype"`
	Decision struct {
		Action string `json:"action"`
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"decision"`
	Matches []struct {
		Action   string `json:"action"`
		Source   string `json:"source"`
		Name     string `json:"name"`
		Pattern  string `json:"pattern"`
		Applies  bool   `json:"applies"`
		Decisive bool   `json:"decisive"`
	} `json:"matches"`
}

func explainCmd(args []string) int {
	f := newAPIFlags("explain")
	client := f.fs.String("client", "", "evaluate as this client address")
	qtype := f.fs.String("type", "", "query type (default A)")
	asJSON := f.fs.Bool("json", false, "print the API answer")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 1 {
		return apiUsageError(errors.New("give one domain"))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	body := map[string]any{"domain": pos[0]}
	if *client != "" {
		body["clientIp"] = *client
	}
	if *qtype != "" {
		body["qtype"] = *qtype
	}
	raw, err := c.callRaw(ctx, http.MethodPost, "/api/v1/filter/explain", nil, jsonBody(body), false)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		printRaw(os.Stdout, raw)
		return 0
	}
	var a explainAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail(err)
	}
	fmt.Print(renderExplain(&a))
	return 0
}

func renderExplain(a *explainAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", escapeControls(a.Domain), escapeControls(a.QType))
	switch a.Decision.Action {
	case "", "none":
		b.WriteString("decision: not filtered\n")
	default:
		fmt.Fprintf(&b, "decision: %s by %s %q\n", escapeControls(a.Decision.Action), escapeControls(a.Decision.Source),
			escapeControls(a.Decision.Name))
	}
	for _, m := range a.Matches {
		state := "does not apply"
		switch {
		case m.Decisive:
			state = "decisive"
		case m.Applies:
			state = "applies"
		}
		fmt.Fprintf(&b, "  %-5s %-4s %s: %s (%s)\n", escapeControls(m.Action), escapeControls(m.Source), escapeControls(m.Name),
			escapeControls(m.Pattern), state)
	}
	return b.String()
}

// --- lists update ---

func listsCmd(args []string) int {
	if len(args) == 0 || args[0] != "update" {
		return apiUsageError(errors.New("usage: picache lists update"))
	}
	f := newAPIFlags("lists update")
	pos, err := f.parse(args[1:])
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 0 {
		return apiUsageError(fmt.Errorf("unexpected argument %q", pos[0]))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	if err := c.call(ctx, http.MethodPost, "/api/v1/filter/lists/refresh", nil, nil, true, nil); err != nil {
		return fail(err)
	}
	fmt.Println("refresh started")
	return 0
}

// --- allow / deny ---

func ruleCmd(action string, args []string) int {
	f := newAPIFlags(action)
	var groups multiFlag
	f.fs.Var(&groups, "group", "group name or id (repeatable)")
	comment := f.fs.String("comment", "", "comment of the rule")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) != 1 {
		return apiUsageError(errors.New("give one domain"))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	body := map[string]any{"action": map[string]string{"allow": "allow", "deny": "block"}[action], "type": "subtree",
		"pattern": pos[0], "enabled": true, "comment": *comment}
	if len(groups) > 0 {
		ids, err := resolveGroups(ctx, c, groups)
		if err != nil {
			return fail(err)
		}
		body["groupIds"] = ids
	}
	var rule struct {
		ID      int64  `json:"id"`
		Action  string `json:"action"`
		Pattern string `json:"pattern"`
	}
	err = c.call(ctx, http.MethodPost, "/api/v1/filter/rules", nil, jsonBody(body), true, &rule)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusConflict && strings.HasSuffix(err.Error(), ": this rule already exists") {
		fmt.Println("this rule already exists")
		return 0
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("rule %d added: %s %s and its subdomains\n", rule.ID, escapeControls(rule.Action), escapeControls(rule.Pattern))
	return 0
}

// resolveGroups maps group names (case-insensitive) or ids to ids through
// GET /groups.
func resolveGroups(ctx context.Context, c *apiClient, in []string) ([]int64, error) {
	var groups []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v1/groups", nil, nil, false, &groups); err != nil {
		return nil, err
	}
	var out []int64
	for _, g := range in {
		found := int64(0)
		for _, have := range groups {
			if strings.EqualFold(have.Name, g) || strconv.FormatInt(have.ID, 10) == g {
				found = have.ID
				break
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("no group named %s", g)
		}
		out = append(out, found)
	}
	return out, nil
}

// --- query ---

type lookupAnswer struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	RCode    string   `json:"rcode"`
	Answers  []string `json:"answers"`
	Reason   string   `json:"reason"`
	Upstream string   `json:"upstream"`
	Steps    []string `json:"steps"`
}

func queryCmd(args []string) int {
	f := newAPIFlags("query")
	client := f.fs.String("client", "", "evaluate as this client address")
	asJSON := f.fs.Bool("json", false, "print the API answer")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) < 1 || len(pos) > 2 {
		return apiUsageError(errors.New("give a name and optionally a type"))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	body := map[string]any{"name": pos[0]}
	if len(pos) == 2 {
		body["type"] = pos[1]
	}
	if *client != "" {
		body["clientIp"] = *client
	}
	raw, err := c.callRaw(ctx, http.MethodPost, "/api/v1/dns/lookup", nil, jsonBody(body), false)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		printRaw(os.Stdout, raw)
		return 0
	}
	var a lookupAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return fail(err)
	}
	fmt.Print(renderLookup(&a))
	return 0
}

func renderLookup(a *lookupAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s", escapeControls(a.Name), escapeControls(a.Type), escapeControls(a.Status))
	if a.RCode != "" {
		fmt.Fprintf(&b, " (%s)", escapeControls(a.RCode))
	}
	b.WriteString("\n")
	if a.Reason != "" {
		fmt.Fprintf(&b, "reason: %s\n", escapeControls(a.Reason))
	}
	if a.Upstream != "" {
		fmt.Fprintf(&b, "upstream: %s\n", escapeControls(a.Upstream))
	}
	for _, ans := range a.Answers {
		fmt.Fprintf(&b, "  %s\n", escapeControls(ans))
	}
	if len(a.Steps) > 0 {
		b.WriteString("steps:\n")
		for i, s := range a.Steps {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, escapeControls(s))
		}
	}
	return b.String()
}

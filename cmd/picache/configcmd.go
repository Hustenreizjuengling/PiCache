package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strings"
)

// picache config (docs/DEPLOYMENT.md "Declarative configuration"): the
// settings document through the API. get prints it (or one section) as
// JSON without cache.activeStoreId and the read-only members; set sends
// one section (PATCH /settings/{section}), apply the whole or a partial
// document (PUT /settings, decoded on top of the current one by the
// server, so it applies completely or not at all); --dry-run runs every
// check without storing. The JSON comes from a file or stdin, never from
// an argument, so no secret reaches the shell history or ps.

// maxConfigFile bounds a settings file.
const maxConfigFile = 1 << 20

// readOnlyMembers are left out of get and removed before set and apply
// (JSON pointers into the settings document).
var readOnlyMembers = []string{"/cache/activeStoreId", "/logs/privacyLevel", "/sync/tokenSet", "/network/proxy/passwordSet"}

// settingsSections are the sections of PATCH /settings/{section}.
var settingsSections = []string{"dns", "filter", "downloadCache", "cache", "logs", "web", "updates", "backups", "dhcp", "health",
	"clients", "sync", "network", "ntp"}

func configCmd(args []string) int {
	if len(args) == 0 {
		return apiUsageError(errors.New("usage: picache config get|set|apply"))
	}
	switch args[0] {
	case "get":
		return configGet(args[1:])
	case "set":
		return configWrite(args[1:], true)
	case "apply":
		return configWrite(args[1:], false)
	}
	return apiUsageError(fmt.Errorf("unknown config command %q", args[0]))
}

func configGet(args []string) int {
	f := newAPIFlags("config get")
	f.fs.Bool("json", true, "print JSON (always)")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	if len(pos) > 1 {
		return apiUsageError(errors.New("give at most one section"))
	}
	section := ""
	if len(pos) == 1 {
		if section = pos[0]; !slices.Contains(settingsSections, section) {
			return apiUsageError(fmt.Errorf("unknown settings section %q (%s)", section, strings.Join(settingsSections, ", ")))
		}
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	raw, err := c.callRaw(ctx, http.MethodGet, "/api/v1/settings", nil, nil, false)
	if err != nil {
		return fail(err)
	}
	out, err := settingsView(raw, section)
	if err != nil {
		return fail(err)
	}
	printRaw(os.Stdout, out)
	return 0
}

// settingsView returns the document (or one section) without the
// read-only members, indented.
func settingsView(raw []byte, section string) ([]byte, error) {
	drop := readOnlyMembers
	if section != "" {
		var doc map[string]jsontext.Value
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		v, ok := doc[section]
		if !ok {
			return nil, fmt.Errorf("the settings have no section %s", section)
		}
		raw, drop = v, nil
		for _, p := range readOnlyMembers {
			if rest, ok := strings.CutPrefix(p, "/"+section+"/"); ok {
				drop = append(drop, "/"+rest)
			}
		}
	}
	return dropMembers(raw, drop)
}

func configWrite(args []string, section bool) int {
	name := "config apply"
	if section {
		name = "config set"
	}
	f := newAPIFlags(name)
	dry := f.fs.Bool("dry-run", false, "check only, store nothing")
	pos, err := f.parse(args)
	if err != nil {
		return apiUsageError(err)
	}
	want := 1
	if section {
		want = 2
	}
	if len(pos) != want {
		return apiUsageError(errors.New("usage: picache config set <section> <file|-> | config apply <file|-> [--dry-run]"))
	}
	sec := ""
	if section {
		if sec = pos[0]; !slices.Contains(settingsSections, sec) {
			return apiUsageError(fmt.Errorf("unknown settings section %q (%s)", sec, strings.Join(settingsSections, ", ")))
		}
	}
	body, err := readConfigInput(pos[len(pos)-1], os.Stdin)
	if err != nil {
		return fail(err)
	}
	drop := readOnlyMembers
	if section {
		drop = nil
		for _, p := range readOnlyMembers {
			if rest, ok := strings.CutPrefix(p, "/"+sec+"/"); ok {
				drop = append(drop, "/"+rest)
			}
		}
	}
	if body, err = dropMembers(body, drop); err != nil {
		return fail(fmt.Errorf("invalid JSON: %w", err))
	}
	c, code := clientFor(*f.url, *f.tokenFile)
	if c == nil {
		return code
	}
	ctx, stop := commandContext()
	defer stop()
	q := neturl.Values{}
	if *dry {
		q.Set("dryRun", "true")
	}
	method, path := http.MethodPut, "/api/v1/settings"
	if section {
		method, path = http.MethodPatch, "/api/v1/settings/"+sec
	}
	if err := c.call(ctx, method, path, q, body, true, nil); err != nil {
		return fail(err)
	}
	if *dry {
		fmt.Println("the settings are valid; nothing was stored (--dry-run)")
	} else {
		fmt.Println("settings saved")
	}
	return 0
}

// readConfigInput reads a settings file (regular, not a link, at most
// 1 MiB) or stdin ("-").
func readConfigInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, maxConfigFile+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxConfigFile {
			return nil, errors.New("the input is larger than 1 MiB")
		}
		return b, nil
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > maxConfigFile {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	fh, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxConfigFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigFile {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return b, nil
}

// dropMembers copies one JSON value without the object members at the
// JSON pointers drop, keeping the order (duplicate names and trailing
// data are errors), indented.
func dropMembers(in []byte, drop []string) ([]byte, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(in))
	var buf bytes.Buffer
	enc := jsontext.NewEncoder(&buf, jsontext.WithIndent("  "))
	for {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if kind, n := dec.StackIndex(dec.StackDepth()); kind == '{' && n%2 == 1 && tok.Kind() == '"' &&
			slices.Contains(drop, string(dec.StackPointer())) {
			if err := dec.SkipValue(); err != nil {
				return nil, err
			}
			continue
		}
		if err := enc.WriteToken(tok); err != nil {
			return nil, err
		}
	}
	if buf.Len() == 0 {
		return nil, errors.New("no JSON value")
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

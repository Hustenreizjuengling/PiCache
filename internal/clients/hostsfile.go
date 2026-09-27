package clients

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"strings"
	"time"
)

// The name source hostsFile (clients.nameSources.hostsFile, off by
// default): the names of /etc/hosts of the machine PiCache runs on (in
// Docker the container's own file) become client names; never DNS answers
// (local records keep that role). The file is read when the source is
// switched on and every 5 minutes when its size or modification time
// changed: at most 1 MiB and 10 000 lines (the rest is ignored, logged
// once), lines longer than 4 KiB skipped, with the syntax and the skips of
// the hosts import (docs/API.md): "#" comments, loopback and unspecified
// addresses, the junk names of hosts-file headers, invalid host names. The
// first name of an address wins; names are cleaned like other untrusted
// names (sanitizeHostname).

// Bounds and interval of the hosts file.
const (
	hostsMaxBytes = 1 << 20
	hostsMaxLines = 10000
	hostsMaxLine  = 4 << 10
	hostsEvery    = 5 * time.Minute
)

// hostsJunk are the names of hosts-file headers (the hosts import's list).
var hostsJunk = map[string]bool{
	"localhost": true, "localhost.localdomain": true, "local": true, "broadcasthost": true,
	"ip6-localhost": true, "ip6-loopback": true, "ip6-localnet": true, "ip6-mcastprefix": true,
	"ip6-allnodes": true, "ip6-allrouters": true, "ip6-allhosts": true, "0.0.0.0": true,
}

// hostsTable is a snapshot of the names of the hosts file.
type hostsTable struct {
	names   map[netip.Addr]string
	size    int64
	modTime time.Time
	read    bool // the file was read (size and modTime describe it)
}

// hostsName returns the hosts-file name of ip ("" if none or the source is
// off).
func (r *Registry) hostsName(ip netip.Addr) string {
	if !r.config().Sources.HostsFile {
		return ""
	}
	if t := r.hosts.Load(); t != nil {
		return t.names[ip]
	}
	return ""
}

// hostsLoop reads the hosts file while the source is on: at once when it
// is switched on (hostsKick) and every 5 minutes.
func (r *Registry) hostsLoop(ctx context.Context) {
	tick := time.NewTicker(hostsEvery)
	defer tick.Stop()
	warned := false
	for {
		if r.config().Sources.HostsFile {
			r.refreshHosts(&warned)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.hostsKick:
		}
	}
}

// refreshHosts reads the file again when its size or modification time
// changed and drops the cached identities when its names changed.
func (r *Registry) refreshHosts(warned *bool) {
	cur := r.hosts.Load()
	fi, err := os.Stat(r.hostsPath)
	if err != nil || !fi.Mode().IsRegular() {
		if cur != nil && len(cur.names) > 0 {
			r.hosts.Store(&hostsTable{})
			r.invalidate()
		}
		return
	}
	if cur != nil && cur.read && cur.size == fi.Size() && cur.modTime.Equal(fi.ModTime()) {
		return
	}
	f, err := os.Open(r.hostsPath)
	if err != nil {
		return
	}
	names, truncated := parseHostsFile(f)
	f.Close()
	if truncated && !*warned {
		*warned = true
		r.log.Warn("the hosts file is larger than 1 MiB or 10000 lines; the rest is ignored", slog.String("path", r.hostsPath))
	}
	if !r.config().Sources.HostsFile {
		return // switched off meanwhile
	}
	r.hosts.Store(&hostsTable{names: names, size: fi.Size(), modTime: fi.ModTime(), read: true})
	if cur == nil || !maps.Equal(cur.names, names) {
		r.invalidate()
	}
}

// parseHostsFile returns the first valid name of every address of a hosts
// file and whether the file had more than 1 MiB or 10 000 lines.
func parseHostsFile(rd io.Reader) (map[netip.Addr]string, bool) {
	out := map[netip.Addr]string{}
	lr := &io.LimitedReader{R: rd, N: hostsMaxBytes}
	br := bufio.NewReaderSize(lr, 64<<10)
	truncated := false
	for n := 0; ; n++ {
		if n == hostsMaxLines {
			if _, err := br.Peek(1); err == nil {
				truncated = true
			}
			break
		}
		line, tooLong, err := readHostsLine(br)
		if err != nil && len(line) == 0 && !tooLong {
			break
		}
		if !tooLong {
			parseHostsFileLine(line, out)
		}
		if err != nil {
			break
		}
	}
	if lr.N <= 0 {
		if _, err := rd.Read(make([]byte, 1)); err == nil {
			truncated = true
		}
	}
	return out, truncated
}

// readHostsLine reads one line; tooLong reports a line of more than 4 KiB
// (read to its end and skipped).
func readHostsLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf bytes.Buffer
	for {
		part, isPrefix, err := br.ReadLine()
		if buf.Len()+len(part) > hostsMaxLine {
			tooLong = true
		} else {
			buf.Write(part)
		}
		if err != nil {
			return buf.Bytes(), tooLong, err
		}
		if !isPrefix {
			return buf.Bytes(), tooLong, nil
		}
	}
}

// parseHostsFileLine adds the first valid name of a line's address unless
// the address already has one.
func parseHostsFileLine(b []byte, out map[netip.Addr]string) {
	text := string(b)
	if i := strings.IndexByte(text, '#'); i >= 0 {
		text = text[:i]
	}
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return
	}
	ip, err := netip.ParseAddr(fields[0])
	if err != nil || ip.Zone() != "" {
		return
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || !ip.IsValid() {
		return
	}
	if _, ok := out[ip]; ok {
		return
	}
	for _, name := range fields[1:] {
		n := sanitizeHostname(name)
		if n == "" || hostsJunk[n] || strings.HasPrefix(n, "ip6-") {
			continue
		}
		out[ip] = n
		return
	}
}

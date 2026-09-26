// Package hostinfo samples the resources of the host PiCache runs on
// (docs/ARCHITECTURE.md 4): load, uptime, memory, the memory of its own
// cgroup, temperatures, the model and the disks of the data and cache
// directories. Everything is read from /proc and /sys (below an injectable
// root for tests); a value that cannot be read is left out and never
// warns. Every read is bounded. It imports no PiCache package.
package hostinfo

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bounds of the reads.
const (
	maxZones    = 32       // thermal zones read
	maxZoneRead = 64       // bytes read of a thermal zone's type and temp
	maxModel    = 256      // bytes of the device-tree model
	maxSmall    = 4 << 10  // loadavg, uptime, cgroup files
	maxMeminfo  = 64 << 10 // meminfo, memory.stat
	minCelsius  = -40.0
	maxCelsius  = 150.0
	cgroupMount = "/sys/fs/cgroup"
)

// Info is a sample of the host (api.HostInfo). In a container (Container)
// the load, the uptime and Memory are the host's.
type Info struct {
	SampledAt    time.Time     `json:"sampledAt"`
	Container    bool          `json:"container"`
	Model        string        `json:"model,omitempty"`
	CPUs         int           `json:"cpus"`
	Load         *Load         `json:"load,omitempty"`
	UptimeSec    *int64        `json:"uptimeSec,omitempty"`
	Memory       *Memory       `json:"memory,omitempty"`
	Cgroup       *Cgroup       `json:"cgroup,omitempty"`
	Temperatures []Temperature `json:"temperatures"`
	Disks        []Disk        `json:"disks"`
}

// Load is the load average.
type Load struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

// Memory is the memory of the host (/proc/meminfo).
type Memory struct {
	TotalBytes     int64 `json:"totalBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	UsedBytes      int64 `json:"usedBytes"` // total − available
	SwapTotalBytes int64 `json:"swapTotalBytes"`
	SwapUsedBytes  int64 `json:"swapUsedBytes"`
}

// Cgroup is the memory limit of PiCache's own cgroup (v2) and its usage
// without the inactive page cache.
type Cgroup struct {
	LimitBytes     int64 `json:"limitBytes"`
	UsageBytes     int64 `json:"usageBytes"`
	AvailableBytes int64 `json:"availableBytes"`
}

// Temperature is a thermal zone.
type Temperature struct {
	Zone    string  `json:"zone"` // thermal_zone0
	Type    string  `json:"type"` // cpu-thermal, x86_pkg_temp, …
	Celsius float64 `json:"celsius"`
}

// Disk is the filesystem of a directory.
type Disk struct {
	Path       string `json:"path"`
	Role       string `json:"role"` // data | cache
	TotalBytes uint64 `json:"totalBytes"`
	FreeBytes  uint64 `json:"freeBytes"`
}

// DiskPath is a directory whose filesystem is sampled.
type DiskPath struct {
	Path string
	Role string // data | cache
}

// Sampler reads samples.
type Sampler struct {
	Root      string // "" = "/"; the tree that holds proc and sys
	Container bool
	Disks     []DiskPath
	// DiskUsage returns the size and the free bytes of the filesystem of a
	// directory (nil: the operating system's).
	DiskUsage func(dir string) (total, free uint64, ok bool)
}

// Sample reads the host's values now.
func (s *Sampler) Sample(now time.Time) Info {
	in := Info{SampledAt: now.UTC(), Container: s.Container, CPUs: runtime.NumCPU(),
		Temperatures: []Temperature{}, Disks: []Disk{}}
	in.Model = s.model()
	in.Load = s.load()
	in.UptimeSec = s.uptime()
	in.Memory = s.memory()
	in.Cgroup = s.cgroup()
	in.Temperatures = s.temperatures()
	usage := s.DiskUsage
	if usage == nil {
		usage = diskUsage
	}
	for _, d := range s.Disks {
		if total, free, ok := usage(d.Path); ok {
			in.Disks = append(in.Disks, Disk{Path: d.Path, Role: d.Role, TotalBytes: total, FreeBytes: free})
		}
	}
	return in
}

// Values reports whether anything besides the CPU count was read.
func (in *Info) Values() bool {
	return in.Load != nil || in.UptimeSec != nil || in.Memory != nil || in.Cgroup != nil || len(in.Temperatures) > 0
}

// file returns the path of p (absolute, slash-separated) below the root.
func (s *Sampler) file(p string) string {
	root := s.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

// read returns at most n bytes of a file (nil when it cannot be read).
func (s *Sampler) read(p string, n int64) []byte {
	f, err := os.Open(s.file(p))
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return nil
	}
	return b
}

func (s *Sampler) model() string {
	b := s.read("/proc/device-tree/model", maxModel)
	if len(b) == 0 {
		b = s.read("/sys/class/dmi/id/product_name", maxModel)
	}
	return cleanText(string(bytes.TrimRight(b, "\x00\n ")))
}

// cleanText removes control and bidi characters and invalid UTF-8.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return strings.TrimSpace(s)
}

func (s *Sampler) load() *Load {
	f := strings.Fields(string(s.read("/proc/loadavg", maxSmall)))
	if len(f) < 3 {
		return nil
	}
	var v [3]float64
	for i := range v {
		x, err := strconv.ParseFloat(f[i], 64)
		if err != nil || x < 0 || x > 1e6 {
			return nil
		}
		v[i] = x
	}
	return &Load{One: v[0], Five: v[1], Fifteen: v[2]}
}

func (s *Sampler) uptime() *int64 {
	f := strings.Fields(string(s.read("/proc/uptime", maxSmall)))
	if len(f) < 1 {
		return nil
	}
	x, err := strconv.ParseFloat(f[0], 64)
	if err != nil || x < 0 || x > 1e12 {
		return nil
	}
	sec := int64(x)
	return &sec
}

// keyValues parses "Key: value [kB]" lines (meminfo) or "key value" lines
// (memory.stat) into bytes for meminfo (kB) or plain numbers.
func keyValues(b []byte, kb bool) map[string]int64 {
	out := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(strings.Replace(sc.Text(), ":", " ", 1))
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || v < 0 {
			continue
		}
		if kb {
			if v > 1<<52 {
				continue
			}
			v *= 1024
		}
		out[f[0]] = v
	}
	return out
}

func (s *Sampler) memory() *Memory {
	m := keyValues(s.read("/proc/meminfo", maxMeminfo), true)
	total, ok1 := m["MemTotal"]
	avail, ok2 := m["MemAvailable"]
	if !ok1 || !ok2 || total <= 0 || avail > total {
		return nil
	}
	mem := &Memory{TotalBytes: total, AvailableBytes: avail, UsedBytes: total - avail}
	if st, ok := m["SwapTotal"]; ok {
		mem.SwapTotalBytes = st
		if sf, ok := m["SwapFree"]; ok && sf <= st {
			mem.SwapUsedBytes = st - sf
		}
	}
	return mem
}

// cgroupDir returns the directory of the own cgroup (v2, "0::<path>" in
// /proc/self/cgroup), confined below /sys/fs/cgroup.
func (s *Sampler) cgroupDir() (string, bool) {
	for line := range strings.SplitSeq(string(s.read("/proc/self/cgroup", maxSmall)), "\n") {
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !ok || !strings.HasPrefix(p, "/") {
			continue
		}
		dir := path.Join(cgroupMount, path.Clean(p))
		if dir != cgroupMount && !strings.HasPrefix(dir, cgroupMount+"/") {
			return "", false
		}
		return dir, true
	}
	return "", false
}

func (s *Sampler) cgroup() *Cgroup {
	dir, ok := s.cgroupDir()
	if !ok {
		return nil
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(s.read(dir+"/memory.max", maxSmall))), 10, 64)
	if err != nil || limit <= 0 { // "max": no limit
		return nil
	}
	current, err := strconv.ParseInt(strings.TrimSpace(string(s.read(dir+"/memory.current", maxSmall))), 10, 64)
	if err != nil || current < 0 {
		return nil
	}
	usage := current
	if inactive, ok := keyValues(s.read(dir+"/memory.stat", maxMeminfo), false)["inactive_file"]; ok && inactive <= usage {
		usage -= inactive
	}
	return &Cgroup{LimitBytes: limit, UsageBytes: usage, AvailableBytes: max(limit-usage, 0)}
}

func (s *Sampler) temperatures() []Temperature {
	dir := s.file("/sys/class/thermal")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Temperature{}
	}
	var zones []string
	for _, e := range entries {
		if n, ok := strings.CutPrefix(e.Name(), "thermal_zone"); ok {
			if _, err := strconv.Atoi(n); err == nil {
				zones = append(zones, e.Name())
			}
		}
	}
	slices.SortFunc(zones, func(a, b string) int {
		x, _ := strconv.Atoi(strings.TrimPrefix(a, "thermal_zone"))
		y, _ := strconv.Atoi(strings.TrimPrefix(b, "thermal_zone"))
		return x - y
	})
	out := []Temperature{}
	for _, z := range zones[:min(len(zones), maxZones)] {
		raw := strings.TrimSpace(string(s.read("/sys/class/thermal/"+z+"/temp", maxZoneRead)))
		milli, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		c := float64(milli) / 1000
		if c < minCelsius || c > maxCelsius {
			continue
		}
		typ := cleanText(string(s.read("/sys/class/thermal/"+z+"/type", maxZoneRead)))
		out = append(out, Temperature{Zone: z, Type: typ, Celsius: c})
	}
	return out
}

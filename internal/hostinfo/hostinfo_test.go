package hostinfo

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tree writes files below a temporary root (slash paths).
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const meminfo = `MemTotal:        3884052 kB
MemFree:          148724 kB
MemAvailable:    2911232 kB
Buffers:           61324 kB
SwapTotal:        102396 kB
SwapFree:         100000 kB
`

func TestSample(t *testing.T) {
	root := tree(t, map[string]string{
		"proc/loadavg":           "0.52 0.58 1.59 1/345 12345\n",
		"proc/uptime":            "12345.67 54321.00\n",
		"proc/meminfo":           meminfo,
		"proc/device-tree/model": "Raspberry Pi 4 Model B Rev 1.4\x00",
		"proc/self/cgroup":       "0::/system.slice/picache.service\n",
		"sys/fs/cgroup/system.slice/picache.service/memory.max":     "536870912\n",
		"sys/fs/cgroup/system.slice/picache.service/memory.current": "300000000\n",
		"sys/fs/cgroup/system.slice/picache.service/memory.stat":    "anon 1000\nfile 5000\ninactive_file 100000000\n",
		"sys/class/thermal/thermal_zone0/type":                      "cpu-thermal\n",
		"sys/class/thermal/thermal_zone0/temp":                      "48250\n",
		"sys/class/thermal/thermal_zone1/type":                      "bogus\n",
		"sys/class/thermal/thermal_zone1/temp":                      "-273000\n",
		"sys/class/thermal/thermal_zone2/temp":                      "not a number",
		"sys/class/thermal/cooling_device0/type":                    "fan",
	})
	s := &Sampler{Root: root, Container: true, Disks: []DiskPath{{"/data", "data"}, {"/cache", "cache"}},
		DiskUsage: func(dir string) (uint64, uint64, bool) { return 1000, 250, dir == "/data" }}
	now := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	in := s.Sample(now)
	if !in.SampledAt.Equal(now) || !in.Container || in.CPUs != runtime.NumCPU() || in.Model != "Raspberry Pi 4 Model B Rev 1.4" {
		t.Fatalf("info %+v", in)
	}
	if in.Load == nil || *in.Load != (Load{0.52, 0.58, 1.59}) || in.UptimeSec == nil || *in.UptimeSec != 12345 {
		t.Fatalf("load %+v uptime %v", in.Load, in.UptimeSec)
	}
	if m := in.Memory; m == nil || m.TotalBytes != 3884052*1024 || m.AvailableBytes != 2911232*1024 ||
		m.UsedBytes != (3884052-2911232)*1024 || m.SwapTotalBytes != 102396*1024 || m.SwapUsedBytes != 2396*1024 {
		t.Fatalf("memory %+v", in.Memory)
	}
	if c := in.Cgroup; c == nil || c.LimitBytes != 536870912 || c.UsageBytes != 200000000 || c.AvailableBytes != 336870912 {
		t.Fatalf("cgroup %+v", in.Cgroup)
	}
	if fmt.Sprint(in.Temperatures) != "[{thermal_zone0 cpu-thermal 48.25}]" {
		t.Fatalf("temperatures %v", in.Temperatures)
	}
	if fmt.Sprint(in.Disks) != "[{/data data 1000 250}]" {
		t.Fatalf("disks %v", in.Disks)
	}
}

// Missing files leave values out; a cgroup path cannot leave /sys/fs/cgroup;
// "max" is no limit; the model falls back to the DMI product name.
func TestSampleMissingAndHostile(t *testing.T) {
	in := (&Sampler{Root: t.TempDir()}).Sample(time.Now())
	if in.Values() || in.Load != nil || in.Memory != nil || in.Cgroup != nil || in.UptimeSec != nil || in.Model != "" ||
		in.Temperatures == nil || in.Disks == nil {
		t.Fatalf("empty tree %+v", in)
	}
	root := tree(t, map[string]string{
		"proc/self/cgroup":                     "0::/../../../etc\n",
		"etc/memory.max":                       "100\n",
		"sys/class/dmi/id/product_name":        "Virtual\u202e Machine\n",
		"proc/loadavg":                         "1 2\n",
		"proc/meminfo":                         "MemTotal: 10 kB\nMemAvailable: 20 kB\n",
		"sys/class/thermal/thermal_zone0/temp": strings.Repeat("9", 200),
	})
	in = (&Sampler{Root: root}).Sample(time.Now())
	if in.Cgroup != nil || in.Load != nil || in.Memory != nil || len(in.Temperatures) != 0 {
		t.Fatalf("hostile tree %+v", in)
	}
	if in.Model != "Virtual Machine" {
		t.Fatalf("model %q", in.Model)
	}
	root = tree(t, map[string]string{
		"proc/self/cgroup":                "12:cpu:/x\n0::/\n",
		"sys/fs/cgroup/memory.max":        "max\n",
		"sys/fs/cgroup/memory.current":    "123\n",
		"proc/device-tree/model":          strings.Repeat("M", 400),
		"sys/class/thermal/thermal_zone0": "",
	})
	in = (&Sampler{Root: root}).Sample(time.Now())
	if in.Cgroup != nil || len(in.Model) != maxModel {
		t.Fatalf("no limit / long model: %+v", in)
	}
}

// At most 32 thermal zones are read, in numeric order.
func TestThermalZoneCap(t *testing.T) {
	files := map[string]string{}
	for i := range 40 {
		files[fmt.Sprintf("sys/class/thermal/thermal_zone%d/temp", i)] = fmt.Sprint(30000 + i*100)
		files[fmt.Sprintf("sys/class/thermal/thermal_zone%d/type", i)] = "z"
	}
	in := (&Sampler{Root: tree(t, files)}).Sample(time.Now())
	if len(in.Temperatures) != maxZones || in.Temperatures[0].Zone != "thermal_zone0" || in.Temperatures[31].Zone != "thermal_zone31" {
		t.Fatalf("%d zones: first %v last %v", len(in.Temperatures), in.Temperatures[0], in.Temperatures[len(in.Temperatures)-1])
	}
}

func TestCheck(t *testing.T) {
	th := Thresholds{MemoryAvailableMinPercent: 5, LoadPerCPUMax: 2, TemperatureMaxCelsius: 80}
	if _, _, show := (&Info{CPUs: 4}).Check(th); show {
		t.Fatal("shown without values")
	}
	ok := Info{CPUs: 4, Load: &Load{Fifteen: 8}, Memory: &Memory{TotalBytes: 100, AvailableBytes: 5},
		Temperatures: []Temperature{{Zone: "thermal_zone0", Celsius: 79.9}}}
	if st, msg, show := ok.Check(th); st != "ok" || msg != "" || !show {
		t.Fatalf("ok: %s %q", st, msg)
	}
	bad := Info{CPUs: 4, Load: &Load{Fifteen: 8.26}, Memory: &Memory{TotalBytes: 1000, AvailableBytes: 400},
		Cgroup: &Cgroup{LimitBytes: 1000, AvailableBytes: 31}, Temperatures: []Temperature{{Zone: "thermal_zone0", Type: "cpu-thermal", Celsius: 80}}}
	st, msg, _ := bad.Check(th)
	want := "memory available to PiCache's cgroup 3.1 % (below 5 %); 15-minute load 8.3 (above 8 = 2 per CPU × 4 CPUs); " +
		"thermal_zone0 (cpu-thermal) 80 °C (at or above 80 °C)"
	if st != "warn" || msg != want {
		t.Fatalf("warn: %s %q\nwant %q", st, msg, want)
	}
}

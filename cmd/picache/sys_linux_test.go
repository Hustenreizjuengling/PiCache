package main

import (
	"os"
	"path/filepath"
	"testing"
)

// OPS-7: the memory limit is the lowest limit of PiCache's own cgroup and
// its ancestors: a MemoryMax= of picache.service on a host (not only the
// root file of a container's namespace), and cgroup v1 hosts.
func TestCgroupMemoryLimit(t *testing.T) {
	// tree writes files below a fresh root: name → content.
	tree := func(files map[string]string) string {
		root := t.TempDir()
		for name, content := range files {
			p := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  int64
	}{
		{"v2 host: MemoryMax of the service", map[string]string{
			"proc/self/cgroup": "0::/system.slice/picache.service\n",
			"sys/fs/cgroup/system.slice/picache.service/memory.max": "335544320\n",
			"sys/fs/cgroup/system.slice/memory.max":                 "max\n",
		}, 335544320},
		{"v2 host: the lower limit of a parent slice", map[string]string{
			"proc/self/cgroup": "0::/system.slice/picache.service\n",
			"sys/fs/cgroup/system.slice/picache.service/memory.max": "max\n",
			"sys/fs/cgroup/system.slice/memory.max":                 "268435456\n",
		}, 268435456},
		{"v2 container namespace: the root file", map[string]string{
			"proc/self/cgroup":         "0::/\n",
			"sys/fs/cgroup/memory.max": "536870912\n",
		}, 536870912},
		{"v2 without a limit", map[string]string{
			"proc/self/cgroup": "0::/system.slice/picache.service\n",
			"sys/fs/cgroup/system.slice/picache.service/memory.max": "max\n",
		}, 0},
		{"v1 host", map[string]string{
			"proc/self/cgroup": "12:pids:/system.slice/picache.service\n5:memory:/system.slice/picache.service\n1:name=systemd:/system.slice/picache.service\n",
			"sys/fs/cgroup/memory/system.slice/picache.service/memory.limit_in_bytes": "402653184\n",
			"sys/fs/cgroup/memory/memory.limit_in_bytes":                              "9223372036854771712\n",
		}, 402653184},
		{"v1 container: its own cgroup is the mount's root", map[string]string{
			"proc/self/cgroup":                           "4:cpu,memory:/docker/0123abcd\n",
			"sys/fs/cgroup/memory/memory.limit_in_bytes": "1073741824\n",
		}, 1073741824},
		{"v1 unlimited", map[string]string{
			"proc/self/cgroup":                           "4:memory:/\n",
			"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n",
		}, 0},
		{"a path outside the mount is not followed", map[string]string{
			"proc/self/cgroup": "0::/../../../etc\n",
			"etc/memory.max":   "1234\n",
		}, 0},
		{"no /proc/self/cgroup: the root file", map[string]string{
			"sys/fs/cgroup/memory.max": "805306368\n",
		}, 805306368},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cgroupMemoryLimit(tree(tc.files)); got != tc.want {
				t.Fatalf("limit %d, want %d", got, tc.want)
			}
		})
	}
}

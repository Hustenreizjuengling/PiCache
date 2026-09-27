package hostinfo

import (
	"slices"
	"strings"
	"testing"
)

func TestInterfaces(t *testing.T) {
	files := map[string]string{
		"sys/class/net/lo/flags":   "0x9\n",
		"sys/class/net/lo/type":    "772\n",
		"sys/class/net/lo/ifindex": "1\n",

		"sys/class/net/eth0/ifindex":              "2\n",
		"sys/class/net/eth0/address":              "DC:A6:32:00:00:01\n",
		"sys/class/net/eth0/flags":                "0x1003\n",
		"sys/class/net/eth0/operstate":            "up\n",
		"sys/class/net/eth0/mtu":                  "1500\n",
		"sys/class/net/eth0/speed":                "1000\n",
		"sys/class/net/eth0/duplex":               "full\n",
		"sys/class/net/eth0/statistics/rx_bytes":  "123456789\n",
		"sys/class/net/eth0/statistics/tx_bytes":  "987654\n",
		"sys/class/net/eth0/statistics/rx_errors": "3\n",
		"sys/class/net/eth0/statistics/tx_errors": "0\n",

		// A tunnel: no MAC, speed -1 (EINVAL), unknown duplex, virtual.
		"sys/class/net/wg0/ifindex":     "5\n",
		"sys/class/net/wg0/address":     "00:00:00:00:00:00\n",
		"sys/class/net/wg0/flags":       "0x91\n",
		"sys/class/net/wg0/operstate":   "weird\x1b[31m\n",
		"sys/class/net/wg0/mtu":         "1420\n",
		"sys/class/net/wg0/speed":       "-1\n",
		"sys/class/net/wg0/duplex":      "unknown\n",
		"sys/devices/virtual/net/wg0/x": "",

		// Down, hostile values.
		"sys/class/net/eth1/ifindex":   "3\n",
		"sys/class/net/eth1/address":   "<script>\n",
		"sys/class/net/eth1/flags":     "0x1002\n",
		"sys/class/net/eth1/operstate": "down\n",
		"sys/class/net/eth1/mtu":       "x\n",
	}
	root := tree(t, files)
	got := (&Sampler{Root: root}).Interfaces()
	if len(got) != 3 || got[0].Name != "eth0" || got[1].Name != "eth1" || got[2].Name != "wg0" {
		t.Fatalf("interfaces %+v", got)
	}
	e := got[0]
	if e.Index != 2 || e.MAC != "dc:a6:32:00:00:01" || !e.Up || e.OperState != "up" || e.MTU != 1500 || e.SpeedMbps != 1000 ||
		e.Duplex != "full" || e.Virtual || e.RxBytes != 123456789 || e.TxBytes != 987654 || e.RxErrors != 3 {
		t.Fatalf("eth0 %+v", e)
	}
	d := got[1]
	if d.Up || d.OperState != "down" || d.MAC != "" || d.MTU != 0 || d.SpeedMbps != 0 {
		t.Fatalf("eth1 %+v", d)
	}
	w := got[2]
	if w.MAC != "" || w.SpeedMbps != 0 || w.Duplex != "" || !w.Virtual || w.OperState != "unknown" || !w.Up {
		t.Fatalf("wg0 %+v", w)
	}
	// No /sys: none, never nil.
	if got := (&Sampler{Root: t.TempDir()}).Interfaces(); got == nil || len(got) != 0 {
		t.Fatalf("without sysfs: %v", got)
	}
	// At most 64.
	many := map[string]string{}
	for i := range 70 {
		many["sys/class/net/"+strings.Repeat("x", 1)+string(rune('a'+i/26))+string(rune('a'+i%26))+"/flags"] = "0x1\n"
	}
	if got := (&Sampler{Root: tree(t, many)}).Interfaces(); len(got) != MaxInterfaces ||
		!slices.IsSortedFunc(got, func(a, b Interface) int { return strings.Compare(a.Name, b.Name) }) {
		t.Fatalf("%d interfaces", len(got))
	}
}

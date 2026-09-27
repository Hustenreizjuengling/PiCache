package app

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// listenerApp is an App whose configured listeners are loopback with
// ephemeral ports (DNS and web only) and no DHCP sockets.
func listenerApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), CacheDir: filepath.Join(dir, "cache"), MountRoot: filepath.Join(dir, "mnt"),
		DNSListen: []string{"127.0.0.1:0"}, WebListen: []string{"127.0.0.1:0"}, DHCP: config.DHCPOff}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return newApp(cfg, slog.New(slog.DiscardHandler))
}

// freeUDPPort returns a port that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func saveListeners(t *testing.T, a *App, name string, roles map[string][]string) {
	t.Helper()
	if err := config.WriteListenersFile(a.cfg.DataDir, name, config.EncodeListeners(roles)); err != nil {
		t.Fatal(err)
	}
}

// A saved DNS port that is taken and a saved web address this machine does
// not have fall back to the configured values for this start (never an
// exit caused by the file); listeners.json then holds what was bound from
// the file, listeners.failed.json the saved set, and the health check
// fails until a start binds everything.
func TestSavedListenersFallBack(t *testing.T) {
	a := listenerApp(t)
	taken, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	dnsAddr := taken.LocalAddr().String()
	ntpAddr := "127.0.0.1:" + strconv.Itoa(freeUDPPort(t))
	saveListeners(t, a, config.ListenersNextFile, map[string][]string{
		config.RoleDNS: {dnsAddr}, config.RoleWeb: {"192.0.2.1:8080"}, config.RoleNTP: {ntpAddr}})
	if err := a.bindListeners(); err != nil {
		t.Fatalf("a saved value made the start fail: %v", err)
	}
	defer a.ln.closeAll()
	info := a.Listeners()
	if len(info.Bound["dns-udp"]) != 1 || info.Bound["dns-udp"][0] == dnsAddr {
		t.Errorf("dns %v", info.Bound["dns-udp"])
	}
	if len(info.Bound["web"]) != 1 || !strings.HasPrefix(info.Bound["web"][0], "127.0.0.1:") {
		t.Errorf("web %v", info.Bound["web"])
	}
	if len(info.Bound["ntp"]) != 1 || info.Bound["ntp"][0] != ntpAddr {
		t.Errorf("ntp %v, want the saved %s", info.Bound["ntp"], ntpAddr)
	}
	if msg := info.Failed["dns"]; !strings.HasPrefix(msg, "listener dns from the saved listeners could not be bound (") ||
		!strings.HasSuffix(msg, "); using 127.0.0.1:0") {
		t.Errorf("dns failure %q", msg)
	}
	if status, _, _ := listenersHealth(info, settings.EncryptedDNS{}, false, a.ln.savedFailures()); status != "fail" {
		t.Errorf("health %s", status)
	}
	a.finishListenerFiles()
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, config.ListenersNextFile)); !os.IsNotExist(err) {
		t.Error("listeners.next.json was not removed")
	}
	cur, err := config.ReadListenersFile(a.cfg.DataDir, config.ListenersFile)
	if err != nil || len(cur) != 1 || cur[config.RoleNTP][0] != ntpAddr {
		t.Errorf("listeners.json %v %v", cur, err)
	}
	failed, err := config.ReadListenersFile(a.cfg.DataDir, config.ListenersFailedFile)
	if err != nil || failed[config.RoleDNS][0] != dnsAddr || failed[config.RoleWeb][0] != "192.0.2.1:8080" {
		t.Errorf("listeners.failed.json %v %v", failed, err)
	}
	if f := a.FailedListeners(); f == nil || f.Roles[config.RoleDNS] == "" || f.Roles[config.RoleWeb] == "" {
		t.Errorf("failed listeners %+v", f)
	}
}

// A broken listeners.next.json is ignored (listeners.json applies); a role
// set by the environment or a flag ignores the files.
func TestSavedListenersBrokenAndLocked(t *testing.T) {
	a := listenerApp(t)
	ntpAddr := "127.0.0.1:" + strconv.Itoa(freeUDPPort(t))
	saveListeners(t, a, config.ListenersFile, map[string][]string{config.RoleNTP: {ntpAddr}})
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, config.ListenersNextFile), []byte(`{"version":2}`), 0o640); err != nil {
		t.Fatal(err)
	}
	a.cfg.ListenerLock = map[string]string{config.RoleDNS: "PICACHE_DNS_LISTEN"}
	if err := a.bindListeners(); err != nil {
		t.Fatal(err)
	}
	defer a.ln.closeAll()
	if got := a.Listeners().Bound["ntp"]; len(got) != 1 || got[0] != ntpAddr {
		t.Errorf("ntp %v", got)
	}
	if len(a.ln.savedFailed) != 0 {
		t.Errorf("saved failures %v", a.ln.savedFailed)
	}
	a.finishListenerFiles()
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, config.ListenersFailedFile)); !os.IsNotExist(err) {
		t.Error("listeners.failed.json written although everything was bound")
	}
}

// PICACHE_RUN_AS (Docker) ignores the files: root never reads what the
// service can write.
func TestSavedListenersIgnoredWithRunAs(t *testing.T) {
	a := listenerApp(t)
	a.cfg.RunAs = "65532:65532"
	saveListeners(t, a, config.ListenersNextFile, map[string][]string{config.RoleWeb: {"192.0.2.1:8080"}})
	if err := a.bindListeners(); err != nil {
		t.Fatal(err)
	}
	defer a.ln.closeAll()
	if a.ln.candidate != nil || len(a.ln.savedFailed) != 0 {
		t.Errorf("the saved listeners were used with PICACHE_RUN_AS: %v", a.ln.candidate)
	}
	a.finishListenerFiles()
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, config.ListenersNextFile)); err != nil {
		t.Error("listeners.next.json was touched with PICACHE_RUN_AS")
	}
}

// The DNS hint about port 53 (systemd-resolved's stub listener) is only
// given for port 53: a saved listener on another port gets the general
// hint.
func TestBindErrHint(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	addr := pc.LocalAddr().String()
	_, inUse := net.ListenPacket("udp", addr)
	if inUse == nil || !isAddrInUse(inUse) {
		t.Skipf("no address-in-use error: %v", inUse)
	}
	if msg := bindErr("DNS (udp)", addr, inUse).Error(); strings.Contains(msg, "port 53") ||
		!strings.Contains(msg, "used by another program") {
		t.Errorf("another port: %q", msg)
	}
	if msg := bindErr("DNS (tcp)", ":53", inUse).Error(); !strings.Contains(msg, "port 53 is in use") {
		t.Errorf("port 53: %q", msg)
	}
}

package api

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func TestProfileRoutes(t *testing.T) {
	e := newEncEnv(t)
	get := func(query string, https bool, peer string) *httptest.ResponseRecorder {
		r := coreRequest("GET", "/api/v1/dns/profile.mobileconfig?"+query, "")
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: e.session})
		if peer != "" {
			r.RemoteAddr = peer
		}
		if https {
			r.TLS = &tls.ConnectionState{}
		}
		return e.serve(r)
	}
	update := func(fn func(a *settings.All)) {
		t.Helper()
		if _, err := e.set.Update(context.Background(), func(a *settings.All) error { fn(a); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// The 400s in order.
	coreWantError(t, get("protocol=x&dnsClientId=-x", true, ""), 400, "invalid", "protocol")
	coreWantError(t, get("protocol=doh&dnsClientId=-x&ssid=", true, ""), 400, "invalid", "dnsClientId")
	w := get("protocol=doh&ssid=", true, "")
	coreWantError(t, w, 400, "invalid", "ssids")
	if !strings.Contains(w.Body.String(), "at most 16 Wi-Fi names of 1–32 bytes without control characters") {
		t.Fatal(w.Body)
	}
	// Served over HTTPS with the headers.
	w = get("protocol=doh&dnsClientId=TV&ssid=Home&addresses=true", true, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-apple-aspen-config" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="picache-doh-tv.mobileconfig"` ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	if s := strings.Join(profileStrings(t, w.Body.Bytes()), "\n"); !strings.Contains(s, "https://dns.lan:8443/dns-query/tv") ||
		!strings.Contains(s, "192.168.1.2") {
		t.Fatal(s)
	}
	// Plain HTTP only from this machine.
	w = get("protocol=doh", false, "192.168.1.44:5000")
	coreWantError(t, w, 409, "conflict", "")
	if !strings.Contains(w.Body.String(), "download profiles over HTTPS: on plain HTTP a profile could be replaced on the way") {
		t.Fatal(w.Body)
	}
	if w := get("protocol=doh", false, "127.0.0.1:5000"); w.Code != 200 {
		t.Fatalf("loopback: %d %s", w.Code, w.Body)
	}
	// The 409s of the settings, in order.
	e.rt.mu.Lock()
	e.rt.bound = map[string][]string{"dot": {"0.0.0.0:8853"}}
	e.rt.mu.Unlock()
	for _, tc := range []struct {
		query, want string
		setup       func()
	}{
		{"protocol=dot", "Apple devices use DoT on port 853 only; PICACHE_DOT_LISTEN has no listener on port 853", nil},
		{"protocol=dot", "DoT is not serving", func() { e.enc.mu.Lock(); e.enc.st.DoT.Serving = false; e.enc.mu.Unlock() }},
		{"protocol=doh", "DoH is not serving", func() { e.enc.mu.Lock(); e.enc.st.DoH.Serving = false; e.enc.mu.Unlock() }},
		{"protocol=doh", "DoH is not enabled", func() { update(func(a *settings.All) { a.DNS.Encrypted.DoH = false }) }},
		{"protocol=dot", "DoT is not enabled", func() { update(func(a *settings.All) { a.DNS.Encrypted.DoT = false }) }},
		{"protocol=dot", "set dns.encrypted.serverName first", func() { update(func(a *settings.All) { a.DNS.Encrypted.ServerName = "" }) }},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		w := get(tc.query, true, "")
		if w.Code != 409 || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: %d %s", tc.want, w.Code, w.Body)
		}
	}
}

func TestProfileLinks(t *testing.T) {
	e := newEncEnv(t)
	create := func(body string) *httptest.ResponseRecorder {
		return e.do("POST", "/api/v1/dns/profile-links", body, e.session)
	}
	// Without the HTTPS web listener: 409 (after the checks of the options).
	e.rt.mu.Lock()
	e.rt.tlsAddr = ""
	e.rt.mu.Unlock()
	coreWantError(t, create(`{"protocol":"x"}`), 400, "invalid", "protocol")
	w := create(`{"protocol":"doh"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "profile links need the HTTPS web listener (PICACHE_WEB_TLS_LISTEN)") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	e.rt.mu.Lock()
	e.rt.tlsAddr = "[::]:8443"
	e.rt.mu.Unlock()
	w = create(`{"protocol":"doh","dnsClientId":"Kid","ssids":["Home"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var link profileLinkResult
	coreDecode(t, w, &link)
	token, ok := strings.CutPrefix(link.URL, "https://dns.lan:8443/api/v1/dns/profile-links/")
	if !ok || len(token) != 43 || time.Until(link.ExpiresAt) > 15*time.Minute || link.ExpiresAt.Before(time.Now().Add(14*time.Minute)) {
		t.Fatalf("link %+v", link)
	}
	// Public, reusable, HEAD without a body.
	for range 2 {
		r := coreRequest("GET", "/api/v1/dns/profile-links/"+token, "")
		r.Host = "dns.lan:8443"
		r.TLS = &tls.ConnectionState{}
		w := e.serve(r)
		if w.Code != 200 || !strings.Contains(strings.Join(profileStrings(t, w.Body.Bytes()), "\n"), "https://dns.lan:8443/dns-query/kid") {
			t.Fatalf("download: %d %s", w.Code, w.Body)
		}
	}
	r := coreRequest("HEAD", "/api/v1/dns/profile-links/"+token, "")
	if w := e.serve(r); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != profileMediaType {
		t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
	}
	coreWantError(t, e.do("GET", "/api/v1/dns/profile-links/"+strings.Repeat("A", 43), "", ""), 404, "not_found", "")
	// Settings changed since: the 409s of the checks.
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.DNS.Encrypted.DoH = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/v1/dns/profile-links/"+token, "", ""); w.Code != 409 {
		t.Fatalf("after a change: %d", w.Code)
	}
	// The audit names the protocol and the ClientID; neither the audit nor
	// the log contains the token.
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, en := range entries {
		if en.Action == "dns.profile_link.create" {
			found = true
			details := en.Details
			if en.Target != "doh" || !strings.Contains(string(details), `"dnsClientId":"kid"`) || strings.Contains(string(details), token) {
				t.Fatalf("audit %+v %s", en, details)
			}
		}
	}
	if !found {
		t.Fatal("no audit entry")
	}
	if strings.Contains(e.log.String(), token) {
		t.Fatal("the token is in the log")
	}
	lr := httptest.NewRequest("GET", "/api/v1/dns/profile-links/"+token, nil)
	if got := logPath(lr); got != "/api/v1/dns/profile-links/…" {
		t.Fatal(got)
	}
}

// TestProfileLinkStore: 15 minutes, 32 links, 20 downloads a minute per
// client key; a new store knows no link.
func TestProfileLinkStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProfileLinks()
		tok, exp, err := p.add(ProfileOptions{Protocol: "doh"})
		if err != nil || time.Until(exp) != 15*time.Minute {
			t.Fatal(err, exp)
		}
		time.Sleep(15*time.Minute - time.Second)
		if _, ok := p.get(tok); !ok {
			t.Fatal("expired early")
		}
		time.Sleep(time.Second)
		if _, ok := p.get(tok); ok {
			t.Fatal("valid after 15 minutes")
		}
		if _, ok := newProfileLinks().get(tok); ok {
			t.Fatal("a new store knows the link")
		}
		var toks []string
		for range maxProfileLinks + 3 {
			tok, _, _ := p.add(ProfileOptions{Protocol: "dot"})
			toks = append(toks, tok)
			time.Sleep(time.Millisecond)
		}
		if len(p.links) != maxProfileLinks {
			t.Fatalf("%d links", len(p.links))
		}
		if _, ok := p.get(toks[0]); ok {
			t.Fatal("the oldest link was kept")
		}
		if _, ok := p.get(toks[len(toks)-1]); !ok {
			t.Fatal("the newest link was dropped")
		}
		key := netip.MustParsePrefix("192.168.1.9/32")
		for i := range profileLinkRate {
			if !p.allow(key) {
				t.Fatalf("request %d refused", i)
			}
		}
		if p.allow(key) {
			t.Fatal("21st request allowed")
		}
		time.Sleep(time.Minute)
		if !p.allow(key) {
			t.Fatal("not allowed after a minute")
		}
	})
}

func TestEncryptedClientRoutes(t *testing.T) {
	e := newEncEnv(t)
	if _, err := e.reg.CreateClient(context.Background(), clients.ClientInput{Name: "TV", Identifiers: []string{"clientid:tv"}}); err != nil {
		t.Fatal(err)
	}
	e.reg.Seen(netip.MustParseAddr("192.168.1.50"))
	e.reg.SeenDNSClientID(netip.MustParseAddr("192.168.1.50"), "tv")
	w := e.do("GET", "/api/v1/clients/dns-client-ids", "", e.session)
	var ids []clients.SeenDNSClientID
	coreDecode(t, w, &ids)
	if len(ids) != 1 || ids[0].DNSClientID != "tv" || ids[0].Name != "TV" || ids[0].Address != "192.168.1.50" {
		t.Fatalf("%s", w.Body)
	}
	// Blocked clients accept clientid: entries; known rows name them.
	w = e.do("POST", "/api/v1/dns/blocked-clients", `{"client":"ClientID:TV","device":true}`, e.session)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"entry":"clientid:tv","added":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/v1/dns/blocked-clients", `{"client":"clientid:tv"}`, e.session); !strings.Contains(w.Body.String(), `"added":false`) {
		t.Fatalf("duplicate: %s", w.Body)
	}
	w = e.do("POST", "/api/v1/dns/blocked-clients", `{"client":"clientid:-x"}`, e.session)
	coreWantError(t, w, 400, "invalid", "client")
	if !strings.Contains(w.Body.String(), "must be an IP address, CIDR, MAC address or clientid:<ClientID>") {
		t.Fatal(w.Body)
	}
	w = e.do("GET", "/api/v1/clients/known", "", e.session)
	if !strings.Contains(w.Body.String(), `"dnsClientId":"tv"`) || !strings.Contains(w.Body.String(), `"blockedBy":"clientid:tv"`) {
		t.Fatalf("known: %s", w.Body)
	}
	// Lookup with a ClientID.
	w = e.do("POST", "/api/v1/dns/lookup", `{"name":"x.example","clientIp":"192.168.1.60","dnsClientId":"bad_id"}`, e.session)
	coreWantError(t, w, 400, "invalid", "dnsClientId")
	w = e.do("POST", "/api/v1/dns/lookup", `{"name":"x.example","clientIp":"192.168.1.60","dnsClientId":"tv"}`, e.session)
	if !strings.Contains(w.Body.String(), `"status":"dropped"`) || !strings.Contains(w.Body.String(), "blocked client: clientid:tv") {
		t.Fatalf("lookup: %s", w.Body)
	}
	// The TLS routes without any TLS listener.
	w = e.do("POST", "/api/v1/system/tls/local-ca", `{"currentPassword":"`+corePassword+`"}`, e.session)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "no TLS listener: PICACHE_WEB_TLS_LISTEN, PICACHE_DOT_LISTEN and PICACHE_DOH_LISTEN are off") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

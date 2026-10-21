package steam

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SteamIDs of the logon header: universe Public (1), account type
// Individual (1) on the desktop instance (1) or AnonUser (10), account 0.
const (
	steamIDIndividual = 1<<56 | 1<<52 | 1<<32
	steamIDAnonymous  = 1<<56 | 10<<52

	clientPackageVersion = 1771
	// clientOSType is EOSType_LinuxUnknown as the uint32 of client_os_type.
	clientOSType   = 1<<32 + osTypeLinux
	clientLanguage = "english"
)

// LogOn is how to sign in.
type LogOn struct {
	// AccountName, SteamID and RefreshToken from a QR sign-in; an empty
	// RefreshToken signs in anonymously (dedicated servers and free tools
	// only).
	AccountName  string
	SteamID      uint64
	RefreshToken string
	// LoginID tells this client's sessions apart from the account's other
	// sessions (a Steam client on a PC): sessions with the same LoginID
	// end each other. A random value kept with the sign-in.
	LoginID uint32
	// MachineID seeds the machine ID Steam sees (stable per installation,
	// no hardware data).
	MachineID string
	// MachineName is shown by Steam for the session.
	MachineName string
}

// Session is a signed-in connection.
type Session struct {
	*Conn
	SteamID uint64
	CellID  uint32
	// TokenID is the ID of the refresh token the session used.
	TokenID  uint64
	licenses chan []License
}

// License is one license of the account.
type License struct {
	PackageID   uint32
	AccessToken uint64
	Flags       uint32 // ELicenseFlags (0x08 expired)
}

// anonymousPackage is what an anonymous session owns ("Anonymous
// Dedicated Server Comp").
const anonymousPackage = 17906

const logonTimeout = 15 * time.Second

// SignIn logs on. The license list Steam sends right after the logon is
// kept for Licenses.
func (c *Conn) SignIn(ctx context.Context, l LogOn) (*Session, error) {
	anon := l.RefreshToken == ""
	lic, err := c.await(emsgClientLicenseList)
	if err != nil {
		return nil, err
	}
	resp, err := c.await(emsgClientLogOnResponse)
	if err != nil {
		return nil, err
	}
	var e pb
	e.uint(1, protocolVersion)
	e.uint(2, uint64(l.LoginID)) // the legacy field, set like Steam's client
	e.uint(3, 0)                 // cell_id
	if !anon {
		e.uint(5, clientPackageVersion)
	}
	e.string(6, clientLanguage)
	e.uint(7, clientOSType)
	if !anon {
		e.bool(8, true) // should_remember_password
	}
	var ip pb
	ip.fixed32(1, l.LoginID)
	e.bytes(11, ip.b) // obfuscated_private_ip: the LoginID, never the LAN address
	e.bytes(30, machineID(l.MachineID))
	sid := uint64(steamIDAnonymous)
	if !anon {
		sid = l.SteamID
		if sid == 0 {
			sid = steamIDIndividual
		}
		e.string(50, l.AccountName)
		if l.MachineName != "" {
			e.string(96, l.MachineName)
		}
		e.bool(102, true) // supports_rate_limit_response
		e.string(108, l.RefreshToken)
	}
	if err := c.send(emsgClientLogon, header{steamID: sid, jobSource: noJob, jobTarget: noJob}, e.b); err != nil {
		return nil, err
	}
	m, err := c.waitFor(ctx, resp, "logon", logonTimeout)
	if err != nil {
		return nil, err
	}
	res, hb, legacyHB := int32(resultFail), int32(0), int32(0)
	var cell uint32
	var tokenID, supplied uint64
	for f, err := range fields(m.body) {
		if err != nil {
			return nil, err
		}
		switch f.num {
		case 1:
			res = f.int32()
		case 2:
			legacyHB = f.int32()
		case 3:
			hb = f.int32()
		case 7:
			cell = uint32(f.n)
		case 20:
			supplied = f.n
		case 30:
			tokenID = f.n
		}
	}
	if res != resultOK {
		return nil, &Error{Call: "logon", Result: res}
	}
	steamID := m.hdr.steamID
	if steamID == 0 {
		steamID = supplied
	}
	if steamID == 0 {
		return nil, errors.New("steam: the logon answer names no Steam ID")
	}
	c.mu.Lock()
	c.steamID = steamID
	c.sessionID = m.hdr.sessionID
	c.mu.Unlock()
	interval := time.Duration(cmp.Or(max(hb, 0), max(legacyHB, 0), 9)) * time.Second
	go c.heartbeat(interval)
	s := &Session{Conn: c, SteamID: steamID, CellID: cell, TokenID: tokenID, licenses: make(chan []License, 1)}
	if anon {
		s.licenses <- []License{{PackageID: anonymousPackage}}
		return s, nil
	}
	go func() {
		select {
		case lm, ok := <-lic:
			if ok {
				ls, err := decodeLicenses(lm.body)
				if err == nil {
					s.licenses <- ls
				}
			}
		case <-c.done:
		}
	}()
	return s, nil
}

// machineID is the binary KeyValues Steam expects: a MessageObject with
// the three strings BB3, FF2 and 3B3 (SHA-1 hex). They are derived from
// seed, a random value of this installation, not from the hardware.
func machineID(seed string) []byte {
	h := func(k string) string {
		s := sha1.Sum([]byte(k + ":" + seed))
		return hex.EncodeToString(s[:])
	}
	var b []byte
	cs := func(s string) { b = append(append(b, s...), 0) }
	b = append(b, kvbSection)
	cs("MessageObject")
	for _, k := range []string{"BB3", "FF2", "3B3"} {
		b = append(b, kvbString)
		cs(k)
		cs(h(k))
	}
	b = append(b, kvbEnd, kvbEnd)
	return b
}

func decodeLicenses(b []byte) ([]License, error) {
	var out []License
	for f, err := range fields(b) {
		if err != nil {
			return nil, err
		}
		if f.num != 2 || f.wire != wireBytes {
			continue
		}
		var l License
		for g, err := range fields(f.b) {
			if err != nil {
				return nil, err
			}
			switch g.num {
			case 1:
				l.PackageID = uint32(g.n)
			case 7:
				l.Flags = uint32(g.n)
			case 17:
				l.AccessToken = g.n
			}
		}
		out = append(out, l)
	}
	return out, nil
}

// Licenses returns the licenses Steam sent after the logon.
func (s *Session) Licenses(ctx context.Context) ([]License, error) {
	t := time.NewTimer(callTimeout)
	defer t.Stop()
	select {
	case ls := <-s.licenses:
		s.licenses <- ls
		return ls, nil
	case <-s.done:
		return nil, s.connErr()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, errors.New("steam: no license list after the logon")
	}
}

// LogOff ends the session politely and closes the connection.
func (s *Session) LogOff() {
	_ = s.send(emsgClientLogOff, header{jobSource: noJob, jobTarget: noJob}, nil)
	time.Sleep(100 * time.Millisecond)
	s.Close()
}

// Product is the PICS info of one app or package.
type Product struct {
	ID           uint32
	ChangeNumber uint32
	MissingToken bool
	// Info is the parsed KeyValues (apps: the "appinfo" section; packages:
	// the section named after the package).
	Info *kv
}

// ProductRef names an app or package with its access token.
type ProductRef struct {
	ID          uint32
	AccessToken uint64
}

const picsBatch = 100

// AccessTokens asks for the PICS access tokens of apps and packages.
func (s *Session) AccessTokens(ctx context.Context, apps, packages []uint32) (map[uint32]uint64, map[uint32]uint64, error) {
	appTok, pkgTok := map[uint32]uint64{}, map[uint32]uint64{}
	for len(apps) > 0 || len(packages) > 0 {
		na, np := min(len(apps), picsBatch), min(len(packages), picsBatch)
		var e pb
		for _, id := range packages[:np] {
			e.uint(1, uint64(id))
		}
		for _, id := range apps[:na] {
			e.uint(2, uint64(id))
		}
		apps, packages = apps[na:], packages[np:]
		m, err := s.call(ctx, emsgClientPICSAccessTokenRequest, "", e.b, "access tokens")
		if err != nil {
			return nil, nil, err
		}
		for f, err := range fields(m.body) {
			if err != nil {
				return nil, nil, err
			}
			if f.wire != wireBytes || (f.num != 1 && f.num != 3) {
				continue
			}
			var id uint32
			var tok uint64
			for g, err := range fields(f.b) {
				if err != nil {
					return nil, nil, err
				}
				switch g.num {
				case 1:
					id = uint32(g.n)
				case 2:
					tok = g.n
				}
			}
			if f.num == 1 {
				pkgTok[id] = tok
			} else {
				appTok[id] = tok
			}
		}
	}
	return appTok, pkgTok, nil
}

// ProductInfo reads the PICS info of apps and packages.
func (s *Session) ProductInfo(ctx context.Context, apps, packages []ProductRef) (map[uint32]Product, map[uint32]Product, error) {
	appInfo, pkgInfo := map[uint32]Product{}, map[uint32]Product{}
	for len(apps) > 0 || len(packages) > 0 {
		na, np := min(len(apps), picsBatch), min(len(packages), picsBatch)
		var e pb
		for _, p := range packages[:np] {
			var sub pb
			sub.uint(1, uint64(p.ID))
			if p.AccessToken != 0 {
				sub.uint(2, p.AccessToken)
			}
			e.bytes(1, sub.b)
		}
		for _, a := range apps[:na] {
			var sub pb
			sub.uint(1, uint64(a.ID))
			if a.AccessToken != 0 {
				sub.uint(2, a.AccessToken)
			}
			e.bytes(2, sub.b)
		}
		e.bool(3, false) // meta_data_only
		apps, packages = apps[na:], packages[np:]
		if err := s.productInfo(ctx, e.b, appInfo, pkgInfo); err != nil {
			return nil, nil, err
		}
	}
	return appInfo, pkgInfo, nil
}

// productInfo sends one request and reads its answers until none is
// pending.
func (s *Session) productInfo(ctx context.Context, req []byte, apps, pkgs map[uint32]Product) error {
	id := s.nextJob.Add(1)
	ch := make(chan message, 64)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.connErr()
	}
	// Several answers carry the same job ID.
	s.jobs[id] = jobWait{ch: ch, multi: true}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.jobs, id)
		s.mu.Unlock()
	}()
	if err := s.send(emsgClientPICSProductInfoRequest, header{jobSource: id, jobTarget: noJob}, req); err != nil {
		return err
	}
	for {
		m, err := s.wait(ctx, ch, "product info")
		if err != nil {
			return err
		}
		if m.emsg == emsgDestJobFailed {
			return &Error{Call: "product info", Result: resultFail, Message: "the server could not route the call"}
		}
		pending, err := decodeProductInfo(m.body, apps, pkgs)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
	}
}

func decodeProductInfo(b []byte, apps, pkgs map[uint32]Product) (pending bool, err error) {
	for f, err := range fields(b) {
		if err != nil {
			return false, err
		}
		switch f.num {
		case 1, 3:
			if f.wire != wireBytes {
				continue
			}
			var p Product
			var buf []byte
			for g, err := range fields(f.b) {
				if err != nil {
					return false, err
				}
				switch g.num {
				case 1:
					p.ID = uint32(g.n)
				case 2:
					p.ChangeNumber = uint32(g.n)
				case 3:
					p.MissingToken = g.bool()
				case 5:
					buf = g.b
				}
			}
			if f.num == 1 {
				if len(buf) > 0 {
					root, err := parseTextKV(buf)
					if err != nil {
						return false, fmt.Errorf("app %d: %w", p.ID, err)
					}
					p.Info = root.child("appinfo")
				}
				apps[p.ID] = p
			} else {
				if len(buf) > 0 {
					p.Info, err = parsePackageBuffer(p.ID, buf)
					if err != nil {
						return false, fmt.Errorf("package %d: %w", p.ID, err)
					}
				}
				pkgs[p.ID] = p
			}
		case 6:
			pending = f.bool()
		}
	}
	return pending, nil
}

// parsePackageBuffer parses a package's binary KeyValues. The buffer
// starts with the package ID (uint32) before the KeyValues.
func parsePackageBuffer(id uint32, b []byte) (*kv, error) {
	if len(b) >= 4 {
		if root, err := parseBinaryKV(b[4:]); err == nil {
			if n := root.child(fmt.Sprint(id)); n != nil {
				return n, nil
			}
		}
	}
	root, err := parseBinaryKV(b)
	if err != nil {
		return nil, err
	}
	if n := root.child(fmt.Sprint(id)); n != nil {
		return n, nil
	}
	return nil, errKV
}

// ManifestRequestCode asks for the code that a manifest download needs
// (valid for a few minutes).
func (s *Session) ManifestRequestCode(ctx context.Context, app, depot uint32, manifest uint64, branch string) (uint64, error) {
	var e pb
	e.uint(1, uint64(app))
	e.uint(2, uint64(depot))
	e.uint(3, manifest)
	if branch != "" && branch != "public" {
		e.string(4, branch)
	}
	b, err := s.service(ctx, "ContentServerDirectory.GetManifestRequestCode#1", e.b, true)
	if err != nil {
		return 0, err
	}
	for f, err := range fields(b) {
		if err != nil {
			return 0, err
		}
		if f.num == 1 && f.n != 0 {
			return f.n, nil
		}
	}
	return 0, ErrNoAccess
}

// ErrNoAccess: Steam gives no manifest request code for the depot (the
// account may not download it).
var ErrNoAccess = errors.New("steam: no access to the depot (no manifest request code)")

// ContentServer is a Steam content server.
type ContentServer struct {
	Type   string // CDN, SteamCache, …
	Host   string
	VHost  string
	HTTPS  string // https_support: "mandatory" means HTTPS only
	Proxy  bool   // use_as_proxy: not a content server itself
	Load   float32
	AppIDs []uint32 // only these apps when set
}

// ContentServers lists the content servers for the session's cell.
func (s *Session) ContentServers(ctx context.Context) ([]ContentServer, error) {
	var e pb
	e.uint(1, uint64(s.CellID))
	e.uint(2, 20)
	b, err := s.service(ctx, "ContentServerDirectory.GetServersForSteamPipe#1", e.b, true)
	if err != nil {
		return nil, err
	}
	var out []ContentServer
	for f, err := range fields(b) {
		if err != nil {
			return nil, err
		}
		if f.num != 1 || f.wire != wireBytes {
			continue
		}
		var cs ContentServer
		for g, err := range fields(f.b) {
			if err != nil {
				return nil, err
			}
			switch g.num {
			case 1:
				cs.Type = g.str()
			case 5:
				cs.Load = g.float32()
			case 8:
				cs.Host = g.str()
			case 10:
				cs.Proxy = g.bool()
			case 9:
				cs.VHost = g.str()
			case 12:
				cs.HTTPS = g.str()
			case 13:
				if cs.AppIDs, err = appendUint32s(cs.AppIDs, g); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, cs)
	}
	return out, nil
}

func appendUint32s(dst []uint32, f field) ([]uint32, error) {
	vs, err := packedUints(nil, f)
	for _, v := range vs {
		dst = append(dst, uint32(v))
	}
	return dst, err
}

// RevokeRefreshToken revokes the session's refresh token permanently: the
// sign-in disappears from the account's device list.
func (s *Session) RevokeRefreshToken(ctx context.Context, tokenID uint64) error {
	var e pb
	e.fixed64(1, tokenID)
	e.fixed64(2, s.SteamID)
	e.uint(3, 1) // k_EAuthTokenRevokePermanent
	_, err := s.service(ctx, "Authentication.RevokeRefreshToken#1", e.b, true)
	return err
}

// RevokeToken revokes a refresh token permanently (the stored sign-in):
// it disappears from the account's device list.
func (s *Session) RevokeToken(ctx context.Context, token string) error {
	var e pb
	e.string(1, token)
	e.uint(2, 1) // k_EAuthTokenRevokePermanent
	_, err := s.service(ctx, "Authentication.RevokeToken#1", e.b, true)
	return err
}

// TokenRejected reports whether a logon result means that the stored
// token is no longer valid (sign in again), not a passing failure.
func TokenRejected(result int32) bool {
	switch result {
	case resultInvalidPassword, resultAccessDenied, resultRevoked, resultExpired, resultInvalidSignature,
		resultAccountLogonDenied, resultNeedTwoFactor, resultTwoFactorMismatch:
		return true
	}
	return false
}

// Transient reports whether a result means "try another server".
func Transient(result int32) bool {
	switch result {
	case resultFail, resultNoConnection, resultServiceUnavailable, resultTryAnotherCM:
		return true
	}
	return false
}

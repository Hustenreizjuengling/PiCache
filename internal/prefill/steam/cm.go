package steam

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// A connection to a Steam connection manager (CM) over WebSocket
// (wss://<endpoint>/cmsocket/): every binary message is one Steam message,
// EMsg (uint32, little endian, with protoMask set for protobuf messages),
// the length of the header, a CMsgProtoBufHeader, then the body.

const (
	protoMask       = 0x80000000
	protocolVersion = 65581 // MsgClientLogon.CurrentProtocol

	emsgMulti                                = 1
	emsgDestJobFailed                        = 113
	emsgJobHeartbeat                         = 123
	emsgServiceMethod                        = 146
	emsgServiceMethodResponse                = 147
	emsgServiceMethodCallFromClient          = 151
	emsgClientHeartBeat                      = 703
	emsgClientLogOff                         = 706
	emsgClientLogOnResponse                  = 751
	emsgClientLoggedOff                      = 757
	emsgClientLicenseList                    = 780
	emsgClientServerUnavailable              = 5500
	emsgClientLogon                          = 5514
	emsgClientPICSProductInfoRequest         = 8903
	emsgClientPICSProductInfoResponse        = 8904
	emsgClientPICSAccessTokenRequest         = 8905
	emsgClientPICSAccessTokenResponse        = 8906
	emsgServiceMethodCallFromClientNonAuthed = 9804
	emsgClientHello                          = 9805

	noJob = math.MaxUint64

	maxMessage   = 16 << 20 // one WebSocket message
	maxUnzipped  = 64 << 20 // the content of one Multi
	callTimeout  = 30 * time.Second
	dialTimeout  = 15 * time.Second
	writeTimeout = 15 * time.Second
)

// EResult values the client looks at (enums.proto EResult).
const (
	resultOK                 = 1
	resultFail               = 2
	resultNoConnection       = 3
	resultInvalidPassword    = 5
	resultAccessDenied       = 15
	resultServiceUnavailable = 20
	resultRevoked            = 26
	resultExpired            = 27
	resultTryAnotherCM       = 48
	resultAccountLogonDenied = 63
	resultNeedTwoFactor      = 85
	resultTwoFactorMismatch  = 88
	resultRateLimit          = 84
	resultLoginThrottle      = 87
	resultInvalidSignature   = 121
)

// Error is a failed Steam call.
type Error struct {
	Call    string
	Result  int32
	Message string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("steam %s: %s", e.Call, resultName(e.Result))
	if e.Message != "" {
		s += " (" + e.Message + ")"
	}
	return s
}

// resultName names the EResults that matter to a user.
func resultName(r int32) string {
	switch r {
	case resultOK:
		return "OK"
	case resultFail:
		return "failed"
	case resultNoConnection:
		return "no connection"
	case resultInvalidPassword:
		return "the sign-in is no longer valid"
	case resultAccessDenied:
		return "access denied"
	case resultServiceUnavailable:
		return "service unavailable"
	case resultRevoked:
		return "the sign-in was revoked"
	case resultExpired:
		return "expired"
	case resultTryAnotherCM:
		return "try another server"
	case resultRateLimit, resultLoginThrottle:
		return "too many sign-ins: Steam asks to wait"
	case resultInvalidSignature, resultAccountLogonDenied, resultNeedTwoFactor, resultTwoFactorMismatch:
		return "the sign-in is no longer valid"
	}
	return fmt.Sprintf("EResult %d", r)
}

// ErrClosed is returned by calls on a closed connection.
var ErrClosed = errors.New("steam: connection closed")

// header is the part of CMsgProtoBufHeader the client uses.
type header struct {
	steamID       uint64
	sessionID     int32
	jobSource     uint64
	jobTarget     uint64
	targetJobName string
	eresult       int32
	errorMessage  string
}

func (h header) encode() []byte {
	var e pb
	if h.steamID != 0 {
		e.fixed64(1, h.steamID)
	}
	if h.sessionID != 0 {
		e.int32(2, h.sessionID)
	}
	if h.jobSource != noJob {
		e.fixed64(10, h.jobSource)
	}
	if h.jobTarget != noJob {
		e.fixed64(11, h.jobTarget)
	}
	if h.targetJobName != "" {
		e.string(12, h.targetJobName)
	}
	return e.b
}

func decodeHeader(b []byte) (header, error) {
	h := header{jobSource: noJob, jobTarget: noJob, eresult: resultFail}
	for f, err := range fields(b) {
		if err != nil {
			return h, err
		}
		switch f.num {
		case 1:
			h.steamID = f.n
		case 2:
			h.sessionID = f.int32()
		case 10:
			h.jobSource = f.n
		case 11:
			h.jobTarget = f.n
		case 12:
			h.targetJobName = f.str()
		case 13:
			h.eresult = f.int32()
		case 14:
			h.errorMessage = f.str()
		}
	}
	return h, nil
}

// jobWait waits for the answers to a job; a multi job gets several (PICS
// product info) and is removed by its caller.
type jobWait struct {
	ch    chan message
	multi bool
}

// message is one received message.
type message struct {
	emsg uint32
	hdr  header
	body []byte
}

func parseMessage(b []byte) (message, bool, error) {
	if len(b) < 8 {
		return message{}, false, errWire
	}
	raw := binary.LittleEndian.Uint32(b)
	if raw&protoMask == 0 {
		// A message with the old binary header: not used by the calls of
		// this client.
		return message{emsg: raw}, false, nil
	}
	hl := binary.LittleEndian.Uint32(b[4:])
	if uint64(hl) > uint64(len(b)-8) {
		return message{}, false, errWire
	}
	h, err := decodeHeader(b[8 : 8+hl])
	if err != nil {
		return message{}, false, err
	}
	return message{emsg: raw &^ protoMask, hdr: h, body: b[8+hl:]}, true, nil
}

// Options configure a connection.
type Options struct {
	// DirectoryURL is the CM list (ISteamDirectory/GetCMListForConnect).
	DirectoryURL string
	// DialContext, if set, opens every TCP connection (the directory's
	// HTTPS and the CM's WebSocket); tests and pinned addresses use it.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLS, if set, is the TLS configuration (tests: their CA).
	TLS *tls.Config
	// Plain uses ws:// instead of wss:// (tests only).
	Plain bool
	// Used is told about every host contacted and why.
	Used func(host, protocol, purpose string)
	Log  *slog.Logger
}

func (o *Options) used(host, protocol, purpose string) {
	if o.Used != nil {
		o.Used(host, protocol, purpose)
	}
}

func (o *Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o *Options) httpClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       o.TLS,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
	}
	if o.DialContext != nil {
		tr.DialContext = o.DialContext
	} else {
		tr.DialContext = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// Conn is a connection to a CM.
type Conn struct {
	opt  Options
	ws   *websocket.Conn
	host string

	wmu sync.Mutex // one writer at a time

	mu        sync.Mutex
	jobs      map[uint64]jobWait
	waiters   map[uint32][]chan message
	steamID   uint64
	sessionID int32
	closed    bool
	closeErr  error
	done      chan struct{}

	nextJob atomic.Uint64
	stopHB  chan struct{}
}

// Connect picks a CM from the directory and connects to it, trying up to
// three servers.
func Connect(ctx context.Context, opt Options) (*Conn, error) {
	servers, err := cmServers(ctx, &opt)
	if err != nil {
		return nil, err
	}
	var last error
	for i, s := range servers {
		if i == 3 {
			break
		}
		c, err := dialCM(ctx, opt, s)
		if err == nil {
			return c, nil
		}
		last = err
		opt.log().Debug("steam CM connection failed", "cm", s, "err", err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("no Steam server answered: %w", last)
}

func dialCM(ctx context.Context, opt Options, endpoint string) (*Conn, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("CM endpoint %q: %w", endpoint, err)
	}
	scheme := "wss"
	if opt.Plain {
		scheme = "ws"
	}
	loc := &url.URL{Scheme: scheme, Host: endpoint, Path: "/cmsocket/"}
	cfg, err := websocket.NewConfig(loc.String(), "https://"+host)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	opt.used(host, scheme, "steam-cm")
	var ws *websocket.Conn
	if opt.DialContext == nil {
		cfg.Dialer = &net.Dialer{Timeout: dialTimeout}
		cfg.TlsConfig = tlsFor(opt.TLS, host)
		ws, err = cfg.DialContext(dctx)
	} else {
		var nc net.Conn
		nc, err = opt.DialContext(dctx, "tcp", endpoint)
		if err == nil && !opt.Plain {
			tc := tls.Client(nc, tlsFor(opt.TLS, host))
			if err = tc.HandshakeContext(dctx); err != nil {
				nc.Close()
			}
			nc = tc
		}
		if err == nil {
			_ = nc.SetDeadline(time.Now().Add(dialTimeout))
			ws, err = websocket.NewClient(cfg, nc)
			if err != nil {
				nc.Close()
			} else {
				_ = nc.SetDeadline(time.Time{})
			}
		}
	}
	if err != nil {
		return nil, err
	}
	ws.PayloadType = websocket.BinaryFrame
	ws.MaxPayloadBytes = maxMessage
	c := &Conn{
		opt: opt, ws: ws, host: host,
		jobs:    map[uint64]jobWait{},
		waiters: map[uint32][]chan message{},
		done:    make(chan struct{}),
		stopHB:  make(chan struct{}),
	}
	c.nextJob.Store(uint64(time.Now().UnixNano()) & 0x0000FFFFFFFFFFFF)
	go c.readLoop()
	var hello pb
	hello.uint(1, protocolVersion)
	if err := c.send(emsgClientHello, header{jobSource: noJob, jobTarget: noJob}, hello.b); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func tlsFor(base *tls.Config, host string) *tls.Config {
	var c *tls.Config
	if base != nil {
		c = base.Clone()
	} else {
		c = &tls.Config{}
	}
	if c.ServerName == "" {
		c.ServerName = host
	}
	c.MinVersion = tls.VersionTLS12
	return c
}

// Host is the CM's host name.
func (c *Conn) Host() string { return c.host }

// Close ends the connection.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	return nil
}

// Done is closed when the connection ends; Err says why.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	jobs := c.jobs
	c.jobs = map[uint64]jobWait{}
	c.waiters = map[uint32][]chan message{}
	c.mu.Unlock()
	close(c.done)
	close(c.stopHB)
	_ = c.ws.Close()
	for _, j := range jobs {
		close(j.ch)
	}
}

func (c *Conn) send(emsg uint32, h header, body []byte) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if h.steamID == 0 {
		h.steamID = c.steamID
	}
	if h.sessionID == 0 {
		h.sessionID = c.sessionID
	}
	c.mu.Unlock()
	hb := h.encode()
	b := make([]byte, 8, 8+len(hb)+len(body))
	binary.LittleEndian.PutUint32(b, emsg|protoMask)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(hb)))
	b = append(append(b, hb...), body...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err := c.ws.Write(b); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

func (c *Conn) readLoop() {
	for {
		var b []byte
		if err := websocket.Message.Receive(c.ws, &b); err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("steam: the server closed the connection")
			}
			c.fail(err)
			return
		}
		if err := c.dispatch(b, 0); err != nil {
			c.opt.log().Debug("steam message dropped", "err", err)
		}
	}
}

func (c *Conn) dispatch(b []byte, depth int) error {
	m, proto, err := parseMessage(b)
	if err != nil {
		return err
	}
	if !proto {
		if m.emsg == emsgClientServerUnavailable {
			c.fail(&Error{Call: "session", Result: resultServiceUnavailable})
		}
		return nil
	}
	if m.emsg == emsgMulti {
		if depth > 0 {
			return errors.New("nested Multi")
		}
		return c.unpackMulti(m.body)
	}
	switch m.emsg {
	case emsgClientLoggedOff, emsgClientServerUnavailable:
		res := int32(resultServiceUnavailable)
		if m.emsg == emsgClientLoggedOff {
			res = decodeLoggedOff(m.body)
		}
		c.fail(&Error{Call: "session", Result: res})
		return nil
	}
	c.mu.Lock()
	if m.hdr.jobTarget != noJob {
		if j, ok := c.jobs[m.hdr.jobTarget]; ok {
			// A JobHeartbeat says the server still works on the job.
			if !j.multi && m.emsg != emsgJobHeartbeat {
				delete(c.jobs, m.hdr.jobTarget)
			}
			c.mu.Unlock()
			j.ch <- m
			return nil
		}
	}
	ws := c.waiters[m.emsg]
	delete(c.waiters, m.emsg)
	c.mu.Unlock()
	for _, ch := range ws {
		ch <- m
	}
	return nil
}

func decodeLoggedOff(b []byte) int32 {
	r := int32(resultFail)
	for f, err := range fields(b) {
		if err != nil {
			break
		}
		if f.num == 1 {
			r = f.int32()
		}
	}
	return r
}

// unpackMulti dispatches the messages of a CMsgMulti: gzip when
// size_unzipped is set, then (uint32 length, message) pairs.
func (c *Conn) unpackMulti(body []byte) error {
	var unzipped uint64
	var payload []byte
	for f, err := range fields(body) {
		if err != nil {
			return err
		}
		switch f.num {
		case 1:
			unzipped = f.n
		case 2:
			payload = f.b
		}
	}
	if unzipped > 0 {
		if unzipped > maxUnzipped {
			return errors.New("Multi too large")
		}
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(zr, maxUnzipped+1))
		if err != nil {
			return err
		}
		if len(b) > maxUnzipped {
			return errors.New("Multi too large")
		}
		payload = b
	}
	for len(payload) > 0 {
		if len(payload) < 4 {
			return errWire
		}
		n := binary.LittleEndian.Uint32(payload)
		if uint64(n) > uint64(len(payload)-4) {
			return errWire
		}
		if err := c.dispatch(payload[4:4+n], 1); err != nil {
			return err
		}
		payload = payload[4+n:]
	}
	return nil
}

// await registers for the next message of emsg (one that answers no job).
func (c *Conn) await(emsg uint32) (chan message, error) {
	ch := make(chan message, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, c.closeErr
	}
	c.waiters[emsg] = append(c.waiters[emsg], ch)
	return ch, nil
}

// wait waits for the next answer on ch; a JobHeartbeat restarts the
// timeout.
func (c *Conn) wait(ctx context.Context, ch chan message, what string) (message, error) {
	return c.waitFor(ctx, ch, what, callTimeout)
}

func (c *Conn) waitFor(ctx context.Context, ch chan message, what string, timeout time.Duration) (message, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return message{}, c.connErr()
			}
			if m.emsg == emsgJobHeartbeat {
				t.Reset(timeout)
				continue
			}
			return m, nil
		case <-c.done:
			return message{}, c.connErr()
		case <-ctx.Done():
			return message{}, ctx.Err()
		case <-t.C:
			return message{}, fmt.Errorf("steam %s: no answer within %s", what, timeout)
		}
	}
}

func (c *Conn) connErr() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// call sends a message as a job and waits for the answer to it.
func (c *Conn) call(ctx context.Context, emsg uint32, name string, body []byte, what string) (message, error) {
	id := c.nextJob.Add(1)
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return message{}, c.connErr()
	}
	c.jobs[id] = jobWait{ch: ch}
	c.mu.Unlock()
	if err := c.send(emsg, header{jobSource: id, jobTarget: noJob, targetJobName: name}, body); err != nil {
		return message{}, err
	}
	m, err := c.wait(ctx, ch, what)
	if err != nil {
		c.mu.Lock()
		delete(c.jobs, id)
		c.mu.Unlock()
		return m, err
	}
	if m.emsg == emsgDestJobFailed {
		return m, &Error{Call: what, Result: resultFail, Message: "the server could not route the call"}
	}
	return m, nil
}

// service calls a unified service method ("Authentication.BeginAuthSessionViaQR#1").
// Before the sign-in, authed is false.
func (c *Conn) service(ctx context.Context, method string, body []byte, authed bool) ([]byte, error) {
	emsg := uint32(emsgServiceMethodCallFromClientNonAuthed)
	if authed {
		emsg = emsgServiceMethodCallFromClient
	}
	m, err := c.call(ctx, emsg, method, body, method)
	if err != nil {
		return nil, err
	}
	if m.hdr.eresult != resultOK {
		return nil, &Error{Call: method, Result: m.hdr.eresult, Message: m.hdr.errorMessage}
	}
	return m.body, nil
}

// heartbeat sends a heartbeat every interval until the connection ends.
func (c *Conn) heartbeat(interval time.Duration) {
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.stopHB:
			return
		case <-t.C:
			if err := c.send(emsgClientHeartBeat, header{jobSource: noJob, jobTarget: noJob}, nil); err != nil {
				return
			}
		}
	}
}

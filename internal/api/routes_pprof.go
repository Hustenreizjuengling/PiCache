package api

import (
	"io"
	"net/http"
	"runtime/pprof"
	"slices"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// The Go profiles (PICACHE_PPROF, docs/DEPLOYMENT.md "Profiling"): heap,
// allocs and goroutine in the protobuf format and a CPU profile of 1–30 s,
// through runtime/pprof directly (net/http/pprof is never imported: its
// init registers on the default mux of net/http). Outside /api/v1 behind the full
// middleware; permission A (an admin session or an admin token, so curl
// with a bearer token works) and only for an effective client that is
// loopback or one of this machine's addresses (profile from elsewhere
// through ssh -L). Every other client, and every client while the switch
// is off, gets 404. One profile at a time (429).

// pprofProfiles are the profiles served (nothing else: no trace, cmdline
// or symbol).
var pprofProfiles = []string{"heap", "allocs", "goroutine"}

func (s *Server) registerPProfRoutes() {
	gate := routeGate(s.pprofAllowed)
	s.route("GET /debug/pprof/{$}", permAdmin, s.pprofIndex, gate)
	for _, name := range pprofProfiles {
		s.route("GET /debug/pprof/"+name, permAdmin, s.pprofProfile(name), gate)
	}
	s.route("GET /debug/pprof/profile", permAdmin, s.pprofCPU, gate)
}

// pprofAllowed reports whether the profiles are on and the effective
// client is loopback or this machine.
func (s *Server) pprofAllowed(r *http.Request) bool {
	if s.d.Config == nil || !s.d.Config.PProf {
		return false
	}
	ip := netutil.AddrFromRemote(r.RemoteAddr)
	return ip.IsValid() && (ip.IsLoopback() || slices.Contains(s.ownAddrs(), ip))
}

func (s *Server) pprofIndex(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, err := io.WriteString(w, "PiCache profiles (go tool pprof):\n"+
		"  /debug/pprof/heap\n  /debug/pprof/allocs\n  /debug/pprof/goroutine\n  /debug/pprof/profile?seconds=10 (CPU, 1-30 s)\n")
	return err
}

// pprofBegin takes the one profile slot (429 while taken).
func (s *Server) pprofBegin() (func(), error) {
	if !s.profiling.CompareAndSwap(false, true) {
		return nil, apperr.TooMany("another profile is running")
	}
	return func() { s.profiling.Store(false) }, nil
}

func (s *Server) pprofProfile(name string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		done, err := s.pprofBegin()
		if err != nil {
			return err
		}
		defer done()
		p := pprof.Lookup(name)
		if p == nil {
			return apperr.NotFound("profile", name)
		}
		s.audit(r, "system.pprof", "", map[string]any{"profile": name, "seconds": 0})
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pb.gz"`)
		if err := p.WriteTo(w, 0); err != nil {
			return errAlreadyWritten{err}
		}
		return nil
	}
}

func (s *Server) pprofCPU(w http.ResponseWriter, r *http.Request) error {
	secs := 10
	if v := r.URL.Query().Get("seconds"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			return apperr.Invalid("seconds", "must be between 1 and 30")
		}
		secs = n
	}
	done, err := s.pprofBegin()
	if err != nil {
		return err
	}
	defer done()
	extendDeadlines(w, time.Duration(secs)*time.Second+10*time.Second)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="profile.pb.gz"`)
	if err := pprof.StartCPUProfile(w); err != nil {
		return apperr.Conflict("a CPU profile is already running")
	}
	s.audit(r, "system.pprof", "", map[string]any{"profile": "profile", "seconds": secs})
	t := time.NewTimer(time.Duration(secs) * time.Second)
	select {
	case <-t.C:
	case <-r.Context().Done():
		t.Stop()
	}
	pprof.StopCPUProfile()
	return nil
}

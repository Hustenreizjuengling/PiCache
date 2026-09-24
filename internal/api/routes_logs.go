package api

import (
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// Default time ranges of the logs endpoints (docs/API.md).
const (
	logsQueryRange     = time.Hour
	logsStatsRange     = 24 * time.Hour
	logsEventRange     = 24 * time.Hour
	logsDownloadsRange = 7 * 24 * time.Hour
	logsMaxStepSec     = 400 * 24 * 3600
)

// registerLogsRoutes registers the logs endpoints (docs/API.md).
func (s *Server) registerLogsRoutes() {
	s.route("GET /api/v1/logs/queries", permRead, s.logsQueries)
	s.route("GET /api/v1/stats/summary", permRead, s.logsSummary)
	s.route("GET /api/v1/stats/dns", permRead, s.logsDNSSeries)
	s.route("GET /api/v1/stats/cache", permRead, s.logsCacheSeries)
	s.route("GET /api/v1/stats/top", permRead, s.logsTop)
	s.route("GET /api/v1/stats/services", permRead, s.logsServiceStats)
	s.route("GET /api/v1/stats/clients", permRead, s.logsClientStats)
	s.route("GET /api/v1/cache/downloads", permRead, s.logsDownloads)
	s.route("GET /api/v1/cache/requests", permRead, s.logsCacheRequests)
	s.route("GET /api/v1/cache/sni-events", permRead, s.logsSNIEvents)
	s.route("GET /api/v1/cache/evictions", permRead, s.logsEvictions)
	s.route("GET /api/v1/stream/queries", permRead, s.logsStreamQueries)
	s.route("GET /api/v1/stream/cache", permRead, s.logsStreamCache)
}

func (s *Server) logsQueries(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsQueryRange)
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", 0)
	if err != nil {
		return err
	}
	page, err := s.d.Logs.QueryLog(r.Context(), logs.QueryFilter{
		From: from, To: to,
		Client:   qString(r, "client"),
		Domain:   qString(r, "domain"),
		Status:   qList(r, "status"),
		QType:    qString(r, "qtype"),
		Upstream: qString(r, "upstream"),
		Cursor:   qString(r, "cursor"),
		Limit:    limit,
	})
	if err != nil {
		return err
	}
	return ok(w, page)
}

func (s *Server) logsSummary(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	sum, err := s.d.Logs.Summary(r.Context(), from, to)
	if err != nil {
		return err
	}
	return ok(w, sum)
}

// logsStep reads the optional step (seconds); 0 lets the store choose at
// most 300 points at the rollup resolution.
func logsStep(r *http.Request) (time.Duration, error) {
	step, err := qInt(r, "step", 0)
	if err != nil {
		return 0, err
	}
	if step < 0 || step > logsMaxStepSec {
		return 0, apperr.Invalid("step", "must be between 1 and %d seconds", logsMaxStepSec)
	}
	return time.Duration(step) * time.Second, nil
}

func (s *Server) logsDNSSeries(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	step, err := logsStep(r)
	if err != nil {
		return err
	}
	ser, err := s.d.Logs.DNSSeries(r.Context(), from, to, step)
	if err != nil {
		return err
	}
	return ok(w, ser)
}

func (s *Server) logsCacheSeries(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	step, err := logsStep(r)
	if err != nil {
		return err
	}
	ser, err := s.d.Logs.CacheSeries(r.Context(), from, to, step, qString(r, "service"))
	if err != nil {
		return err
	}
	return ok(w, ser)
}

func (s *Server) logsTop(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", 0)
	if err != nil {
		return err
	}
	kind := logs.TopKind(qString(r, "kind"))
	if kind == "" {
		return apperr.Invalid("kind", "required")
	}
	items, err := s.d.Logs.Top(r.Context(), kind, from, to, limit)
	if err != nil {
		return err
	}
	return ok(w, items)
}

func (s *Server) logsServiceStats(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	stats, err := s.d.Logs.ServiceStats(r.Context(), from, to)
	if err != nil {
		return err
	}
	return ok(w, stats)
}

func (s *Server) logsClientStats(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsStatsRange)
	if err != nil {
		return err
	}
	stats, err := s.d.Logs.ClientStats(r.Context(), from, to)
	if err != nil {
		return err
	}
	return ok(w, stats)
}

func (s *Server) logsDownloads(w http.ResponseWriter, r *http.Request) error {
	from, to, err := qRange(r, logsDownloadsRange)
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", 0)
	if err != nil {
		return err
	}
	offset, err := qInt(r, "offset", 0)
	if err != nil {
		return err
	}
	page, err := s.d.Logs.Downloads(r.Context(), logs.DownloadFilter{
		From: from, To: to,
		Client:     qString(r, "client"),
		Service:    qString(r, "service"),
		GroupKey:   qString(r, "group"),
		Search:     qString(r, "search"),
		ActiveOnly: qBool(r, "active"),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		return err
	}
	return ok(w, page)
}

// logsEventFilter reads the parameters shared by the raw event lists.
func logsEventFilter(r *http.Request) (logs.EventFilter, error) {
	from, to, err := qRange(r, logsEventRange)
	if err != nil {
		return logs.EventFilter{}, err
	}
	limit, err := qInt(r, "limit", 0)
	if err != nil {
		return logs.EventFilter{}, err
	}
	return logs.EventFilter{
		From: from, To: to,
		Client:  qString(r, "client"),
		Service: qString(r, "service"),
		Search:  qString(r, "search"),
		Status:  qString(r, "status"),
		Cursor:  qString(r, "cursor"),
		Limit:   limit,
	}, nil
}

func (s *Server) logsCacheRequests(w http.ResponseWriter, r *http.Request) error {
	f, err := logsEventFilter(r)
	if err != nil {
		return err
	}
	page, err := s.d.Logs.CacheRequests(r.Context(), f)
	if err != nil {
		return err
	}
	return ok(w, page)
}

func (s *Server) logsSNIEvents(w http.ResponseWriter, r *http.Request) error {
	f, err := logsEventFilter(r)
	if err != nil {
		return err
	}
	page, err := s.d.Logs.SNIEvents(r.Context(), f)
	if err != nil {
		return err
	}
	return ok(w, page)
}

func (s *Server) logsEvictions(w http.ResponseWriter, r *http.Request) error {
	f, err := logsEventFilter(r)
	if err != nil {
		return err
	}
	page, err := s.d.Logs.Evictions(r.Context(), f)
	if err != nil {
		return err
	}
	return ok(w, page)
}

// logsAlive re-validates the principal of a stream (checked every 15 s).
func (s *Server) logsAlive(r *http.Request) func() bool {
	return func() bool { return s.d.Auth.Valid(r.Context(), principal(r)) }
}

func (s *Server) logsStreamQueries(w http.ResponseWriter, r *http.Request) error {
	match, err := logs.QueryMatcher(qString(r, "client"), qList(r, "status"))
	if err != nil {
		return err
	}
	ch, cancel, err := s.d.Logs.SubscribeQueries(match)
	if err != nil {
		return err // 429 beyond the subscriber limit, 503 when unavailable
	}
	defer cancel()
	return sse(w, r, "query", ch, s.logsAlive(r))
}

func (s *Server) logsStreamCache(w http.ResponseWriter, r *http.Request) error {
	ch, cancel, err := s.d.Logs.SubscribeCache(nil)
	if err != nil {
		return err
	}
	defer cancel()
	return sse(w, r, "request", ch, s.logsAlive(r))
}

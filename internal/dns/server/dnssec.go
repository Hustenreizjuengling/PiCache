package dnsserver

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
)

// DNSSEC statuses of the verdicts (the dnssec package's; logs.QueryEvent
// .dnssecStatus).
const (
	dnssecSecure        = "secure"
	dnssecInsecure      = "insecure"
	dnssecBogus         = "bogus"
	dnssecIndeterminate = "indeterminate"
)

// maxDNSSECReason bounds the reason of a bogus SERVFAIL ("dnssec: <zone>:
// <failure>") in the query log.
const maxDNSSECReason = 200

// bogusError is the result of a fetch whose verdict is bogus for a client
// with CD=0 (step 13v).
type bogusError struct{ v *upstream.Verdict }

func (e *bogusError) Error() string { return bogusReason(e.v) }

// bogusReason is the reason of a bogus SERVFAIL: "dnssec: <zone>:
// <failure>" (at most 200 bytes, without control or bidi characters).
func bogusReason(v *upstream.Verdict) string {
	s := "dnssec: " + v.Reason
	if v.Zone != "" {
		s = "dnssec: " + v.Zone + ": " + v.Reason
	}
	s = upstream.SanitizeEDEText(s)
	if len(s) > maxDNSSECReason {
		n := maxDNSSECReason
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}

// bogusServfail is step 13v: SERVFAIL with status error, the reason
// "dnssec: <zone>: <failure>" and the verdict's EDE for EDNS clients.
func (s *Server) bogusServfail(qc *qctx, v *upstream.Verdict, upstreamName string) result {
	if qc.tracing() {
		qc.note("dnssec: bogus (" + strings.TrimPrefix(bogusReason(v), "dnssec: ") + ")")
	}
	r := s.servfail(qc, bogusReason(v))
	r.upstream, r.dnssec, r.dnssecFail = upstreamName, v, true
	return r
}

// verdictRank orders the statuses: the worst verdict of a combined answer
// is logged (bogus > indeterminate > insecure > secure).
func verdictRank(v *upstream.Verdict) int {
	if v == nil {
		return -1
	}
	switch v.Status {
	case dnssecBogus:
		return 3
	case dnssecIndeterminate:
		return 2
	case dnssecInsecure:
		return 1
	}
	return 0
}

// worstVerdict returns the worse of two verdicts (nil: not validated).
func worstVerdict(a, b *upstream.Verdict) *upstream.Verdict {
	if verdictRank(b) > verdictRank(a) {
		return b
	}
	return a
}

// traceDNSSEC notes the verdict of a fetch in the Lookup trace;
// notValidated marks a route that is not validated (reported in the
// DNSSEC mode validate).
func (s *Server) traceDNSSEC(qc *qctx, v *upstream.Verdict, notValidated bool, what string) {
	if !qc.tracing() {
		return
	}
	if v == nil {
		if notValidated && qc.set.DNS.Validating() {
			qc.note("dnssec: not validated (" + what + ")")
		}
		return
	}
	switch v.Status {
	case dnssecSecure:
		qc.note("dnssec: secure (" + v.Zone + ")")
	case dnssecInsecure:
		qc.note("dnssec: insecure (" + v.Zone + ": " + v.Reason + ")")
	case dnssecBogus:
		qc.note("dnssec: bogus (" + v.Zone + ": " + v.Reason + ")")
		qc.note("dnssec: bogus answer passed on (checking disabled)")
	case dnssecIndeterminate:
		qc.note("dnssec: indeterminate (" + v.Reason + ")")
	}
}

// DNSSECCounts are the answered queries by DNSSEC status since the start
// (Stats.dnssec, picache_dns_dnssec_total).
type DNSSECCounts struct {
	Secure        int64 `json:"secure"`
	Insecure      int64 `json:"insecure"`
	Bogus         int64 `json:"bogus"`
	Indeterminate int64 `json:"indeterminate"`
}

// dnssecStats counts the answered queries with a DNSSEC status: in total
// and in one-minute buckets of the last hour (the health check).
type dnssecStats struct {
	secure, insecure, bogus, indeterminate atomic.Int64

	mu      sync.Mutex
	buckets [60]minuteCount
}

type minuteCount struct {
	minute       int64 // unix minute of the bucket
	total, bogus int64
}

// count records the verdict of an answered query (nil: none).
func (d *dnssecStats) count(v *upstream.Verdict, now time.Time) {
	if v == nil {
		return
	}
	switch v.Status {
	case dnssecSecure:
		d.secure.Add(1)
	case dnssecInsecure:
		d.insecure.Add(1)
	case dnssecBogus:
		d.bogus.Add(1)
	case dnssecIndeterminate:
		d.indeterminate.Add(1)
	default:
		return
	}
	m := now.Unix() / 60
	d.mu.Lock()
	b := &d.buckets[m%60]
	if b.minute != m {
		*b = minuteCount{minute: m}
	}
	b.total++
	if v.Status == dnssecBogus {
		b.bogus++
	}
	d.mu.Unlock()
}

func (d *dnssecStats) totals() DNSSECCounts {
	return DNSSECCounts{Secure: d.secure.Load(), Insecure: d.insecure.Load(), Bogus: d.bogus.Load(),
		Indeterminate: d.indeterminate.Load()}
}

// lastHour returns the bogus and all validated answers of the last hour.
func (d *dnssecStats) lastHour(now time.Time) (bogus, total int64) {
	m := now.Unix() / 60
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.buckets {
		if b.minute > m-60 && b.minute <= m {
			bogus += b.bogus
			total += b.total
		}
	}
	return bogus, total
}

// DNSSECLastHour returns the answered queries of the last hour that failed
// DNSSEC validation and those with any DNSSEC status (the health check
// "dnssec"; in-memory, never filtered).
func (s *Server) DNSSECLastHour() (bogus, validated int64) { return s.dnssec.lastHour(time.Now()) }

package dnsserver

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	lookupTimeout    = 15 * time.Second
	maxLookupAnswers = 64
)

// Lookup runs the pipeline for a test query without sending the reply
// anywhere (does not log to the query log). The rate limit is not applied
// and the client is not marked as seen.
func (s *Server) Lookup(ctx context.Context, req LookupRequest, caller netip.Addr) (LookupResult, error) {
	name := normalizeName(req.Name)
	if !validDomain(name) {
		return LookupResult{}, apperr.Invalid("name", "must be a valid domain name")
	}
	typ := strings.ToUpper(strings.TrimSpace(req.Type))
	if typ == "" {
		typ = "A"
	}
	qtype, ok := dns.StringToType[typ]
	if !ok {
		return LookupResult{}, apperr.Invalid("type", "unknown record type %q", req.Type)
	}
	client := netutil.Canon(caller)
	if v := strings.TrimSpace(req.ClientIP); v != "" {
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return LookupResult{}, apperr.Invalid("clientIp", "must be an IP address")
		}
		client = netutil.Canon(ip)
	}
	if !client.IsValid() {
		return LookupResult{}, apperr.Invalid("clientIp", "must be an IP address")
	}

	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	msg := new(dns.Msg)
	msg.SetQuestion(fqdn(name), qtype)
	set := s.d.Settings.Get()
	steps := []string{}
	qc := newQuery(ctx, msg, client, "lookup", set)
	qc.steps = &steps
	qc.id = s.identify(client)
	s.scope(qc)
	s.traceClient(qc)

	var res result
	srcEntry, srcBlocked := s.blockedSource(client)
	idEntry, idBlocked := s.blockedIdentity(qc)
	switch rcode, reason := validate(msg); {
	case s.healthProbe(msg, client):
		qc.note("health probe from this machine: answered like localhost, never counted or logged")
		res = s.addrAnswer(qc, localhostV4, localhostV6)
	case srcBlocked:
		qc.note("blocked client: " + srcEntry)
		res = result{drop: true, status: StatusDropped, reason: srcEntry}
	case rcode >= 0:
		qc.note("refused: " + reason)
		res = s.refusal(qc, rcode, reason)
	case set.DNS.RefuseANY && qtype == dns.TypeANY:
		qc.note("refused: ANY queries are refused (dns.refuseAny)")
		res = s.refusal(qc, dns.RcodeNotImplemented, "ANY queries are refused")
	case idBlocked:
		qc.note("blocked client: " + idEntry)
		res = result{drop: true, status: StatusDropped, reason: idEntry}
	default:
		res = s.process(qc)
	}
	dur := time.Since(qc.start)

	out := LookupResult{
		Name:       name,
		Type:       typ,
		Status:     res.status,
		Answers:    []string{},
		Reason:     res.reason,
		Upstream:   res.upstream,
		DurationUs: dur.Microseconds(),
		GroupIDs:   qc.id.GroupIDs,
		Steps:      steps,
		Matches:    []filter.Match{},
	}
	if res.msg != nil {
		out.RCode = rcodeString(res.msg.Rcode)
		for _, rr := range res.msg.Answer {
			if len(out.Answers) == maxLookupAnswers {
				break
			}
			out.Answers = append(out.Answers, rr.String())
		}
	}
	if s.d.Filter != nil {
		matches, err := s.d.Filter.Explain(ctx, name, qc.id.GroupIDs)
		switch {
		case err != nil:
			out.Steps = append(out.Steps, "filter explanation unavailable: "+errText(err))
		case matches != nil:
			out.Matches = matches
		}
	}
	return out, nil
}

// traceClient records the client identity, ACL and blocking state.
func (s *Server) traceClient(qc *qctx) {
	id := qc.id
	who := "unknown client (Default group)"
	if id.ClientID != 0 {
		who = fmt.Sprintf("client #%d %q", id.ClientID, id.Name)
	} else if id.Name != "" {
		who = fmt.Sprintf("unconfigured client %q", id.Name)
	}
	qc.note(fmt.Sprintf("client %s: %s, enabled groups %v", qc.client, who, id.GroupIDs))
	if !s.allowed(qc.source) {
		qc.note("note: this address is not allowed by the DNS ACL; its real queries are dropped")
	}
	if id.DownloadCacheBypass {
		qc.note("this client bypasses the download cache DNS answers")
	}
	f := qc.set.Filter
	switch {
	case !f.Enabled:
		qc.note("blocking is disabled")
	case !f.BlockingActive(qc.start) && f.PausedUntil != nil:
		qc.note("blocking is paused until " + f.PausedUntil.UTC().Format(time.RFC3339))
	case !qc.blocking:
		qc.note("blocking is inactive: filtering is paused for every group of the client")
	default:
		qc.note("blocking is active (mode " + f.BlockingMode + ")")
	}
	if s.d.Parental != nil {
		for _, p := range s.d.Parental.PausedGroups(id.GroupIDs, qc.start) {
			qc.note(fmt.Sprintf("filtering paused for group %s until %s", p.Group, p.Until.UTC().Format(time.RFC3339)))
		}
	}
}

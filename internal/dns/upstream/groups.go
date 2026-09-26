package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// maxGroupSets bounds the group sets (the clients package allows at most 16
// distinct group upstream lists; a set beyond this fails closed).
const maxGroupSets = 16

// GroupUpstreams is the upstream setting of one enabled client group:
// a preset (settings.UpstreamPresets) or its own upstreams.
type GroupUpstreams struct {
	ID        int64
	Name      string
	Preset    string
	Upstreams []string
}

// GroupUpstreamStat describes one group set for GET /dns/upstreams: the
// groups that use it, their preset, the statistics of its upstreams (of
// the clock-guard part while the clock guard is active) and, when the set
// could not be built, why.
type GroupUpstreamStat struct {
	GroupIDs   []int64        `json:"groupIds"`
	Preset     string         `json:"preset"`
	Upstreams  []UpstreamStat `json:"upstreams"`
	ClockGuard bool           `json:"clockGuard"`
	Error      string         `json:"error,omitempty"`
	// Names are the groups' names (the health check; not in the API).
	Names []string `json:"-"`
}

// groupSet is the upstream set of one distinct group upstream list (a
// preset counts as its list), shared by the groups that name it
// (ARCHITECTURE 7.4, group sets): no fallbacks, no client subnet; while
// the clock guard is active only the plain IP-literal entries of its own
// list (a preset's plain addresses) are asked.
type groupSet struct {
	key    string
	ids    []int64
	names  []string
	preset string
	normal *upstreamSet // nil: the set could not be built (err)
	guard  *upstreamSet // nil: no plain IP-literal entry
	err    string
}

// groupSets is the immutable registry of group sets.
type groupSets struct {
	sets    []*groupSet
	byGroup map[int64]*groupSet
}

func (g *groupSets) close() {
	for _, s := range g.sets {
		for _, u := range []*upstreamSet{s.normal, s.guard} {
			if u != nil {
				u.close()
			}
		}
	}
}

// SetGroupUpstreams builds one set per distinct group upstream list from
// the enabled groups that have a preset or their own upstreams (app calls
// it at start and after every group change; rebuild does it again when
// the bootstrap servers change). Sets are built here, never on the query
// path, and never evicted by queries; the replaced sets are closed after
// the swap. Upstream statistics survive for lists that stay.
func (r *Resolver) SetGroupUpstreams(groups []GroupUpstreams) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groupCfg = slices.Clone(groups)
	r.rebuildGroupsLocked(r.def.Load().boot)
}

// rebuildGroupsLocked builds the group sets of r.groupCfg with the
// bootstrap servers boot. r.mu must be held.
func (r *Resolver) rebuildGroupsLocked(boot *bootstrap) {
	r.lifeMu.Lock()
	closed := r.closed
	r.lifeMu.Unlock()
	if closed {
		return
	}
	prev := map[string]*upstreamStats{}
	old := r.groups.Load()
	if old != nil {
		for _, s := range old.sets {
			for _, set := range []*upstreamSet{s.normal, s.guard} {
				if set != nil {
					for _, u := range set.ups {
						prev[s.key+"\x00"+u.name] = u.st
					}
				}
			}
		}
	}
	next := &groupSets{byGroup: map[int64]*groupSet{}}
	byKey := map[string]*groupSet{}
	for _, g := range r.groupCfg {
		list, plain, key, perr := groupList(g)
		if s, ok := byKey[key]; ok && perr == nil {
			s.ids, s.names = append(s.ids, g.ID), append(s.names, g.Name)
			next.byGroup[g.ID] = s
			continue
		}
		s := &groupSet{key: key, ids: []int64{g.ID}, names: []string{g.Name}, preset: g.Preset}
		switch {
		case perr != nil:
			s.err = perr.Error()
		case len(next.sets) >= maxGroupSets:
			s.err = fmt.Sprintf("more than %d different group upstream lists", maxGroupSets)
		default:
			s.normal, s.guard, s.err = r.buildGroupSet(key, list, plain, boot, prev)
		}
		byKey[key] = s
		next.sets = append(next.sets, s)
		next.byGroup[g.ID] = s
	}
	r.groups.Store(next)
	if old != nil {
		old.close()
	}
	for _, s := range next.sets {
		if s.err != "" {
			r.log.Warn("group upstreams cannot be used; the group's clients get SERVFAIL", slog.Any("groups", s.names), slog.String("err", s.err))
		}
	}
}

// groupList returns the upstreams of a group setting, its plain IP-literal
// entries (a preset's plain addresses) and the key of its set.
func groupList(g GroupUpstreams) (list, plain []string, key string, err error) {
	if g.Preset != "" {
		p, ok := settings.UpstreamPresetByKey(g.Preset)
		if !ok {
			return nil, nil, "preset\x00" + g.Preset, fmt.Errorf("unknown preset %q", g.Preset)
		}
		return p.Upstreams, p.Plain, "preset\x00" + p.Key, nil
	}
	if len(g.Upstreams) == 0 {
		return nil, nil, "list\x00", errors.New("no upstreams")
	}
	for _, u := range g.Upstreams {
		spec, err := settings.ParseUpstream(u)
		if err == nil && spec.IsIPLit && (spec.Proto == "udp" || spec.Proto == "tcp") {
			plain = append(plain, u)
		}
	}
	return g.Upstreams, plain, "list\x00" + joinKey(g.Upstreams), nil
}

// buildGroupSet builds the normal and the clock-guard part of a group set.
// Every upstream given by name needs bootstrap servers; a list without a
// usable upstream cannot be built.
func (r *Resolver) buildGroupSet(key string, list, plain []string, boot *bootstrap, prev map[string]*upstreamStats) (normal, guard *upstreamSet, errText string) {
	for _, u := range list {
		if spec, err := settings.ParseUpstream(u); err == nil && !spec.IsIPLit && len(boot.servers) == 0 {
			return nil, nil, fmt.Sprintf("%s needs dns.bootstrap servers", spec.Host)
		}
	}
	stats := map[string]*upstreamStats{}
	for k, st := range prev {
		if name, ok := strings.CutPrefix(k, key+"\x00"); ok {
			stats[name] = st
		}
	}
	normal, errs := r.buildSet("group", list, boot, stats)
	if len(normal.ups) == 0 {
		normal.close()
		return nil, nil, errors.Join(append([]error{errNoUpstreams}, errs...)...).Error()
	}
	if len(plain) > 0 {
		if g, _ := r.buildSet("groupguard", plain, boot, stats); len(g.ups) > 0 {
			guard = g
		}
	}
	return normal, guard, ""
}

// GroupFor returns the group whose upstreams answer a client with the
// enabled groups (sorted): a group with a preset wins, else a group with
// its own upstreams; among equals the lowest id. Pauses do not matter (a
// family resolver is content protection). Hot path: no allocation.
func (r *Resolver) GroupFor(groups []int64) (int64, bool) {
	gs := r.groups.Load()
	if gs == nil || len(gs.byGroup) == 0 {
		return 0, false
	}
	var custom int64
	found := false
	for _, g := range groups {
		s, ok := gs.byGroup[g]
		if !ok {
			continue
		}
		if s.preset != "" {
			return g, true
		}
		if !found {
			custom, found = g, true
		}
	}
	return custom, found
}

// GroupName returns the name of a group with group upstreams ("" if none).
func (r *Resolver) GroupName(groupID int64) string {
	gs := r.groups.Load()
	if gs == nil {
		return ""
	}
	s, ok := gs.byGroup[groupID]
	if !ok {
		return ""
	}
	return s.names[slices.Index(s.ids, groupID)]
}

// ResolveGroup answers req through the group set of groupID (with its own
// cache namespace and in-flight de-duplication, the classification of
// blocked answers and the fastest-address order like the default set; no
// fallbacks and no client subnet). While the clock guard is active only
// the plain IP-literal entries of the group's own list are asked (none:
// an error; never the default set or the bootstrap servers). A set that
// could not be built fails (fail closed).
func (r *Resolver) ResolveGroup(ctx context.Context, req *dns.Msg, groupID int64) (*dns.Msg, Info, error) {
	gs := r.groups.Load()
	var s *groupSet
	if gs != nil {
		s = gs.byGroup[groupID]
	}
	switch {
	case s == nil:
		return nil, Info{}, errors.New("no group upstreams configured")
	case s.normal == nil:
		return nil, Info{}, errors.New(s.err)
	}
	set := s.normal
	if r.clockBehind() {
		if s.guard == nil {
			return nil, Info{}, errors.New("the clock guard is active and the group has no plain upstream given by IP address")
		}
		set = s.guard
	}
	return r.resolve(ctx, req, route{set: set, def: true}, netip.Prefix{})
}

// GroupStats describes every group set (never nil).
func (r *Resolver) GroupStats() []GroupUpstreamStat {
	gs := r.groups.Load()
	out := []GroupUpstreamStat{}
	if gs == nil {
		return out
	}
	guard := r.clockBehind()
	for _, s := range gs.sets {
		st := GroupUpstreamStat{GroupIDs: slices.Clone(s.ids), Preset: s.preset, Upstreams: []UpstreamStat{}, ClockGuard: guard, Error: s.err,
			Names: slices.Clone(s.names)}
		switch {
		case s.normal == nil:
		case guard && s.guard == nil:
			st.Error = "the clock guard is active and the group has no plain upstream given by IP address"
		case guard:
			st.Upstreams = setStats(s.guard)
		default:
			st.Upstreams = setStats(s.normal)
		}
		out = append(out, st)
	}
	return out
}

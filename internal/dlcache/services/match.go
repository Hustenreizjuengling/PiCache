package services

import (
	"slices"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// snapshot is the immutable matcher and service list.
type snapshot struct {
	ready    bool      // a cache-domains snapshot is loaded
	services []Service // display order
	index    map[string]int
	exact    map[string]int // host → services index
	suffix   map[string]int // base of "*.base" → services index
	steam    int            // index of the steam service (always present)
}

// lookup finds the service of a normalised host: an exact pattern wins,
// otherwise the longest "*.suffix" whose base is a proper parent of host
// (any depth, never the apex).
func (s *snapshot) lookup(host string) (*Service, bool) {
	if host == "" {
		return nil, false
	}
	if i, ok := s.exact[host]; ok {
		return &s.services[i], true
	}
	for i := 0; i < len(host)-1; i++ {
		if host[i] == '.' {
			if j, ok := s.suffix[host[i+1:]]; ok {
				return &s.services[j], true
			}
		}
	}
	return nil, false
}

// name returns the display name of a service ID.
func (s *snapshot) name(id string) string {
	if i, ok := s.index[id]; ok {
		return s.services[i].Name
	}
	return displayName(id)
}

// rebuildLocked publishes a new snapshot from the source, the custom
// services, the extra domains and the disabled list. r.mu must be held.
func (r *Registry) rebuildLocked(set *settings.All) {
	disabled := map[string]bool{}
	for _, id := range set.DownloadCache.DisabledServices {
		disabled[id] = true
	}
	s := &snapshot{ready: r.src != nil, index: map[string]int{}, exact: map[string]int{}, suffix: map[string]int{}, steam: -1}
	add := func(sv Service, patterns []string) {
		extra := r.extras[sv.ID]
		sv.ExtraDomains = append([]string{}, extra...)
		domains := slices.Clone(patterns)
		for _, p := range extra {
			if !slices.Contains(domains, p) {
				domains = append(domains, p)
			}
		}
		if sv.ID == steamID && !slices.Contains(domains, SteamTrigger) {
			domains = append(domains, SteamTrigger)
		}
		sv.Domains = domains
		if sv.Domains == nil {
			sv.Domains = []string{}
		}
		sv.DomainCount = len(sv.Domains)
		sv.Enabled = !disabled[sv.ID]
		s.index[sv.ID] = len(s.services)
		s.services = append(s.services, sv)
	}
	if r.src != nil {
		for _, src := range r.src.Services {
			add(Service{ID: src.ID, Name: displayName(src.ID), Description: src.Description,
				Notes: src.Notes, MixedContent: src.MixedContent}, src.Domains)
		}
	}
	if _, ok := s.index[steamID]; !ok {
		add(Service{ID: steamID, Name: displayName(steamID),
			Description: "Steam (built in: the lancache.steamcontent.com trigger and the Steam User-Agent rule)"}, nil)
	}
	for _, c := range r.custom {
		add(Service{ID: c.ID, Name: c.Name, Description: c.Description, Custom: true}, nil)
	}
	s.steam = s.index[steamID]

	// Patterns: an enabled service beats a disabled one, otherwise the
	// first service (source order, then custom) wins.
	for i := range s.services {
		for _, p := range s.services[i].Domains {
			m, key := s.exact, p
			if base, ok := cutWildcard(p); ok {
				m, key = s.suffix, base
			}
			if cur, ok := m[key]; ok && (s.services[cur].Enabled || !s.services[i].Enabled) {
				continue
			}
			m[key] = i
		}
	}
	r.snap.Store(s)
}

func cutWildcard(p string) (string, bool) {
	if len(p) > 2 && p[0] == '*' && p[1] == '.' {
		return p[2:], true
	}
	return "", false
}

package parental

import (
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// snapshot is the immutable state read by Check: the groups that have at
// least one restriction (blocked services, an enabled schedule or a block
// override; Check ignores an override that has ended).
type snapshot struct {
	groups map[int64]*rules
}

// rules are one group's compiled controls.
type rules struct {
	id       int64
	name     string
	override Override
	services []bool   // catalogue position → always blocked
	blockAll []window // enabled schedules that block all internet
	blockSvc []window // enabled schedules that block services
}

// window is a compiled schedule.
type window struct {
	id, name   string
	days       uint8 // bit d = weekday d (0 = Sunday)
	start, end int   // minutes of the day; end < start: ends the next day
	services   []bool
}

func newSnapshot(rows []row, names map[int64]string) *snapshot {
	s := &snapshot{groups: map[int64]*rules{}}
	for i := range rows {
		r := compile(&rows[i], names[rows[i].groupID])
		if r.restricts() {
			s.groups[r.id] = r
		}
	}
	return s
}

// compile turns a stored row into rules (disabled schedules are left out).
func compile(r *row, name string) *rules {
	out := &rules{id: r.groupID, name: name, override: r.override, services: serviceSet(r.cfg.BlockedServices)}
	for _, s := range r.cfg.Schedules {
		if !s.Enabled {
			continue
		}
		sh, sm, _ := settings.ParseClock(s.Start)
		eh, em, _ := settings.ParseClock(s.End)
		w := window{id: s.ID, name: s.Name, start: sh*60 + sm, end: eh*60 + em}
		for _, d := range s.Days {
			w.days |= 1 << d
		}
		if s.Block == BlockAll {
			out.blockAll = append(out.blockAll, w)
		} else {
			w.services = serviceSet(s.Services)
			out.blockSvc = append(out.blockSvc, w)
		}
	}
	return out
}

// serviceSet returns the catalogue positions of ids as a set (nil if none).
func serviceSet(ids []string) []bool {
	var set []bool
	for _, id := range ids {
		if i, ok := serviceIndex[id]; ok {
			if set == nil {
				set = make([]bool, len(catalogue))
			}
			set[i] = true
		}
	}
	return set
}

// restricts reports whether the rules can block anything (an allow
// override alone lifts nothing).
func (r *rules) restricts() bool {
	return r.services != nil || len(r.blockAll) > 0 || len(r.blockSvc) > 0 || r.override.Mode == OverrideBlock
}

// clock is a reading of the local wall clock.
type clock struct {
	day int // weekday, 0 = Sunday
	min int // minute of the day
}

func wallClock(t time.Time, loc *time.Location) clock {
	lt := t.In(loc)
	return clock{day: int(lt.Weekday()), min: lt.Hour()*60 + lt.Minute()}
}

// active reports whether the window contains the wall-clock time c.
func (w *window) active(c clock) bool {
	today := w.days&(1<<c.day) != 0
	if w.start < w.end {
		return today && c.min >= w.start && c.min < w.end
	}
	yesterday := w.days&(1<<((c.day+6)%7)) != 0
	return today && c.min >= w.start || yesterday && c.min < w.end
}

// endAfter returns the end of the window that is active at t.
func (w *window) endAfter(t time.Time, loc *time.Location) time.Time {
	lt := t.In(loc)
	y, m, d := lt.Date()
	if w.start > w.end && lt.Hour()*60+lt.Minute() >= w.start {
		d++ // started today, ends tomorrow
	}
	return at(y, m, d, w.end, loc)
}

// span is one occurrence of a window.
type span struct{ start, end time.Time }

// spans returns the occurrences of the window that start on the local
// dates from `from` to `to` days after the date of t.
func (w *window) spans(t time.Time, loc *time.Location, from, to int) []span {
	y, m, d := t.In(loc).Date()
	var out []span
	for i := from; i <= to; i++ {
		// Noon: the date arithmetic never touches a DST transition.
		if w.days&(1<<int(time.Date(y, m, d+i, 12, 0, 0, 0, loc).Weekday())) == 0 {
			continue
		}
		endDay := d + i
		if w.end < w.start {
			endDay++
		}
		out = append(out, span{start: at(y, m, d+i, w.start, loc), end: at(y, m, endDay, w.end, loc)})
	}
	return out
}

// at returns the instant of a local date and minute of the day. A time in
// the gap when the clocks go forward becomes the first valid instant after
// the gap (02:30 → 03:00).
func at(y int, m time.Month, d, minutes int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, minutes/60, minutes%60, 0, 0, loc)
	if t.Hour()*60+t.Minute() != minutes {
		// time.Date moved the time forward by the gap; the zone that
		// starts at the transition begins at the first valid instant.
		if start, _ := t.ZoneBounds(); !start.IsZero() && start.Before(t) {
			t = start
		}
	}
	return t
}

// Check returns whether the parental controls of the client's groups block
// qname (lower-case, no trailing dot) at now. The first group in groupIDs
// that blocks decides; within a group: an allow override lifts everything,
// then a block override, a block-all schedule, a blocked service.
func (e *Engine) Check(qname string, groupIDs []int64, now time.Time) Decision {
	s := e.snap.Load()
	if len(s.groups) == 0 {
		return Decision{}
	}
	var (
		c     clock
		haveC bool
		svc   = -2 // not looked up yet
	)
	for _, gid := range groupIDs {
		r := s.groups[gid]
		if r == nil {
			continue
		}
		if r.override.Mode != "" && now.Before(r.override.Until) {
			if r.override.Mode == OverrideAllow {
				continue
			}
			return Decision{Blocked: true, Kind: KindOverride, GroupID: r.id, Group: r.name, Until: r.override.Until}
		}
		if len(r.blockAll) > 0 || len(r.blockSvc) > 0 {
			if !haveC {
				c, haveC = wallClock(now, e.loc), true
			}
			for i := range r.blockAll {
				if w := &r.blockAll[i]; w.active(c) {
					return Decision{Blocked: true, Kind: KindSchedule, GroupID: r.id, Group: r.name, Name: w.name,
						Until: w.endAfter(now, e.loc)}
				}
			}
		}
		if r.services == nil && len(r.blockSvc) == 0 {
			continue
		}
		if svc == -2 {
			svc = serviceOf(qname)
		}
		if svc < 0 {
			continue
		}
		if r.services != nil && r.services[svc] {
			return Decision{Blocked: true, Kind: KindService, GroupID: r.id, Group: r.name, Name: catalogue[svc].Name}
		}
		for i := range r.blockSvc {
			if w := &r.blockSvc[i]; w.services[svc] && w.active(c) {
				return Decision{Blocked: true, Kind: KindService, GroupID: r.id, Group: r.name, Name: catalogue[svc].Name,
					Schedule: w.name, Until: w.endAfter(now, e.loc)}
			}
		}
	}
	return Decision{}
}

// state describes what the rules do at now (for the UI).
func (r *rules) state(now time.Time, loc *time.Location) GroupState {
	zone, offset := now.In(loc).Zone()
	st := GroupState{BlockedServices: []string{}, TimeZone: zone, UTCOffsetMinutes: offset / 60}
	o := r.override
	oActive := o.Mode != "" && now.Before(o.Until)
	c := wallClock(now, loc)
	switch {
	case oActive && o.Mode == OverrideAllow:
		st.Lifted, st.LiftedUntil = true, o.Until.UTC()
	case oActive:
		st.BlockAll, st.Reason, st.Until = true, KindOverride, r.blockAllUntil(o.Until, loc).UTC()
	default:
		for i := range r.blockAll {
			w := &r.blockAll[i]
			if !w.active(c) {
				continue
			}
			until := r.blockAllUntil(now, loc)
			if !until.After(now) { // the wall clock and the instants disagree around a DST change
				until = w.endAfter(now, loc)
			}
			st.BlockAll, st.Reason, st.Schedule, st.Until = true, KindSchedule, w.name, until.UTC()
			break
		}
	}
	if !st.Lifted {
		for i, s := range catalogue {
			blocked := r.services != nil && r.services[i]
			for j := range r.blockSvc {
				if w := &r.blockSvc[j]; w.services[i] && w.active(c) {
					blocked = true
				}
			}
			if blocked {
				st.BlockedServices = append(st.BlockedServices, s.ID)
			}
		}
		slices.Sort(st.BlockedServices)
	}
	st.Next = r.next(now, loc)
	return st
}

// blockAllUntil returns when blocking all internet ends if it lasts at
// least until t: t, extended by every block-all window that contains it
// (a window that starts when another one ends continues the block), at
// most 7 days.
func (r *rules) blockAllUntil(t time.Time, loc *time.Location) time.Time {
	limit := t.Add(maxOverride)
	for t.Before(limit) {
		next := t
		for i := range r.blockAll {
			for _, sp := range r.blockAll[i].spans(t, loc, -1, 0) {
				if !sp.start.After(t) && sp.end.After(next) {
					next = sp.end
				}
			}
		}
		if !next.After(t) {
			break
		}
		t = next
	}
	return t
}

// next returns the first start or end of an enabled schedule window after
// now and within 7 days (ends before starts at the same time).
func (r *rules) next(now time.Time, loc *time.Location) *Change {
	var best *Change
	consider := func(w *window, t time.Time, starts bool) {
		if !t.After(now) || t.After(now.Add(maxOverride)) {
			return
		}
		if best == nil || t.Before(best.Time) || t.Equal(best.Time) && best.Starts && !starts {
			best = &Change{Time: t.UTC(), ScheduleID: w.id, Name: w.name, Starts: starts}
		}
	}
	for _, list := range [][]window{r.blockAll, r.blockSvc} {
		for i := range list {
			w := &list[i]
			for _, sp := range w.spans(now, loc, -1, 7) {
				consider(w, sp.start, true)
				consider(w, sp.end, false)
			}
		}
	}
	return best
}

package parental

import (
	"context"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
)

// Overnight windows, day lists, same-day service windows, always blocked
// services and disabled schedules.
func TestScheduleWindows(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	gc := env.update(kids, Config{
		BlockedServices: []string{"roblox"},
		Schedules: []Schedule{
			bedtime(0, 1, 2, 3, 4), // school nights: Sunday to Thursday
			{Name: "Homework time", Enabled: true, Days: []int{1, 2, 3, 4, 5}, Start: "15:00", End: "17:00",
				Block: BlockServices, Services: []string{"youtube", "tiktok"}},
			{Name: "Off", Enabled: false, Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "23:59", Block: BlockAll},
		},
	})
	g := []int64{kids}
	if wd := env.local(2026, 9, 24, 12, 0).Weekday(); wd != time.Thursday {
		t.Fatalf("2026-09-24 is a %s", wd)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"example.com", env.local(2026, 9, 24, 20, 59), ""},
		{"example.com", env.local(2026, 9, 24, 21, 0), "Kids: Bedtime"},  // Thursday evening
		{"example.com", env.local(2026, 9, 25, 6, 59), "Kids: Bedtime"},  // ends Friday morning
		{"example.com", env.local(2026, 9, 25, 7, 0), ""},                // end is exclusive
		{"example.com", env.local(2026, 9, 25, 22, 0), ""},               // Friday is no school night
		{"example.com", env.local(2026, 9, 26, 6, 0), ""},                // … so Saturday morning is free
		{"example.com", env.local(2026, 9, 27, 21, 30), "Kids: Bedtime"}, // Sunday evening
		{"www.youtube.com", env.local(2026, 9, 25, 16, 0), "Kids: YouTube (Homework time)"},
		{"www.youtube.com", env.local(2026, 9, 25, 17, 0), ""},
		{"www.youtube.com", env.local(2026, 9, 26, 16, 0), ""}, // Saturday
		{"example.com", env.local(2026, 9, 25, 16, 0), ""},
		{"rbxcdn.com", env.local(2026, 9, 26, 12, 0), "Kids: Roblox"},  // always
		{"rbxcdn.com", env.local(2026, 9, 24, 22, 0), "Kids: Bedtime"}, // block-all first
	} {
		if got := env.check(tc.name, g, tc.at); got != tc.want {
			t.Errorf("%s at %s: %q, want %q", tc.name, tc.at.Format("Mon 15:04"), got, tc.want)
		}
	}
	d := env.e.Check("example.com", g, env.local(2026, 9, 24, 22, 0))
	if d.Kind != KindSchedule || d.GroupID != kids || d.Name != "Bedtime" || !d.Until.Equal(env.local(2026, 9, 25, 7, 0)) {
		t.Errorf("bedtime decision %+v", d)
	}
	d = env.e.Check("youtube.com", g, env.local(2026, 9, 25, 16, 0))
	if d.Kind != KindService || d.Name != "YouTube" || d.Schedule != "Homework time" || !d.Until.Equal(env.local(2026, 9, 25, 17, 0)) {
		t.Errorf("service decision %+v", d)
	}
	if d := env.e.Check("roblox.com", g, env.now); d.Kind != KindService || !d.Until.IsZero() || d.Schedule != "" {
		t.Errorf("always blocked %+v", d)
	}
	if len(gc.Schedules) != 3 || gc.Schedules[2].Enabled {
		t.Fatalf("stored %+v", gc.Schedules)
	}
}

// DST in Europe/Berlin: 2026-03-29 02:00 CET → 03:00 CEST and 2026-10-25
// 03:00 CEST → 02:00 CET.
func TestScheduleDST(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	teens := env.group("Teens")
	env.update(kids, Config{Schedules: []Schedule{
		{Name: "Night", Enabled: true, Days: []int{6}, Start: "22:00", End: "07:00", Block: BlockAll}, // Saturday night
	}})
	env.update(teens, Config{Schedules: []Schedule{
		{Name: "Gap", Enabled: true, Days: []int{0}, Start: "02:30", End: "04:00", Block: BlockServices, Services: []string{"youtube"}},
	}})
	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		group          int64
		name, at, want string
	}{
		// Spring forward: the night lasts 8 hours of real time and ends at 07:00 CEST.
		{kids, "example.com", "2026-03-28T20:59:00Z", ""},            // 21:59 CET
		{kids, "example.com", "2026-03-28T21:00:00Z", "Kids: Night"}, // 22:00 CET
		{kids, "example.com", "2026-03-29T04:59:00Z", "Kids: Night"}, // 06:59 CEST
		{kids, "example.com", "2026-03-29T05:00:00Z", ""},            // 07:00 CEST
		{kids, "example.com", "2026-03-21T21:00:00Z", "Kids: Night"}, // an ordinary Saturday, 22:00 CET
		{kids, "example.com", "2026-03-22T05:59:00Z", "Kids: Night"}, // 06:59 CET
		{kids, "example.com", "2026-03-22T06:00:00Z", ""},            // 07:00 CET
		{kids, "example.com", "2026-03-20T21:00:00Z", ""},            // Friday
		// Fall back: the night lasts 10 hours and ends at 07:00 CET.
		{kids, "example.com", "2026-10-24T20:00:00Z", "Kids: Night"}, // 22:00 CEST
		{kids, "example.com", "2026-10-25T00:30:00Z", "Kids: Night"}, // 02:30 CEST
		{kids, "example.com", "2026-10-25T01:30:00Z", "Kids: Night"}, // 02:30 CET
		{kids, "example.com", "2026-10-25T05:59:00Z", "Kids: Night"}, // 06:59 CET
		{kids, "example.com", "2026-10-25T06:00:00Z", ""},            // 07:00 CET
		// A window that starts in the skipped hour starts at the first valid instant.
		{teens, "youtube.com", "2026-03-29T00:59:00Z", ""},                     // 01:59 CET
		{teens, "youtube.com", "2026-03-29T01:00:00Z", "Teens: YouTube (Gap)"}, // 03:00 CEST
		{teens, "youtube.com", "2026-03-29T01:59:00Z", "Teens: YouTube (Gap)"}, // 03:59 CEST
		{teens, "youtube.com", "2026-03-29T02:00:00Z", ""},                     // 04:00 CEST
		{teens, "www.youtube.com", "2026-03-29T01:00:00Z", "Teens: YouTube (Gap)"},
		{teens, "notyoutube.com", "2026-03-29T01:00:00Z", ""},
		{teens, "youtube.com.example", "2026-03-29T01:00:00Z", ""},
		{teens, "example.com", "2026-03-29T01:00:00Z", ""},
		// The repeated hour counts as the clock shows it.
		{teens, "youtube.com", "2026-10-24T23:59:00Z", ""},                     // 01:59 CEST
		{teens, "youtube.com", "2026-10-25T00:35:00Z", "Teens: YouTube (Gap)"}, // 02:35 CEST
		{teens, "youtube.com", "2026-10-25T01:35:00Z", "Teens: YouTube (Gap)"}, // 02:35 CET
		{teens, "youtube.com", "2026-10-25T02:59:00Z", "Teens: YouTube (Gap)"}, // 03:59 CET
		{teens, "youtube.com", "2026-10-25T03:00:00Z", ""},                     // 04:00 CET
	} {
		if got := env.check(tc.name, []int64{tc.group}, utc(tc.at)); got != tc.want {
			t.Errorf("%s at %s: %q, want %q", tc.name, tc.at, got, tc.want)
		}
	}
	for _, tc := range []struct {
		group         int64
		name, at, end string
	}{
		{kids, "example.com", "2026-03-29T00:00:00Z", "2026-03-29T05:00:00Z"},
		{kids, "example.com", "2026-10-25T00:00:00Z", "2026-10-25T06:00:00Z"},
		{teens, "youtube.com", "2026-03-29T01:00:00Z", "2026-03-29T02:00:00Z"},
	} {
		if d := env.e.Check(tc.name, []int64{tc.group}, utc(tc.at)); !d.Until.Equal(utc(tc.end)) {
			t.Errorf("%s at %s: until %v, want %s", tc.name, tc.at, d.Until.UTC(), tc.end)
		}
	}
	// The state: the start in the gap is the first valid instant; the
	// night ends at 07:00 CEST.
	env.now = utc("2026-03-29T00:50:00Z") // 01:50 CET
	gc, _ := env.e.Get(context.Background(), teens)
	if n := gc.State.Next; n == nil || !n.Time.Equal(utc("2026-03-29T01:00:00Z")) || n.Name != "Gap" || !n.Starts {
		t.Errorf("next %+v", gc.State.Next)
	}
	gc, _ = env.e.Get(context.Background(), kids)
	if st := gc.State; !st.BlockAll || st.Reason != "schedule" || st.Schedule != "Night" || !st.Until.Equal(utc("2026-03-29T05:00:00Z")) {
		t.Errorf("state %+v", st)
	}
	if n := gc.State.Next; n == nil || n.Starts || !n.Time.Equal(utc("2026-03-29T05:00:00Z")) {
		t.Errorf("next %+v", gc.State.Next)
	}
}

// Overrides: block beats schedules of its group, allow lifts only its own
// group; the first blocking group in the identity's order decides.
func TestOverridesAndGroups(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	teens := env.group("Teens")
	env.update(clients.DefaultGroupID, Config{BlockedServices: []string{"tiktok"}})
	env.update(kids, Config{BlockedServices: []string{"youtube"}, Schedules: []Schedule{bedtime(0, 1, 2, 3, 4, 5, 6)}})
	env.update(teens, Config{BlockedServices: []string{"roblox"}})
	night := env.local(2026, 9, 21, 22, 0)
	day := env.local(2026, 9, 21, 12, 0)

	both := []int64{clients.DefaultGroupID, kids, teens}
	for _, tc := range []struct {
		name   string
		groups []int64
		at     time.Time
		want   string
	}{
		{"tiktok.com", both, night, "Default: TikTok"}, // Default comes first
		{"example.com", both, night, "Kids: Bedtime"},
		{"roblox.com", both, day, "Teens: Roblox"},
		{"youtube.com", both, day, "Kids: YouTube"},
		{"youtube.com", []int64{teens}, day, ""},
		{"example.com", []int64{}, night, ""},
		{"example.com", nil, night, ""},
		{"example.com", []int64{999}, night, ""},
	} {
		if got := env.check(tc.name, tc.groups, tc.at); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.name, tc.groups, got, tc.want)
		}
	}

	// Allow lifts the Kids' bedtime and services, not Teens' or Default's.
	env.now = night
	env.override(kids, OverrideAllow, 60)
	for _, tc := range []struct{ name, want string }{
		{"example.com", ""},
		{"youtube.com", ""},
		{"roblox.com", "Teens: Roblox"},
		{"tiktok.com", "Default: TikTok"},
	} {
		if got := env.check(tc.name, both, night.Add(time.Minute)); got != tc.want {
			t.Errorf("allow override: %s: %q, want %q", tc.name, got, tc.want)
		}
	}
	gc, _ := env.e.Get(context.Background(), kids)
	if st := gc.State; !st.Lifted || !st.LiftedUntil.Equal(night.Add(time.Hour)) || st.BlockAll || len(st.BlockedServices) != 0 {
		t.Errorf("lifted state %+v", st)
	}
	if got := env.check("example.com", both, night.Add(61*time.Minute)); got != "Kids: Bedtime" {
		t.Errorf("after the allow override: %q", got)
	}

	// Block beats everything of Teens during the day.
	env.now = day
	env.override(teens, OverrideBlock, 120)
	d := env.e.Check("example.com", []int64{teens, kids}, day.Add(time.Minute))
	if d.Kind != KindOverride || d.GroupID != teens || !d.Until.Equal(day.Add(2*time.Hour)) || d.Reason() != "Teens: blocked by hand" {
		t.Errorf("block override %+v", d)
	}
	// A disabled group never applies: the identity lists enabled groups
	// only, so the engine never sees it (nothing to test here beyond the
	// group list), and a group with only an allow override has nothing to lift.
	env.update(teens, Config{})
	env.override(teens, OverrideAllow, 10)
	if _, ok := env.e.snap.Load().groups[teens]; ok {
		t.Error("a group with only an allow override needs no rules in the snapshot")
	}
}

// The state: block-all windows that follow each other are one block; the
// services blocked now; the next start or end.
func TestGroupState(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	env.update(kids, Config{BlockedServices: []string{"roblox"}, Schedules: []Schedule{
		{Name: "Evening", Enabled: true, Days: []int{1}, Start: "20:00", End: "21:00", Block: BlockAll},
		bedtime(1),
		{Name: "Homework time", Enabled: true, Days: []int{1}, Start: "15:00", End: "17:00", Block: BlockServices,
			Services: []string{"youtube"}},
	}})
	ctx := context.Background()
	env.now = env.local(2026, 9, 21, 20, 30) // Monday
	gc, _ := env.e.Get(ctx, kids)
	st := gc.State
	if !st.BlockAll || st.Reason != "schedule" || st.Schedule != "Evening" || !st.Until.Equal(env.local(2026, 9, 22, 7, 0)) {
		t.Errorf("chained block %+v", st)
	}
	if n := st.Next; n == nil || n.Name != "Evening" || n.Starts || !n.Time.Equal(env.local(2026, 9, 21, 21, 0)) {
		t.Errorf("next %+v", st.Next)
	}
	env.now = env.local(2026, 9, 21, 16, 0)
	gc, _ = env.e.Get(ctx, kids)
	if st := gc.State; st.BlockAll || len(st.BlockedServices) != 2 || st.BlockedServices[0] != "roblox" || st.BlockedServices[1] != "youtube" {
		t.Errorf("afternoon %+v", st)
	}
	// A block override that ends inside the bedtime blocks until its end.
	env.now = env.local(2026, 9, 21, 18, 0)
	env.override(kids, OverrideBlock, 4*60)
	gc, _ = env.e.Get(ctx, kids)
	if st := gc.State; !st.BlockAll || st.Reason != "override" || !st.Until.Equal(env.local(2026, 9, 22, 7, 0)) {
		t.Errorf("override into bedtime %+v", st)
	}
	// Without enabled schedules there is no next change.
	if _, err := env.e.ClearOverride(ctx, kids); err != nil {
		t.Fatal(err)
	}
	env.update(kids, Config{})
	gc, _ = env.e.Get(ctx, kids)
	if gc.State.Next != nil || gc.State.BlockAll || len(gc.State.BlockedServices) != 0 {
		t.Errorf("empty state %+v", gc.State)
	}
}

// Check neither reads the time zone nor allocates when no group has
// restrictions, and does not allocate on the restricted path either.
func TestCheckFastPath(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	env.e.loc = nil // would panic if the fast path converted the time
	groups := []int64{clients.DefaultGroupID, kids}
	if n := testing.AllocsPerRun(100, func() { env.e.Check("www.youtube.com", groups, env.now) }); n != 0 {
		t.Errorf("fast path allocates %v times", n)
	}
	env.e.loc = env.loc
	env.update(kids, Config{BlockedServices: []string{"youtube"}, Schedules: []Schedule{bedtime(0, 1, 2, 3, 4, 5, 6)}})
	env.e.loc = nil
	if d := env.e.Check("www.youtube.com", []int64{clients.DefaultGroupID}, env.now); d.Blocked {
		t.Errorf("unrestricted group: %+v", d)
	}
	env.e.loc = env.loc
	if n := testing.AllocsPerRun(100, func() { env.e.Check("www.youtube.com", groups, env.now) }); n != 0 {
		t.Errorf("restricted path allocates %v times", n)
	}
}

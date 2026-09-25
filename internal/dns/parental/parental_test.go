package parental

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Berlin on machines without a zone database

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// testEnv is an engine on a temp picache.db with a real clients registry,
// a settable clock and the Europe/Berlin time zone.
type testEnv struct {
	t   *testing.T
	e   *Engine
	reg *clients.Registry
	db  *db.DB
	now time.Time
	loc *time.Location
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg, err := clients.New(ctx, d, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(ctx, d, reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	env := &testEnv{t: t, e: e, reg: reg, db: d, loc: loc}
	e.loc = loc
	e.now = func() time.Time { return env.now }
	reg.OnChange(func() {
		if err := e.Reload(context.Background()); err != nil {
			t.Error(err)
		}
	})
	env.now = env.local(2026, 9, 21, 12, 0) // a Monday
	return env
}

// local returns a Berlin wall-clock time.
func (env *testEnv) local(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, env.loc)
}

func (env *testEnv) group(name string) int64 {
	env.t.Helper()
	g, err := env.reg.CreateGroup(context.Background(), clients.GroupInput{Name: name, Enabled: true})
	if err != nil {
		env.t.Fatal(err)
	}
	return g.ID
}

func (env *testEnv) update(id int64, cfg Config) GroupControls {
	env.t.Helper()
	gc, err := env.e.Update(context.Background(), id, cfg)
	if err != nil {
		env.t.Fatalf("update group %d: %v", id, err)
	}
	return gc
}

func (env *testEnv) override(id int64, mode string, minutes int) {
	env.t.Helper()
	if _, err := env.e.SetOverride(context.Background(), id, OverrideInput{Mode: mode, Minutes: &minutes}); err != nil {
		env.t.Fatal(err)
	}
}

// check runs Check at t and returns the log reason ("" = not blocked).
func (env *testEnv) check(name string, groups []int64, t time.Time) string {
	d := env.e.Check(name, groups, t)
	if !d.Blocked {
		return ""
	}
	return d.Reason()
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindInvalid || ae.Field != field {
		t.Fatalf("error %v, want invalid field %q", err, field)
	}
}

func bedtime(days ...int) Schedule {
	return Schedule{Name: "Bedtime", Enabled: true, Days: days, Start: "21:00", End: "07:00", Block: BlockAll}
}

func TestValidation(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	ctx := context.Background()
	ok := Schedule{Name: "x", Enabled: true, Days: []int{1}, Start: "08:00", End: "09:00", Block: BlockAll}
	with := func(fn func(*Schedule)) Config {
		s := ok
		fn(&s)
		return Config{Schedules: []Schedule{s}}
	}
	eleven := make([]Schedule, 11)
	for i := range eleven {
		eleven[i] = ok
	}
	for _, tc := range []struct {
		cfg   Config
		field string
	}{
		{Config{BlockedServices: []string{"youtube", "myspace"}}, "blockedServices"},
		{Config{Schedules: eleven}, "schedules"},
		{with(func(s *Schedule) { s.Name = "  " }), "schedules[0].name"},
		{with(func(s *Schedule) { s.Name = strings.Repeat("a", 41) }), "schedules[0].name"},
		{with(func(s *Schedule) { s.Name = "a\nb" }), "schedules[0].name"},
		{with(func(s *Schedule) { s.Days = nil }), "schedules[0].days"},
		{with(func(s *Schedule) { s.Days = []int{7} }), "schedules[0].days"},
		{with(func(s *Schedule) { s.Days = []int{-1} }), "schedules[0].days"},
		{with(func(s *Schedule) { s.Start = "7:00" }), "schedules[0].start"},
		{with(func(s *Schedule) { s.End = "24:00" }), "schedules[0].end"},
		{with(func(s *Schedule) { s.End = s.Start }), "schedules[0].end"},
		{with(func(s *Schedule) { s.Block = "some" }), "schedules[0].block"},
		{with(func(s *Schedule) { s.Services = []string{"youtube"} }), "schedules[0].services"},
		{with(func(s *Schedule) { s.Block = BlockServices }), "schedules[0].services"},
		{with(func(s *Schedule) { s.Block, s.Services = BlockServices, []string{"nope"} }), "schedules[0].services"},
	} {
		_, err := env.e.Update(ctx, kids, tc.cfg)
		wantField(t, err, tc.field)
	}
	if _, err := env.e.Update(ctx, 999, Config{}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown group: %v", err)
	}
	if _, err := env.e.Get(ctx, 999); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown group: %v", err)
	}

	minutes := func(n int) *int { return &n }
	until := func(d time.Duration) *time.Time { t := env.now.Add(d); return &t }
	for _, tc := range []struct {
		in    OverrideInput
		field string
	}{
		{OverrideInput{Mode: "pause", Minutes: minutes(30)}, "override.mode"},
		{OverrideInput{Mode: OverrideBlock}, "minutes"},
		{OverrideInput{Mode: OverrideBlock, Minutes: minutes(0)}, "minutes"},
		{OverrideInput{Mode: OverrideBlock, Minutes: minutes(10081)}, "minutes"},
		{OverrideInput{Mode: OverrideBlock, Minutes: minutes(30), Until: until(time.Hour)}, "minutes"},
		{OverrideInput{Mode: OverrideAllow, Until: until(-time.Minute)}, "override.until"},
		{OverrideInput{Mode: OverrideAllow, Until: until(7*24*time.Hour + 2*time.Minute)}, "override.until"},
	} {
		_, err := env.e.SetOverride(ctx, kids, tc.in)
		wantField(t, err, tc.field)
	}
	if _, err := env.e.SetOverride(ctx, 999, OverrideInput{Mode: OverrideBlock, Minutes: minutes(5)}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown group: %v", err)
	}
	if _, err := env.e.ClearOverride(ctx, 999); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown group: %v", err)
	}
	if gc, err := env.e.SetOverride(ctx, kids, OverrideInput{Mode: OverrideAllow, Minutes: minutes(10080)}); err != nil ||
		gc.Override == nil || !gc.Override.Until.Equal(env.now.Add(7*24*time.Hour)) {
		t.Fatalf("7 days: %+v %v", gc.Override, err)
	}
}

// Stored configurations are normalised: ids assigned and kept, days and
// services sorted and de-duplicated, services of block-all schedules empty.
func TestUpdateNormalises(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	gc := env.update(kids, Config{
		BlockedServices: []string{"tiktok", "roblox", "tiktok"},
		Schedules: []Schedule{
			bedtime(4, 0, 1, 1),
			{Name: " Homework time ", Enabled: true, Days: []int{5, 1}, Start: "15:00", End: "17:00", Block: BlockServices,
				Services: []string{"youtube", "discord"}},
		},
	})
	if !slices.Equal(gc.BlockedServices, []string{"roblox", "tiktok"}) || len(gc.Schedules) != 2 {
		t.Fatalf("stored %+v", gc)
	}
	s0, s1 := gc.Schedules[0], gc.Schedules[1]
	if !validScheduleID(s0.ID) || !validScheduleID(s1.ID) || s0.ID == s1.ID {
		t.Fatalf("ids %q %q", s0.ID, s1.ID)
	}
	if !slices.Equal(s0.Days, []int{0, 1, 4}) || s0.Services == nil || len(s0.Services) != 0 || s1.Name != "Homework time" ||
		!slices.Equal(s1.Services, []string{"discord", "youtube"}) || !slices.Equal(s1.Days, []int{1, 5}) {
		t.Fatalf("schedules %+v", gc.Schedules)
	}
	// Ids are kept on update; duplicates and malformed ids get new ones.
	s2 := s0
	s2.Name = "Copy"
	s3 := s0
	s3.ID, s3.Name = "NOT-HEX!", "Other"
	gc = env.update(kids, Config{Schedules: []Schedule{s1, s0, s2, s3}})
	ids := []string{gc.Schedules[0].ID, gc.Schedules[1].ID, gc.Schedules[2].ID, gc.Schedules[3].ID}
	if ids[0] != s1.ID || ids[1] != s0.ID || ids[2] == s0.ID || !validScheduleID(ids[2]) || !validScheduleID(ids[3]) ||
		ids[3] == ids[2] {
		t.Fatalf("ids %v (had %s, %s)", ids, s0.ID, s1.ID)
	}
	if gc.BlockedServices == nil || len(gc.BlockedServices) != 0 || gc.UpdatedAt.IsZero() {
		t.Fatalf("controls %+v", gc)
	}
	// A group without a row has empty (not null) lists.
	all, err := env.e.List(context.Background())
	if err != nil || len(all) != 2 || all[0].GroupID != clients.DefaultGroupID || all[1].GroupID != kids ||
		all[0].BlockedServices == nil || all[0].Schedules == nil || all[0].State.BlockedServices == nil || all[0].GroupName != "Default" {
		t.Fatalf("list %+v %v", all, err)
	}
}

// Deleting a group deletes its controls (foreign key), and the snapshot
// follows group renames.
func TestGroupDeleteCascades(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	kids := env.group("Kids")
	env.update(kids, Config{BlockedServices: []string{"youtube"}})
	if got := env.check("youtube.com", []int64{kids}, env.now); got != "Kids: YouTube" {
		t.Fatalf("before rename: %q", got)
	}
	if _, err := env.reg.UpdateGroup(ctx, kids, clients.GroupInput{Name: "Children", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := env.check("youtube.com", []int64{kids}, env.now); got != "Children: YouTube" {
		t.Fatalf("after rename: %q", got)
	}
	if err := env.reg.DeleteGroup(ctx, kids); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := env.db.R.QueryRow(`SELECT COUNT(*) FROM parental_groups WHERE group_id = ?`, kids).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left: %d %v", n, err)
	}
	if d := env.e.Check("youtube.com", []int64{kids}, env.now); d.Blocked {
		t.Fatalf("deleted group still blocks: %+v", d)
	}
}

// An expired override is ignored at once and removed with the next write.
func TestOverrideExpiry(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	kids := env.group("Kids")
	env.override(kids, OverrideBlock, 30)
	if got := env.check("example.com", []int64{kids}, env.now); got != "Kids: blocked by hand" {
		t.Fatalf("override: %q", got)
	}
	gc, _ := env.e.Get(ctx, kids)
	if gc.Override == nil || gc.Override.Mode != OverrideBlock || !gc.State.BlockAll || gc.State.Reason != "override" ||
		!gc.State.Until.Equal(env.now.Add(30*time.Minute)) {
		t.Fatalf("state %+v %+v", gc.Override, gc.State)
	}
	env.now = env.now.Add(31 * time.Minute)
	if d := env.e.Check("example.com", []int64{kids}, env.now); d.Blocked {
		t.Fatalf("expired override blocks: %+v", d)
	}
	if gc, _ := env.e.Get(ctx, kids); gc.Override != nil || gc.State.BlockAll {
		t.Fatalf("expired override shown: %+v", gc)
	}
	other := env.group("Teens")
	env.update(other, Config{})
	var mode string
	var until sql.NullInt64
	if err := env.db.R.QueryRow(`SELECT override_mode, override_until FROM parental_groups WHERE group_id = ?`, kids).
		Scan(&mode, &until); err != nil || mode != "" || until.Valid {
		t.Fatalf("expired override not removed: %q %v %v", mode, until, err)
	}
	// Clearing ends an active override.
	env.override(kids, OverrideAllow, 60)
	if gc, err := env.e.ClearOverride(ctx, kids); err != nil || gc.Override != nil || gc.State.Lifted {
		t.Fatalf("clear: %+v %v", gc, err)
	}
}

// A stored configuration that no longer validates (a service left the
// catalogue, an edited database) is sanitised, never fatal.
func TestLoadSanitises(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	env.update(kids, Config{BlockedServices: []string{"youtube"}})
	cfg := `{"blockedServices":["youtube","myspace"],"schedules":[` +
		`{"id":"0000000a","name":"ok","enabled":true,"days":[1,9],"start":"21:00","end":"07:00","block":"all","services":["x"]},` +
		`{"id":"bad","name":"no id","enabled":true,"days":[1],"start":"21:00","end":"07:00","block":"all"},` +
		`{"id":"0000000b","name":"no services","enabled":true,"days":[1],"start":"08:00","end":"09:00","block":"services","services":["myspace"]}]}`
	if _, err := env.db.W.Exec(`UPDATE parental_groups SET config = ? WHERE group_id = ?`, cfg, kids); err != nil {
		t.Fatal(err)
	}
	gc, err := env.e.Get(context.Background(), kids)
	if err != nil || !slices.Equal(gc.BlockedServices, []string{"youtube"}) || len(gc.Schedules) != 1 ||
		!slices.Equal(gc.Schedules[0].Days, []int{1}) || len(gc.Schedules[0].Services) != 0 {
		t.Fatalf("sanitised %+v %v", gc, err)
	}
	if _, err := env.db.W.Exec(`UPDATE parental_groups SET config = 'not json' WHERE group_id = ?`, kids); err != nil {
		t.Fatal(err)
	}
	if err := env.e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gc, err := env.e.Get(context.Background(), kids); err != nil || len(gc.BlockedServices) != 0 {
		t.Fatalf("unreadable config: %+v %v", gc, err)
	}
}

func TestReason(t *testing.T) {
	for _, tc := range []struct {
		d    Decision
		want string
	}{
		{Decision{Kind: KindSchedule, Group: "Kids", Name: "Bedtime"}, "Kids: Bedtime"},
		{Decision{Kind: KindOverride, Group: "Kids"}, "Kids: blocked by hand"},
		{Decision{Kind: KindService, Group: "Kids", Name: "YouTube"}, "Kids: YouTube"},
		{Decision{Kind: KindService, Group: "Kids", Name: "YouTube", Schedule: "Homework time"}, "Kids: YouTube (Homework time)"},
	} {
		if got := tc.d.Reason(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.d, got, tc.want)
		}
	}
}

var errSentinel = errors.New("sentinel")

// failingGroups makes Reload fail.
type failingGroups struct{}

func (failingGroups) Groups(context.Context) ([]clients.Group, error) { return nil, errSentinel }

func TestNewFailsWithoutGroups(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := New(context.Background(), d, failingGroups{}, nil); !errors.Is(err, errSentinel) {
		t.Fatalf("New: %v", err)
	}
}

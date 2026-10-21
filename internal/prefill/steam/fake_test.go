package steam_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/steam"
	"github.com/hustenreizjuengling/picache/internal/prefill/steam/steamtest"
)

// fakeApps is a small library: a game with platform, language, shared
// and DLC depots, a second game, a tool, an unowned game.
func fakeApps() map[uint32]steamtest.App {
	return map[uint32]steamtest.App{
		100: {Name: "Alpha", Type: "Game", DLC: []uint32{150, 160}, Depots: []steamtest.Depot{
			{ID: 101, Manifest: 1101, OS: "windows", Chunks: 5, ChunkSize: 1000},
			{ID: 102, Manifest: 1102, OS: "linux", Chunks: 2, ChunkSize: 1000},
			{ID: 103, Manifest: 1103, OS: "windows", Language: "german", Chunks: 2, ChunkSize: 500},
			{ID: 104, FromApp: 900},
		}},
		150:  {Name: "Alpha DLC", Type: "DLC", Depots: []steamtest.Depot{{ID: 151, Manifest: 1151, OS: "windows", Chunks: 1, ChunkSize: 700}}},
		160:  {Name: "Unowned DLC", Type: "DLC", Depots: []steamtest.Depot{{ID: 161, Manifest: 1161, Chunks: 1, ChunkSize: 700}}},
		200:  {Name: "beta game", Type: "game", Depots: []steamtest.Depot{{ID: 201, Manifest: 1201, Chunks: 3, ChunkSize: 300}}},
		300:  {Name: "SDK", Type: "Tool", Depots: []steamtest.Depot{{ID: 301, Manifest: 1301, Chunks: 1, ChunkSize: 100}}},
		400:  {Name: "Not mine", Type: "Game", Depots: []steamtest.Depot{{ID: 401, Manifest: 1401, Chunks: 1, ChunkSize: 100}}},
		900:  {Name: "Redist", Type: "Config", Depots: []steamtest.Depot{{ID: 104, Manifest: 1104, Chunks: 1, ChunkSize: 200}}},
		1007: {Name: "Steamworks SDK Redist", Type: "Tool", Depots: []steamtest.Depot{{ID: 1004, Manifest: 2004, OS: "windows", Chunks: 2, ChunkSize: 100}}},
	}
}

func fakeAccount() steamtest.Account {
	return steamtest.Account{Name: "gamer", SteamID: 76561198000000001, Packages: map[uint32][]uint32{
		10: {100, 150, 900}, 20: {200, 300},
	}}
}

func fakeOptions(fs *steamtest.Server) steam.Options {
	return steam.Options{DirectoryURL: fs.DirectoryURL(), Plain: true}
}

func TestFakeQRSignIn(t *testing.T) {
	fs := steamtest.New(fakeAccount(), fakeApps())
	defer fs.Close()
	fs.ConfirmAfter(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var used []string
	opt := fakeOptions(fs)
	opt.Used = func(h, p, why string) { used = append(used, p+" "+why) }
	c, err := steam.Connect(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !slices.Contains(used, "http steam-directory") || !slices.Contains(used, "ws steam-cm") {
		t.Errorf("used = %v", used)
	}
	q, err := c.BeginQR(ctx, "PiCache Prefill (test)")
	if err != nil {
		t.Fatal(err)
	}
	if q.URL() != "https://s.team/q/1/1001" || q.Interval() != time.Second {
		t.Fatalf("qr %q %s", q.URL(), q.Interval())
	}
	var res *steam.QRResult
	for i := 0; res == nil; i++ {
		if i > 5 {
			t.Fatal("no result")
		}
		if res, err = q.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		if i == 0 && (q.URL() != "https://s.team/q/1/1002" || q.Scanned()) {
			t.Errorf("after the first poll: %q scanned %v", q.URL(), q.Scanned())
		}
		if i == 1 && !q.Scanned() {
			t.Error("not scanned after the second poll")
		}
	}
	if res.AccountName != "gamer" || res.SteamID != 76561198000000001 || res.RefreshToken != fs.Token() ||
		res.TokenID != "1A2B_3C4D5E6F_7A8B9" || time.Until(res.Expires) < 199*24*time.Hour {
		t.Fatalf("result %+v", res)
	}
}

func TestFakeSession(t *testing.T) {
	fs := steamtest.New(fakeAccount(), fakeApps())
	defer fs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := steam.Connect(ctx, fakeOptions(fs))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.SignIn(ctx, steam.LogOn{AccountName: "gamer", SteamID: 76561198000000001, RefreshToken: fs.Token(),
		LoginID: 77, MachineID: "seed", MachineName: "PiCache Prefill (test)"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.LogOff()
	if sess.SteamID != 76561198000000001 || sess.CellID != 42 || sess.TokenID != 123456789 {
		t.Errorf("session %d cell %d token %d", sess.SteamID, sess.CellID, sess.TokenID)
	}
	l := fs.Logons()[0]
	if l.SteamID != 76561198000000001 || l.AccountName != "gamer" || l.LoginID != 77 || l.MachineName != "PiCache Prefill (test)" {
		t.Errorf("logon %+v", l)
	}

	f := steam.Filter{OS: []string{"windows"}, Languages: []string{"english"}}
	games, err := sess.OwnedGames(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range games {
		names = append(names, g.Name)
	}
	// Sorted by name, case-insensitive; the tool, DLC and config apps are
	// not offered.
	if !slices.Equal(names, []string{"Alpha", "beta game"}) {
		t.Fatalf("games %v", names)
	}
	alpha := games[0]
	var depots []uint32
	for _, d := range alpha.Depots {
		depots = append(depots, d.ID)
	}
	// 101 (windows), 104 (shared from the owned app 900), 151 (owned DLC);
	// not 102 (linux), 103 (german), 161 (DLC not owned).
	if !slices.Equal(depots, []uint32{101, 104, 151}) {
		t.Fatalf("depots %v", depots)
	}
	if d := alpha.Depots[1]; d.App != 900 || d.Manifest != 1104 || d.Bytes != 200 {
		t.Errorf("shared depot %+v", d)
	}
	if d := alpha.Depots[2]; d.App != 150 {
		t.Errorf("DLC depot %+v", d)
	}
	if alpha.Bytes() != 5000+200+700 {
		t.Errorf("bytes %d", alpha.Bytes())
	}
	f.Languages = []string{"english", "german"}
	g2, err := sess.Games(ctx, []uint32{100, 300, 400}, f)
	if err != nil {
		t.Fatal(err)
	}
	// A selection keeps the tool; the unowned game is dropped.
	if len(g2) != 2 || g2[0].ID != 100 || g2[1].ID != 300 || len(g2[0].Depots) != 4 {
		t.Fatalf("selected %+v", g2)
	}

	servers, err := sess.ContentServers(ctx)
	if err != nil || len(servers) != 3 || servers[1].Host != "cache1.example" || servers[1].HTTPS != "mandatory" {
		t.Fatalf("servers %+v %v", servers, err)
	}
	code, err := sess.ManifestRequestCode(ctx, 100, 101, 1101, "public")
	if err != nil || code == 0 {
		t.Fatalf("code %d %v", code, err)
	}
	if err := sess.RevokeToken(ctx, fs.Token()); err != nil {
		t.Fatal(err)
	}
	if r := fs.Revoked(); len(r) != 1 || r[0] != fs.Token() {
		t.Errorf("revoked %v", r)
	}

	// The manifest the fake serves parses into the unique chunks.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+fs.Addr()+steam.ManifestPath(101, 1101, code), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	m, err := steam.ParseManifest(b[:n])
	if err != nil {
		t.Fatal(err)
	}
	if m.Depot != 101 || m.GID != 1101 || len(m.Chunks) != 5 || m.Bytes != 5000 {
		t.Errorf("manifest depot %d gid %d chunks %d bytes %d", m.Depot, m.GID, len(m.Chunks), m.Bytes)
	}
	want := steamtest.ChunkPaths(fakeApps()[100].Depots[0])
	for i, ch := range m.Chunks {
		if ch.Path(101) != want[i] {
			t.Errorf("chunk %d: %s, want %s", i, ch.Path(101), want[i])
		}
	}
}

func TestFakeAnonymousAndRejected(t *testing.T) {
	fs := steamtest.New(fakeAccount(), fakeApps())
	defer fs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := steam.Connect(ctx, fakeOptions(fs))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.SignIn(ctx, steam.LogOn{LoginID: 1})
	if err != nil {
		t.Fatal(err)
	}
	games, err := sess.Games(ctx, []uint32{1007}, steam.Filter{OS: []string{"windows"}, Languages: []string{"english"}})
	if err != nil || len(games) != 1 || len(games[0].Depots) != 1 || games[0].Depots[0].ID != 1004 {
		t.Fatalf("anonymous games %+v %v", games, err)
	}
	sess.LogOff()

	fs.RejectToken(true)
	c, err = steam.Connect(ctx, fakeOptions(fs))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.SignIn(ctx, steam.LogOn{AccountName: "gamer", RefreshToken: fs.Token()})
	var se *steam.Error
	if !errors.As(err, &se) || !steam.TokenRejected(se.Result) {
		t.Fatalf("rejected sign-in: %v", err)
	}
}

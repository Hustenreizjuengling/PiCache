package epic_test

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/epic"
	"github.com/hustenreizjuengling/picache/internal/prefill/epic/epictest"
)

// The chunk paths of real builds (from Epic's CDN as quoted by Legendary
// and others).
func TestChunkPaths(t *testing.T) {
	v4 := epic.Chunk{GUID: [4]uint32{0x1C45C7A1, 0x49575965, 0x54012FBF, 0x7CF64B16}, Hash: 0xB347FC52BDC6D23F, Group: 82}
	if got := v4.Path(18); got != "ChunksV4/82/B347FC52BDC6D23F_1C45C7A14957596554012FBF7CF64B16.chunk" {
		t.Errorf("v4 %s", got)
	}
	for fl, dir := range map[uint32]string{2: "Chunks", 3: "ChunksV2", 6: "ChunksV3", 14: "ChunksV3", 15: "ChunksV4", 21: "ChunksV4"} {
		if !strings.HasPrefix(v4.Path(fl), dir+"/") {
			t.Errorf("feature level %d: %s", fl, v4.Path(fl))
		}
	}
	le := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	guid := func(b []byte) (g [4]uint32) {
		for i := range g {
			g[i] = binary.LittleEndian.Uint32(b[i*4:])
		}
		return g
	}
	v5 := epic.Chunk{GUID: guid(le("MenzZhGyNUm1vif1GwDvHw")), Hash: binary.LittleEndian.Uint64(le("1N5RCNMKGys")), Group: 15,
		SecretGUID: guid(le("CXn4Z7B39k6L8KE5LAJJ-Q"))}
	if got := v5.Path(22); got != "ChunksV5/CXn4Z7B39k6L8KE5LAJJ-Q/15/1N5RCNMKGys_MenzZhGyNUm1vif1GwDvHw.chunk" {
		t.Errorf("v5 %s", got)
	}
	v5.SecretGUID = [4]uint32{}
	if got := v5.Path(23); !strings.HasPrefix(got, "ChunksV5/plain/15/") {
		t.Errorf("v5 plain %s", got)
	}
}

func TestParseManifest(t *testing.T) {
	for _, fl := range []uint32{18, 22} {
		a := epictest.App{AppName: "Game", BuildVersion: "1.0", FeatureLevel: fl, Chunks: []epictest.Chunk{
			{GUID: [4]uint32{1, 2, 3, 4}, Hash: 0x1122, Group: 7, Size: 500},
			{GUID: [4]uint32{5, 6, 7, 8}, Hash: 0x3344, Group: 9, Size: 300, SecretGUID: [4]uint32{9, 9, 9, 9}},
		}}
		b := epictest.Manifest(a)
		sum := sha1.Sum(b)
		m, err := epic.ParseManifest(b, hex.EncodeToString(sum[:]))
		if err != nil {
			t.Fatalf("fl %d: %v", fl, err)
		}
		if m.FeatureLevel != fl || len(m.Chunks) != 2 || m.Bytes != 800 {
			t.Fatalf("fl %d: %+v", fl, m)
		}
		for i, c := range m.Chunks {
			if c.Path(m.FeatureLevel) != epictest.ChunkPath(a.Chunks[i], fl) {
				t.Errorf("fl %d chunk %d: %s", fl, i, c.Path(m.FeatureLevel))
			}
		}
		if _, err := epic.ParseManifest(b, strings.Repeat("0", 40)); err == nil {
			t.Error("a wrong hash was accepted")
		}
		for i := 0; i < len(b); i += 7 {
			_, _ = epic.ParseManifest(b[:i], "")
		}
	}

	// JSON manifest of an old build: blobs of 3-digit bytes.
	j := `{"ManifestFileVersion":"013000000000","ChunkHashList":{"0000000A0000000B0000000C0000000D":"034018000000000000000000"},
	 "ChunkFilesizeList":{"0000000A0000000B0000000C0000000D":"232003000000000000000000"},
	 "DataGroupList":{"0000000A0000000B0000000C0000000D":"042"}}`
	m, err := epic.ParseManifest([]byte(j), "")
	if err != nil {
		t.Fatal(err)
	}
	c := m.Chunks[0]
	if m.FeatureLevel != 13 || c.Hash != 0x1222 || c.FileSize != 1000 || c.Group != 42 || c.GUID != [4]uint32{10, 11, 12, 13} {
		t.Fatalf("json %+v %+v", m, c)
	}
	if c.Path(13) != "ChunksV3/42/0000000000001222_0000000A0000000B0000000C0000000D.chunk" {
		t.Errorf("json path %s", c.Path(13))
	}
	for _, bad := range []string{`{"ChunkHashList":{"zz":"001"}}`, `{"ChunkHashList":{"0000000A0000000B0000000C0000000D":"1"}}`, "\x0c\xc0\xbe\x44"} {
		if _, err := epic.ParseManifest([]byte(bad), ""); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFakeSignInAndLibrary(t *testing.T) {
	fs := epictest.New([]epictest.App{
		{AppName: "Alpha", Namespace: "ns1", CatalogItemID: "item1", Title: "Alpha Game", BuildVersion: "1.0", FeatureLevel: 18},
		{AppName: "AlphaDLC", Namespace: "ns1", CatalogItemID: "item2", Title: "Alpha DLC", BuildVersion: "1.0", DLC: "item1"},
		{AppName: "Plug", Namespace: "ns2", CatalogItemID: "item3", Title: "A plugin", BuildVersion: "1", Category: "plugins"},
		{AppName: "Beta", Namespace: "ns2", CatalogItemID: "item4", Title: "beta", BuildVersion: "2.0", FeatureLevel: 22},
	})
	defer fs.Close()
	c := epic.New(epic.Options{AccountURL: fs.URL(), LauncherURL: fs.URL(), CatalogURL: fs.URL()})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fs.ConfirmAfter(2)
	l, err := c.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l.UserCode != "ABCD1234" || l.VerificationURL != "https://www.epicgames.com/activate?userCode=ABCD1234" || l.Interval() != 10*time.Second {
		t.Fatalf("login %+v", l)
	}
	s, err := l.Poll(ctx)
	if err != nil || s != nil {
		t.Fatalf("first poll %v %v", s, err)
	}
	s, err = l.Poll(ctx)
	if err != nil || s == nil {
		t.Fatalf("second poll %v %v", s, err)
	}
	if s.AccountID != epictest.AccountID || s.DisplayName != epictest.DisplayName || s.RefreshToken == "" || time.Until(s.RefreshExpiresAt) < 22*24*time.Hour {
		t.Fatalf("session %+v", s)
	}
	if k := fs.Killed(); len(k) != 1 || !strings.HasPrefix(k[0], "at-98f7") {
		t.Errorf("the device client's session was not ended: %v", k)
	}
	games, known, err := c.Games(ctx, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 4 || known["item2"].Game || !known["item1"].Game || known["item1"].Title != "Alpha Game" {
		t.Fatalf("catalog answers %+v", known)
	}
	if len(games) != 2 || games[0].Name != "Alpha Game" || games[1].Name != "beta" {
		t.Fatalf("games %+v", games)
	}
	mi, err := c.Manifest(ctx, s, games[0])
	if err != nil || mi.BuildVersion != "1.0" || len(mi.URLs) != 2 || mi.URLs[0].Query().Get("cf_token") != "1_abc" {
		t.Fatalf("manifest %+v %v", mi, err)
	}
	s2, err := c.Refresh(ctx, s.RefreshToken)
	if err != nil || s2.RefreshToken == s.RefreshToken {
		t.Fatalf("refresh %+v %v", s2, err)
	}
	if _, err := c.Refresh(ctx, s.RefreshToken); !epic.Rejected(err) {
		t.Errorf("an old refresh token: %v", err)
	}
	if err := c.Kill(ctx, s2.AccessToken); err != nil {
		t.Fatal(err)
	}
}

// The catalog's answers are kept: a second reading asks for nothing; a
// rate limit is waited out; a catalog that keeps failing leaves the games
// named by their app names, with a CatalogError.
func TestCatalogCacheAndRetry(t *testing.T) {
	fs := epictest.New([]epictest.App{
		{AppName: "b65e183e8f204281b8eb1bbbb6f7a98a", Namespace: "ns1", CatalogItemID: "item1", Title: "Alpha Game", BuildVersion: "1.0"},
		{AppName: "Beta", Namespace: "ns2", CatalogItemID: "item2", Title: "Beta Game", BuildVersion: "2.0"},
		{AppName: "BetaDLC", Namespace: "ns2", CatalogItemID: "item3", Title: "Beta DLC", BuildVersion: "2.0", DLC: "item2"},
	})
	defer fs.Close()
	c := epic.New(epic.Options{AccountURL: fs.URL(), LauncherURL: fs.URL(), CatalogURL: fs.URL()})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fs.ConfirmAfter(1)
	l, err := c.StartDeviceLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Poll(ctx)
	if err != nil || s == nil {
		t.Fatalf("sign-in %v %v", s, err)
	}
	names := func(gs []epic.Game) string {
		var n []string
		for _, g := range gs {
			n = append(n, g.Name)
		}
		return strings.Join(n, ",")
	}
	// Rate limited twice: waited out.
	fs.FailCatalog(2, 429)
	games, known, err := c.Games(ctx, s, nil)
	if err != nil || names(games) != "Alpha Game,Beta Game" {
		t.Fatalf("games %s %v", names(games), err)
	}
	before := fs.CatalogRequests()
	games, _, err = c.Games(ctx, s, known)
	if err != nil || names(games) != "Alpha Game,Beta Game" || fs.CatalogRequests() != before {
		t.Fatalf("second reading: %s %v, %d catalog requests", names(games), err, fs.CatalogRequests()-before)
	}
	// Failing for good: app names, a CatalogError, nothing kept.
	fs.FailCatalog(100, 503)
	games, known, err = c.Games(ctx, s, nil)
	var ce *epic.CatalogError
	if !errors.As(err, &ce) || ce.Missing != 3 || len(known) != 0 || names(games) != "b65e183e8f204281b8eb1bbbb6f7a98a,Beta,BetaDLC" {
		t.Fatalf("failing catalog: %s %+v %v", names(games), known, err)
	}
}

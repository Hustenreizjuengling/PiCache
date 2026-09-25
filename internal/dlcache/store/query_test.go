package cachestore

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestQueries(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	putObject(t, s, "steam", "/depot/1/a", "steam:depot:1", 3000)
	putObject(t, s, "steam", "/depot/1/b", "steam:depot:1", 1000)
	d2, _ := putObject(t, s, "steam", "/depot/2/Big", "steam:depot:2", testSlice+5)
	putObject(t, s, "epicgames", "/Builds/abc/x", "epic:abc", 500)
	s.Touch(d2, 777)
	mustFlush(t, s)

	svc, err := s.Services(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(svc) != 2 || svc[0].Service != "steam" || svc[0].Objects != 3 || svc[0].Groups != 2 ||
		svc[0].CachedBytes != 4000+testSlice+5 || svc[0].BytesServed != 777 || svc[1].Service != "epicgames" {
		t.Fatalf("services %+v", svc)
	}

	keys := func(p []GroupUsage) []string {
		var out []string
		for _, g := range p {
			out = append(out, g.GroupKey)
		}
		return out
	}
	tests := []struct {
		name  string
		q     GroupQuery
		want  []string
		total int
	}{
		{"default bytes asc", GroupQuery{}, []string{"epic:abc", "steam:depot:1", "steam:depot:2"}, 3},
		{"bytes desc", GroupQuery{Sort: "bytes", Desc: true}, []string{"steam:depot:2", "steam:depot:1", "epic:abc"}, 3},
		{"name", GroupQuery{Sort: "name"}, []string{"epic:abc", "steam:depot:1", "steam:depot:2"}, 3},
		{"served desc", GroupQuery{Sort: "served", Desc: true, Limit: 1}, []string{"steam:depot:2"}, 3},
		{"page", GroupQuery{Sort: "name", Limit: 1, Offset: 1}, []string{"steam:depot:1"}, 3},
		{"service", GroupQuery{Service: "epicgames"}, []string{"epic:abc"}, 1},
		{"search case-insensitive", GroupQuery{Search: "DEPOT", Sort: "name"}, []string{"steam:depot:1", "steam:depot:2"}, 2},
		{"search by label keys", GroupQuery{Search: "fortnite", SearchKeys: []string{"epic:abc"}}, []string{"epic:abc"}, 1},
		{"search no match", GroupQuery{Search: "zzz"}, nil, 0},
		{"exact key", GroupQuery{GroupKey: "steam:depot"}, nil, 0},
		{"exact key hit", GroupQuery{GroupKey: "steam:depot:2"}, []string{"steam:depot:2"}, 1},
	}
	for _, tt := range tests {
		p, err := s.Groups(ctx, tt.q)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := keys(p.Items); !slices.Equal(got, tt.want) || p.Total != tt.total {
			t.Errorf("%s: got %v (total %d), want %v (total %d)", tt.name, got, p.Total, tt.want, tt.total)
		}
	}
	p, err := s.Groups(ctx, GroupQuery{GroupKey: "steam:depot:1", Retention: 10 * day})
	if err != nil || len(p.Items) != 1 {
		t.Fatal(err)
	}
	g := p.Items[0]
	if g.Objects != 2 || g.CachedBytes != 4000 || g.TotalBytes != 4000 || g.FirstCached.IsZero() ||
		!g.ExpiresAt.Equal(g.LastAccess.Add(10*day)) {
		t.Fatalf("group %+v", g)
	}
	for _, q := range []GroupQuery{{Sort: "bogus"}, {Offset: -1}, {Search: "a\x00b"}} {
		if _, err := s.Groups(ctx, q); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("Groups(%+v): %v", q, err)
		}
	}

	paths := func(p []Object) []string {
		var out []string
		for _, o := range p {
			out = append(out, o.Path)
		}
		return out
	}
	otests := []struct {
		name  string
		q     ObjectQuery
		want  []string
		total int
	}{
		{"group by path", ObjectQuery{GroupKey: "steam:depot:1", Sort: "path"}, []string{"/depot/1/a", "/depot/1/b"}, 2},
		{"size desc", ObjectQuery{Service: "steam", Sort: "size", Desc: true, Limit: 2}, []string{"/depot/2/Big", "/depot/1/a"}, 3},
		{"search path", ObjectQuery{Search: "big"}, []string{"/depot/2/Big"}, 1},
		{"offset", ObjectQuery{Sort: "path", Offset: 3}, []string{"/depot/2/Big"}, 4},
	}
	for _, tt := range otests {
		p, err := s.Objects(ctx, tt.q)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got := paths(p.Items); !slices.Equal(got, tt.want) || p.Total != tt.total {
			t.Errorf("%s: got %v (total %d), want %v (total %d)", tt.name, got, p.Total, tt.want, tt.total)
		}
	}
	op, err := s.Objects(ctx, ObjectQuery{Search: "Big", Retention: time.Hour})
	if err != nil || len(op.Items) != 1 {
		t.Fatal(err)
	}
	o := op.Items[0]
	if o.ID != d2 || o.Total != testSlice+5 || o.CachedBytes != testSlice+5 || o.SliceCount != 2 || o.SlicesTotal != 2 ||
		o.Hits != 1 || o.BytesServed != 777 || o.ContentType != "application/x-test" || o.Host != "cdn.example.com" ||
		!o.ExpiresAt.Equal(o.LastAccess.Add(time.Hour)) || o.CreatedAt.IsZero() || o.SliceSize != testSlice {
		t.Fatalf("object %+v", o)
	}
	if _, err := s.Objects(ctx, ObjectQuery{Sort: "id; DROP TABLE store_objects"}); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("unknown sort: %v", err)
	}
	if p, err := s.Objects(ctx, ObjectQuery{Limit: 100000}); err != nil || len(p.Items) != 4 {
		t.Fatalf("clamped limit: %v", err)
	}
}

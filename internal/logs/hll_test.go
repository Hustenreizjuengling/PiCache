package logs

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// The estimate is within three standard errors (1.04/√2048 ≈ 2.3 %) for
// 1 000 and 100 000 distinct names; small cardinalities are exact enough to
// show (linear counting).
func TestHLLAccuracy(t *testing.T) {
	se := 1.04 / math.Sqrt(hllRegisters)
	for _, n := range []int{1000, 100_000} {
		var h hll
		for i := range n {
			h.add(hashName(fmt.Sprintf("host-%d.example.com", i)))
		}
		for i := range n / 2 { // duplicates change nothing
			h.add(hashName(fmt.Sprintf("host-%d.example.com", i)))
		}
		est := float64(h.estimate())
		if rel := math.Abs(est-float64(n)) / float64(n); rel > 3*se {
			t.Errorf("n=%d: estimate %.0f, error %.2f %% > %.2f %%", n, est, rel*100, 3*se*100)
		}
	}
	for n := range 101 {
		var h hll
		for i := range n {
			h.add(hashName(fmt.Sprintf("s%d.example", i)))
		}
		// Linear counting: two names sharing a register are counted as
		// expected on average, so small counts are off by at most a few.
		if got := h.estimate(); math.Abs(float64(got-int64(n))) > max(2, float64(n)*0.05) || (n <= 1 && got != int64(n)) {
			t.Errorf("n=%d: estimate %d", n, got)
		}
	}
}

// Merging is the union: overlapping sets are not counted twice.
func TestHLLMergeIsUnion(t *testing.T) {
	var a, b, all hll
	for i := range 30_000 {
		x := hashName(fmt.Sprintf("n%d", i))
		if i < 20_000 {
			a.add(x)
		}
		if i >= 10_000 {
			b.add(x)
		}
		all.add(x)
	}
	a.merge(&b)
	if a != all {
		t.Fatal("merged sketch differs from the sketch of the union")
	}
	if est := a.estimate(); math.Abs(float64(est)-30_000)/30_000 > 0.07 {
		t.Fatalf("union estimate %d", est)
	}
}

func TestHLLEncodeDecode(t *testing.T) {
	var h hll
	for i := range 5000 {
		h.add(hashName(fmt.Sprint(i)))
	}
	got, ok := decodeHLL(h.encode())
	if !ok || *got != h {
		t.Fatal("round trip failed")
	}
	for _, bad := range []string{"", "AAAA", strings.Repeat("A", 2732), h.encode()[:2731] + "!", h.encode() + "A"} {
		if _, ok := decodeHLL(bad); ok {
			t.Errorf("accepted %q…", bad[:min(len(bad), 12)])
		}
	}
	var big hll
	big[7] = hllMaxRank + 1
	if _, ok := decodeHLL(big.encode()); ok {
		t.Error("accepted a register beyond the maximum rank")
	}
}

// decodeHLL reads labels of logs.db rows: it never panics and accepts only
// exactly the registers of a sketch.
func FuzzDecodeHLL(f *testing.F) {
	var h hll
	h.add(1)
	f.Add(h.encode())
	f.Add("")
	f.Add(strings.Repeat("/", 2732))
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := decodeHLL(s)
		if !ok {
			return
		}
		if got.encode() != s {
			t.Fatalf("accepted a non-canonical encoding %q", s)
		}
		for _, v := range got {
			if v > hllMaxRank {
				t.Fatalf("register %d", v)
			}
		}
		_ = got.estimate()
	})
}

// Query types: at most 32 per hour, further types under OTHER; the range
// result is most first, then by name.
func TestQTypesCapAndOther(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	a := query(now, "10.0.0.1", "x.example", "forwarded")
	for range 50 {
		s.w.addQuery(a)
	}
	for i := range 40 {
		q := query(now, "10.0.0.1", "x.example", "forwarded")
		q.QType = fmt.Sprintf("TYPE%d", 100+i)
		for range 1 + i%3 {
			s.w.addQuery(q)
		}
	}
	if m := s.top.dns[dnsTopQType]; len(m) != maxQTypeKeys+1 || m[qtypeOther] == nil || m["A"] == nil {
		t.Fatalf("in-memory qtypes %d, OTHER %v", len(m), m[qtypeOther])
	}
	st, err := s.QTypes(context.Background(), now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for i, q := range st.QTypes {
		total += q.Count
		if i > 0 && (q.Count > st.QTypes[i-1].Count || (q.Count == st.QTypes[i-1].Count && q.QType < st.QTypes[i-1].QType)) {
			t.Fatalf("order %v", st.QTypes)
		}
	}
	// 40 types counted 1, 2, 3, 1, 2, 3, … times: 26 + 26 + 27 = 79, plus 50 A.
	if total != 129 || st.QTypes[0].QType != "A" || st.QTypes[0].Count != 50 || len(st.QTypes) > maxQTypeKeys+1 {
		t.Fatalf("qtypes %v (total %d)", st.QTypes, total)
	}
	if !st.From.Equal(TopFrom(now.Add(-time.Hour), now.Add(time.Minute))) {
		t.Fatalf("from %v", st.From)
	}
	empty, err := s.QTypes(context.Background(), now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	if err != nil || empty.QTypes == nil || len(empty.QTypes) != 0 {
		t.Fatalf("empty range %+v, %v", empty, err)
	}
}

// The upstream list has the average duration in avgDurationUs and, for
// compatibility, in bytes.
func TestUpstreamAverageDuration(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Now()
	for _, d := range []int64{1000, 3000} {
		q := query(now, "10.0.0.1", "x.example", "forwarded")
		q.Upstream, q.DurationUs = "tls://dns.example", d
		s.w.addQuery(q)
	}
	items, err := s.Top(context.Background(), TopUpstreams, now.Add(-time.Hour), now.Add(time.Minute), 5)
	if err != nil || len(items) != 1 || items[0].AvgDurationUs != 2000 || items[0].Bytes != 2000 || items[0].Count != 2 {
		t.Fatalf("upstreams %+v, %v", items, err)
	}
}

package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// TestRealStore runs the cache path against a real cachestore.Store in a
// temp directory: miss → stored slices → hit (WriteRange from slice files)
// → multi-range from disk → forced refetch.
func TestRealStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "store")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	const storeID = "0123456789abcdef0123456789abcdef"
	const sliceSize = 256 << 10
	if _, err := cachestore.InitRoot(root, storeID, sliceSize); err != nil {
		t.Fatal(err)
	}
	st, err := cachestore.Open(ctx, cachestore.Options{
		Root: root, IndexPath: filepath.Join(dir, "index.db"), StoreID: storeID, Log: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // after the proxy stopped (cleanups run in reverse)
	h := newHarness(t, withSliceStore(st))
	data := testData(600 << 10) // three slices, the last one short
	const path = "/Builds/Org/o-abc/0123456789abcdef0123456789abcdef/file.pak"
	h.origin.set(path, &originObj{data: data})
	id := cachestore.ObjectID(testService, path)

	resp, body := h.get("GET", testHost, path+"?sig=secret", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || resp.Header.Get(cacheStatusHeader) != statusMiss {
		t.Fatalf("miss: %d %q", resp.StatusCode, resp.Header.Get(cacheStatusHeader))
	}
	eventually(t, func() bool {
		hd, ok, err := st.Head(ctx, id)
		return err == nil && ok && hd.Has(0) && hd.Has(1) && hd.Has(2)
	})
	hd, _, _ := st.Head(ctx, id)
	if hd.Total != int64(len(data)) || hd.ContentType != "application/x-test" || hd.Header.Get("Etag") != "" || hd.Header.Get("X-Keep") != "kept" {
		t.Fatalf("head %+v", hd)
	}
	upstream := len(h.origin.requests())

	resp, body = h.get("GET", testHost, path, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) || resp.Header.Get(cacheStatusHeader) != statusHit {
		t.Fatalf("hit: %d %q", resp.StatusCode, resp.Header.Get(cacheStatusHeader))
	}
	resp, body = h.get("GET", testHost, path, hdr("Range", "bytes=300000-300099,600000-"))
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusPartialContent || err != nil {
		t.Fatalf("multi-range: %d %v", resp.StatusCode, err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for _, want := range [][]byte{data[300000:300100], data[600000:]} {
		p, err := mr.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(p); !bytes.Equal(got, want) {
			t.Fatal("wrong part")
		}
	}
	if len(h.origin.requests()) != upstream {
		t.Fatal("cached object went upstream")
	}
	evs := h.events(3)
	if ev := evs[1]; ev.CacheStatus != statusHit || ev.BytesHit != int64(len(data)) || ev.BytesWAN != 0 {
		t.Fatalf("hit event %+v", ev)
	}
	if ev := evs[0]; ev.BytesStored != int64(len(data)) || ev.Path != path || ev.GroupKey != services.GroupFor(testService, testHost, path).Key || !strings.HasPrefix(ev.GroupKey, "epic:") {
		t.Fatalf("miss event %+v", ev)
	}

	// A changed object on a forced refetch gets a new generation.
	newData := bytes.Repeat([]byte("n"), 300<<10)
	h.origin.set(path, &originObj{data: newData})
	h.updateSettings(func(a *settings.All) { a.LanCache.NocacheClients = []string{"127.0.0.1/32"} })
	if _, body := h.get("GET", testHost, path+"?nocache=1", nil); !bytes.Equal(body, newData) {
		t.Fatal("refetch served old data")
	}
	eventually(t, func() bool {
		hd, ok, _ := st.Head(ctx, id)
		return ok && hd.Total == int64(len(newData)) && hd.Has(0) && hd.Has(1) && hd.Gen != 0
	})
	if _, body := h.get("GET", testHost, path, nil); !bytes.Equal(body, newData) {
		t.Fatal("new generation not served")
	}
}

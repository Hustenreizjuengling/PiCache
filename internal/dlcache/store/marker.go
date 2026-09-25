package cachestore

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// MarkerFile is the store marker at the root of every store.
const MarkerFile = ".picache-store"

// ErrNoMarker is returned by ReadMarker when the root is not initialised.
var ErrNoMarker = errors.New("cachestore: store not initialised (no .picache-store marker)")

var storeIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidStoreID reports whether id is a well-formed store id.
func ValidStoreID(id string) bool { return storeIDRE.MatchString(id) }

// Marker is the content of .picache-store.
type Marker struct {
	StoreID   string    `json:"storeId"`
	Format    int       `json:"format"`
	SliceSize int64     `json:"sliceSize"`
	CreatedAt time.Time `json:"createdAt"`
}

func (m Marker) validate() error {
	if !ValidStoreID(m.StoreID) {
		return errors.New("invalid store id in marker")
	}
	if m.Format != 1 {
		return fmt.Errorf("unsupported store format %d", m.Format)
	}
	if m.SliceSize < 256<<10 || m.SliceSize > 64<<20 || m.SliceSize&(m.SliceSize-1) != 0 {
		return fmt.Errorf("invalid slice size %d in marker", m.SliceSize)
	}
	return nil
}

// ReadMarker reads and validates the marker of root (max 4 KiB).
func ReadMarker(root string) (Marker, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return Marker{}, err
	}
	defer r.Close()
	f, err := r.Open(MarkerFile)
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, ErrNoMarker
	}
	if err != nil {
		return Marker{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return Marker{}, err
	}
	if len(b) > 4096 {
		return Marker{}, errors.New("store marker too large")
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, fmt.Errorf("corrupt store marker: %w", err)
	}
	if err := m.validate(); err != nil {
		return Marker{}, err
	}
	return m, nil
}

// InitRoot writes a new marker into root. It fails if a marker already
// exists, or (unless allowNonEmpty) if root contains anything else.
func InitRoot(root, storeID string, sliceSize int64) (Marker, error) {
	m := Marker{StoreID: storeID, Format: 1, SliceSize: sliceSize, CreatedAt: time.Now().UTC()}
	if err := m.validate(); err != nil {
		return Marker{}, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return Marker{}, err
	}
	defer r.Close()
	if _, err := r.Stat(MarkerFile); err == nil {
		return Marker{}, errors.New("store already initialised")
	}
	d, err := r.Open(".")
	if err != nil {
		return Marker{}, err
	}
	names, err := d.Readdirnames(8)
	d.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return Marker{}, err
	}
	for _, n := range names {
		if n != "lost+found" && n != "tmp" && n != "slices" {
			return Marker{}, fmt.Errorf("directory is not empty (found %q); choose an empty directory or adopt an existing store", n)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Marker{}, err
	}
	tmp := MarkerFile + ".tmp"
	if err := r.WriteFile(tmp, b, 0o640); err != nil {
		return Marker{}, err
	}
	if err := r.Rename(tmp, MarkerFile); err != nil {
		_ = r.Remove(tmp)
		return Marker{}, err
	}
	return m, nil
}

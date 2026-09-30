//go:build race

package filter

// raceEnabled: the race detector is on (go test -race). Allocation counts
// are not meaningful then: sync.Pool (used by regexp) drops a random share
// of the items put back, so a pooled matcher is allocated again.
const raceEnabled = true

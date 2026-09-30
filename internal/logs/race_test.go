//go:build race

package logs

// raceEnabled: the race detector is on (go test -race); timing tests are
// skipped, it slows SQLite down many times.
const raceEnabled = true

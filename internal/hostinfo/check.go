package hostinfo

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Thresholds of the health check "host" (settings health).
type Thresholds struct {
	MemoryAvailableMinPercent int // warn below this share of available memory
	LoadPerCPUMax             int // warn above this 15-minute load per CPU
	TemperatureMaxCelsius     int // warn at or above this temperature
}

// Check evaluates the health check "host" (warn only): available memory
// below the threshold (the lower of the host's and the cgroup's share),
// the 15-minute load above the threshold per CPU, a temperature at or
// above the threshold. The message names every exceeded value with its
// threshold. show is false when no value could be read (the check is then
// left out).
func (in *Info) Check(t Thresholds) (status, msg string, show bool) {
	if !in.Values() {
		return "", "", false
	}
	var problems []string
	pct, what := -1.0, ""
	if m := in.Memory; m != nil && m.TotalBytes > 0 {
		pct, what = float64(m.AvailableBytes)*100/float64(m.TotalBytes), "memory available"
	}
	if c := in.Cgroup; c != nil && c.LimitBytes > 0 {
		if p := float64(c.AvailableBytes) * 100 / float64(c.LimitBytes); pct < 0 || p < pct {
			pct, what = p, "memory available to PiCache's cgroup"
		}
	}
	if pct >= 0 && pct < float64(t.MemoryAvailableMinPercent) {
		problems = append(problems, fmt.Sprintf("%s %s %% (below %d %%)", what, number(pct), t.MemoryAvailableMinPercent))
	}
	if l := in.Load; l != nil && in.CPUs > 0 {
		if limit := t.LoadPerCPUMax * in.CPUs; l.Fifteen > float64(limit) {
			problems = append(problems, fmt.Sprintf("15-minute load %s (above %d = %d per CPU × %d CPUs)",
				number(l.Fifteen), limit, t.LoadPerCPUMax, in.CPUs))
		}
	}
	for _, z := range in.Temperatures {
		if z.Celsius >= float64(t.TemperatureMaxCelsius) {
			name := z.Zone
			if z.Type != "" {
				name += " (" + z.Type + ")"
			}
			problems = append(problems, fmt.Sprintf("%s %s °C (at or above %d °C)", name, number(z.Celsius), t.TemperatureMaxCelsius))
		}
	}
	if len(problems) == 0 {
		return "ok", "", true
	}
	return "warn", strings.Join(problems, "; "), true
}

// number formats a value with at most one decimal.
func number(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

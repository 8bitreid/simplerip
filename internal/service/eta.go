package service

import "time"

// etaTracker estimates time remaining from recent progress. It uses a sliding
// window so the estimate follows the drive's current speed (which varies across
// a disc) instead of the whole-rip average.
type etaTracker struct {
	samples []etaSample
}

type etaSample struct {
	at  time.Time
	pct int
}

const (
	etaWindow    = 90 * time.Second
	etaMinSpan   = 15 * time.Second // need this much history before showing an estimate
	etaMinPctRun = 1                // and at least this much progress across it
)

// Update records overall progress (0-100) at time now and returns the estimated
// time remaining, or 0 if there isn't enough data yet.
func (t *etaTracker) Update(now time.Time, pct int) time.Duration {
	if n := len(t.samples); n > 0 && pct < t.samples[n-1].pct {
		t.samples = nil // progress went backwards (e.g. retry); start over
	}
	t.samples = append(t.samples, etaSample{now, pct})

	cut := 0
	for cut < len(t.samples)-1 && now.Sub(t.samples[cut+1].at) >= etaWindow {
		cut++
	}
	t.samples = t.samples[cut:]

	first := t.samples[0]
	span := now.Sub(first.at)
	gained := pct - first.pct
	if span < etaMinSpan || gained < etaMinPctRun || pct >= 100 {
		return 0
	}
	perPct := span / time.Duration(gained)
	return perPct * time.Duration(100-pct)
}

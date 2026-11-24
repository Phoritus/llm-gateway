package main

import (
	"testing"
	"time"
)

func TestShutdownEndsInsideItsBudget(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 25s is the default and 5s the least config.Load accepts.
	for _, budget := range []time.Duration{25 * time.Second, 5 * time.Second, 290 * time.Second} {
		p := planShutdown(now, budget)
		if !p.stopBy.Equal(now.Add(budget)) {
			t.Errorf("budget %s: the last step ends at +%s", budget, p.stopBy.Sub(now))
		}
		if !now.Before(p.deregisterBy) || !p.deregisterBy.Before(p.drainBy) || !p.drainBy.Before(p.flushBy) || !p.flushBy.Before(p.stopBy) {
			t.Errorf("budget %s: deregister +%s, drain +%s, flush +%s, stop +%s are not in order after the signal",
				budget, p.deregisterBy.Sub(now), p.drainBy.Sub(now), p.flushBy.Sub(now), p.stopBy.Sub(now))
		}
		if budget >= 25*time.Second && p.deregisterBy.Sub(now) < 2*time.Second {
			t.Errorf("budget %s: deregistering for only %s, under two of the load balancer's 1s polls", budget, p.deregisterBy.Sub(now))
		}
	}
}

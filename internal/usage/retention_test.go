package usage

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakePruner struct {
	mu      sync.Mutex
	cutoffs []time.Time
}

func (p *fakePruner) PruneEvents(_ context.Context, before time.Time, _ int) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cutoffs = append(p.cutoffs, before)
	return 0, nil
}

func TestRetentionPrunesEventsOlderThanTheWindowOnEachTick(t *testing.T) {
	p := &fakePruner{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunRetention(ctx, p, 48*time.Hour, 10*time.Millisecond, nil)
		close(done)
	}()
	eventually(t, "two prunes", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.cutoffs) >= 2
	})
	cancel()
	<-done

	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.cutoffs {
		if age := time.Since(c); age < 48*time.Hour || age > 49*time.Hour {
			t.Errorf("cutoff is %s old, want the 48h window", age)
		}
	}
}

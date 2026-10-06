package cache

import (
	"sync"
	"time"
)

type popularityTracker struct {
	mu      sync.RWMutex
	counts  map[string]*accessCounter
	window  time.Duration
	closeCh chan struct{}
}

type accessCounter struct {
	timestamps []int64
}

func newPopularityTracker(window time.Duration) *popularityTracker {
	p := &popularityTracker{
		counts:  make(map[string]*accessCounter),
		window:  window,
		closeCh: make(chan struct{}),
	}
	go p.cleanupLoop()
	return p
}

func (p *popularityTracker) cleanupLoop() {
	ticker := time.NewTicker(p.window)
	defer ticker.Stop()

	for {
		select {
		case <-p.closeCh:
			return
		case <-ticker.C:
			p.cleanup()
		}
	}
}

func (p *popularityTracker) cleanup() {
	p.mu.Lock()
	defer p.mu.Unlock()

	cutoff := time.Now().UnixNano() - p.window.Nanoseconds()
	for key, ac := range p.counts {
		valid := ac.timestamps[:0]
		for _, ts := range ac.timestamps {
			if ts > cutoff {
				valid = append(valid, ts)
			}
		}
		if len(valid) == 0 {
			delete(p.counts, key)
		} else {
			ac.timestamps = valid
		}
	}
}

func (p *popularityTracker) Close() {
	close(p.closeCh)
}

// Record logs an access for key and trims expired timestamps. The maxCount
// parameter caps retained timestamps to 2*maxCount (minimum 16) so per-endpoint
// thresholds higher than the global default are still reachable.
func (p *popularityTracker) Record(key string, maxCount int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UnixNano()
	cutoff := now - p.window.Nanoseconds()
	maxTimestamps := max(maxCount*2, 16)

	ac, ok := p.counts[key]
	if !ok {
		ac = &accessCounter{timestamps: make([]int64, 0, 8)}
		p.counts[key] = ac
	}

	valid := ac.timestamps[:0]
	for _, ts := range ac.timestamps {
		if ts > cutoff {
			valid = append(valid, ts)
		}
	}

	valid = append(valid, now)

	if len(valid) > maxTimestamps {
		valid = valid[len(valid)-maxTimestamps:]
	}

	ac.timestamps = valid
}

func (p *popularityTracker) Count(key string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ac, ok := p.counts[key]
	if !ok {
		return 0
	}

	cutoff := time.Now().UnixNano() - p.window.Nanoseconds()
	count := 0
	for _, ts := range ac.timestamps {
		if ts > cutoff {
			count++
		}
	}
	return count
}

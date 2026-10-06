package cache

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetch_CacheHit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		return &Response{StatusCode: 200, Body: []byte(`{"test": "data"}`)}, nil
	}

	// First fetch - miss
	r1, err := c.Fetch("key1", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if r1.Source != "upstream" {
		t.Errorf("Source = %s, want upstream", r1.Source)
	}

	time.Sleep(10 * time.Millisecond)

	// Second fetch - hit
	r2, err := c.Fetch("key1", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if r2.Source != "cache" {
		t.Errorf("Source = %s, want cache", r2.Source)
	}
	if r2.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", r2.StatusCode)
	}

	if fetchCount.Load() != 1 {
		t.Errorf("Fetch count = %d, want 1", fetchCount.Load())
	}
}

func TestFetch_HeadersPassthrough(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	fetch := func() (*Response, error) {
		return &Response{
			StatusCode: 200,
			Body:       []byte(`{}`),
			Headers:    map[string]string{"x-rate-limit-remaining": "100"},
		}, nil
	}

	// Upstream - headers present
	r1, _ := c.Fetch("key-headers", "TestEndpoint", fetch)
	if r1.Headers["x-rate-limit-remaining"] != "100" {
		t.Error("Headers not passed through on upstream")
	}

	time.Sleep(10 * time.Millisecond)

	// Cache hit - headers nil
	r2, _ := c.Fetch("key-headers", "TestEndpoint", fetch)
	if r2.Headers != nil {
		t.Error("Headers should be nil on cache hit")
	}
}

func TestFetch_Coalescing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0
	cfg.Whitelist = []string{"TestEndpoint"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		time.Sleep(50 * time.Millisecond)
		return &Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 10 {
		wg.Go(func() {
			<-start
			c.Fetch("coalesce-key", "TestEndpoint", fetch)
		})
	}
	close(start)
	wg.Wait()

	if count := fetchCount.Load(); count != 1 {
		t.Errorf("Fetch count = %d, want 1", count)
	}
}

func TestFetch_StaleIfError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0
	cfg.EnableStaleIfError = true
	cfg.StaleIfErrorWindow = 10 * time.Minute
	cfg.Whitelist = []string{"TestEndpoint"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	// Store an entry that will become stale
	e := &entry{
		Status:   200,
		Body:     []byte(`{"original": true}`),
		CachedAt: time.Now().Add(-6 * time.Minute).UnixNano(),
		TTL:      5 * time.Minute,
		Endpoint: "TestEndpoint",
	}
	c.set("stale-key", e)
	time.Sleep(10 * time.Millisecond)

	// Fetch with error - should serve stale
	fetch := func() (*Response, error) {
		return nil, fmt.Errorf("upstream error")
	}

	result, err := c.Fetch("stale-key", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Should have served stale, got error: %v", err)
	}
	if result.Source != "stale" {
		t.Errorf("Source = %s, want stale", result.Source)
	}
}

func TestFetch_NegativeCaching(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0
	cfg.EnableNegativeCaching = true
	cfg.NegativeCacheTTL = 2 * time.Minute
	cfg.Whitelist = []string{"TestEndpoint"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		return &Response{StatusCode: http.StatusNotFound, Body: []byte(`{}`)}, nil
	}

	c.Fetch("negative-key", "TestEndpoint", fetch)
	time.Sleep(10 * time.Millisecond)
	result, _ := c.Fetch("negative-key", "TestEndpoint", fetch)

	if result.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", result.StatusCode)
	}
	if fetchCount.Load() != 1 {
		t.Errorf("Fetch count = %d, want 1 (404 should be cached)", fetchCount.Load())
	}
}

func TestFetch_ErrorNotCached(t *testing.T) {
	c := testCache(t)

	for _, code := range []int{401, 403, 429, 500, 502, 503} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var n atomic.Int32
			fetch := func() (*Response, error) {
				n.Add(1)
				return &Response{StatusCode: code, Body: []byte(`{"error": true}`)}, nil
			}

			key := fmt.Sprintf("err-%d", code)
			c.Fetch(key, "TestEndpoint", fetch)
			time.Sleep(10 * time.Millisecond)

			r, _ := c.Fetch(key, "TestEndpoint", fetch)
			if r.Source == "cache" {
				t.Errorf("error %d should not be cached", code)
			}
			if n.Load() != 2 {
				t.Errorf("fetch count = %d, want 2", n.Load())
			}
		})
	}
}

func TestFetch_ErrorServesStale(t *testing.T) {
	for _, code := range []int{429, 403} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := testCache(t, func(cfg *Config) {
				cfg.EnableStaleIfError = true
				cfg.StaleIfErrorWindow = 10 * time.Minute
			})

			key := fmt.Sprintf("stale-%d", code)
			seedStale(c, key)
			time.Sleep(10 * time.Millisecond)

			fetch := func() (*Response, error) {
				return &Response{StatusCode: code, Body: []byte(`{"error": true}`)}, nil
			}

			result, err := c.Fetch(key, "TestEndpoint", fetch)
			if err != nil {
				t.Fatalf("should have served stale, got error: %v", err)
			}
			if result.Source != "stale" {
				t.Errorf("source = %s, want stale", result.Source)
			}
			if result.StatusCode != 200 {
				t.Errorf("status = %d, want 200", result.StatusCode)
			}
		})
	}
}

func TestFetch_ErrorPassthroughWithoutStale(t *testing.T) {
	c := testCache(t, func(cfg *Config) { cfg.EnableStaleIfError = false })

	for _, code := range []int{401, 403, 429, 500} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			fetch := func() (*Response, error) {
				return &Response{StatusCode: code, Body: []byte(`{"error": true}`)}, nil
			}

			result, _ := c.Fetch(fmt.Sprintf("no-stale-%d", code), "TestEndpoint", fetch)
			if result.StatusCode != code {
				t.Errorf("status = %d, want %d", result.StatusCode, code)
			}
			if result.Source != "upstream" {
				t.Errorf("source = %s, want upstream", result.Source)
			}
		})
	}
}

func TestPopularityThreshold_DefaultRejectsFirstRequest(t *testing.T) {
	// Default threshold=1 means "accessed 1 time before becoming eligible".
	// 1st request: rejected (no previous accesses)
	// 2nd request: upstream again, but now admitted into cache
	// 3rd request: cache hit
	c := testCache(t, func(cfg *Config) {
		cfg.PopularityThreshold = 1
	})

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		return &Response{StatusCode: 200, Body: []byte(`{"data": true}`)}, nil
	}

	// 1st request: miss, not yet popular enough to cache
	r1, err := c.Fetch("pop-key", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if r1.Source != "upstream" {
		t.Errorf("1st request: source = %s, want upstream", r1.Source)
	}

	time.Sleep(10 * time.Millisecond)

	// 2nd request: miss again, but this time the response IS cached
	r2, err := c.Fetch("pop-key", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if r2.Source != "upstream" {
		t.Errorf("2nd request: source = %s, want upstream", r2.Source)
	}

	time.Sleep(10 * time.Millisecond)

	// 3rd request: cache hit
	r3, err := c.Fetch("pop-key", "TestEndpoint", fetch)
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if r3.Source != "cache" {
		t.Errorf("3rd request: source = %s, want cache", r3.Source)
	}

	if fetchCount.Load() != 2 {
		t.Errorf("fetch count = %d, want 2", fetchCount.Load())
	}
}

func TestPopularityThreshold_ZeroCachesImmediately(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.PopularityThreshold = 0
	})

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		return &Response{StatusCode: 200, Body: []byte(`{"data": true}`)}, nil
	}

	r1, _ := c.Fetch("imm-key", "TestEndpoint", fetch)
	if r1.Source != "upstream" {
		t.Errorf("1st request: source = %s, want upstream", r1.Source)
	}

	time.Sleep(10 * time.Millisecond)

	r2, _ := c.Fetch("imm-key", "TestEndpoint", fetch)
	if r2.Source != "cache" {
		t.Errorf("2nd request: source = %s, want cache", r2.Source)
	}

	if fetchCount.Load() != 1 {
		t.Errorf("fetch count = %d, want 1", fetchCount.Load())
	}
}

func TestPopularityThreshold_EndpointOverride(t *testing.T) {
	// Global threshold=0 (cache immediately), but SearchTimeline=3
	// means SearchTimeline needs 3 previous accesses before caching.
	c := testCache(t, func(cfg *Config) {
		cfg.PopularityThreshold = 0
		cfg.EndpointThresholds = map[string]int{"SearchTimeline": 3}
		cfg.Whitelist = []string{"TestEndpoint", "SearchTimeline"}
	})

	var fetchCount atomic.Int32
	fetch := func() (*Response, error) {
		fetchCount.Add(1)
		return &Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}

	// Requests 1-3: all rejected for SearchTimeline
	for i := 1; i <= 3; i++ {
		r, _ := c.Fetch("search-key", "SearchTimeline", fetch)
		if r.Source != "upstream" {
			t.Errorf("request %d: source = %s, want upstream", i, r.Source)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Request 4: still upstream, but this one gets cached
	r4, _ := c.Fetch("search-key", "SearchTimeline", fetch)
	if r4.Source != "upstream" {
		t.Errorf("request 4: source = %s, want upstream", r4.Source)
	}

	time.Sleep(10 * time.Millisecond)

	// Request 5: cache hit
	r5, _ := c.Fetch("search-key", "SearchTimeline", fetch)
	if r5.Source != "cache" {
		t.Errorf("request 5: source = %s, want cache", r5.Source)
	}

	if fetchCount.Load() != 4 {
		t.Errorf("fetch count = %d, want 4", fetchCount.Load())
	}
}

func TestPopularityThreshold_AdmissionMetrics(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.PopularityThreshold = 1
	})

	fetch := func() (*Response, error) {
		return &Response{StatusCode: 200, Body: []byte(`{}`)}, nil
	}

	// 1st request: should be rejected
	c.Fetch("metrics-key", "TestEndpoint", fetch)
	if c.metrics.AdmissionRejected.Load() != 1 {
		t.Errorf("admission_rejected = %d, want 1", c.metrics.AdmissionRejected.Load())
	}
	if c.metrics.AdmissionAccepted.Load() != 0 {
		t.Errorf("admission_accepted = %d, want 0", c.metrics.AdmissionAccepted.Load())
	}

	time.Sleep(10 * time.Millisecond)

	// 2nd request: should be accepted
	c.Fetch("metrics-key", "TestEndpoint", fetch)
	if c.metrics.AdmissionRejected.Load() != 1 {
		t.Errorf("admission_rejected = %d, want 1", c.metrics.AdmissionRejected.Load())
	}
	if c.metrics.AdmissionAccepted.Load() != 1 {
		t.Errorf("admission_accepted = %d, want 1", c.metrics.AdmissionAccepted.Load())
	}
}

func TestWhitelist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.Whitelist = []string{"UserByScreenName", "TweetDetail"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	if !c.IsCacheable("UserByScreenName") {
		t.Error("IsCacheable(UserByScreenName) = false, want true")
	}
	if c.IsCacheable("SearchTimeline") {
		t.Error("IsCacheable(SearchTimeline) = true, want false")
	}
}

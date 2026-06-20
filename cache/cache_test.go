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

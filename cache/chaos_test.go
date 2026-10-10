package cache

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestChaos_404BurstNeverCached hammers the same key with 404s from many
// goroutines. Every response must come from upstream, never cache.
func TestChaos_404BurstNeverCached(t *testing.T) {
	c := testCache(t)

	var upstreamCalls atomic.Int32
	fetch := func() (*Response, error) {
		upstreamCalls.Add(1)
		time.Sleep(time.Duration(rand.IntN(5)) * time.Millisecond)
		return &Response{StatusCode: 404, Body: []byte(`{}`)}, nil
	}

	const goroutines = 50
	const requestsPerG = 20
	var wg sync.WaitGroup
	var cacheHits atomic.Int32

	for range goroutines {
		wg.Go(func() {
			for range requestsPerG {
				r, err := c.Fetch("burst-404", "TestEndpoint", fetch)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if r.Source == "cache" {
					cacheHits.Add(1)
				}
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
			}
		})
	}
	wg.Wait()

	if hits := cacheHits.Load(); hits != 0 {
		t.Errorf("got %d cache hits for 404 responses, want 0", hits)
	}
	// Singleflight will coalesce some, so upstream calls <= total requests.
	// But we must have more than 1 (not all coalesced into a single cached response).
	if calls := upstreamCalls.Load(); calls < 2 {
		t.Errorf("upstream calls = %d, suspiciously low (404 might be cached)", calls)
	}
}

// TestChaos_FlappingUpstream alternates between 200 and 404 for the same key.
// The cache must never serve a 404 from cache, and 200s must cache normally.
func TestChaos_FlappingUpstream(t *testing.T) {
	c := testCache(t)

	var callCount atomic.Int32
	// Alternate: 200, 404, 200, 404, ...
	fetch := func() (*Response, error) {
		n := callCount.Add(1)
		time.Sleep(2 * time.Millisecond)
		if n%2 == 0 {
			return &Response{StatusCode: 404, Body: []byte(`{"gone": true}`)}, nil
		}
		return &Response{StatusCode: 200, Body: []byte(`{"user": "alive"}`)}, nil
	}

	for i := range 30 {
		r, err := c.Fetch("flap-key", "TestEndpoint", fetch)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		// A 404 from cache means we incorrectly stored one.
		if r.StatusCode == 404 && r.Source == "cache" {
			t.Fatalf("request %d: got 404 from cache, 404s must never be cached", i)
		}
		// Wait for TTL to expire between some requests to force re-fetches.
		if i%5 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestChaos_200ThenSudden404 seeds a valid 200, lets it expire, then
// upstream starts returning 404. With stale-if-error, the stale 200
// should be served. Without it, the 404 passes through.
func TestChaos_200ThenSudden404(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.EnableStaleIfError = true
		cfg.StaleIfErrorWindow = 1 * time.Minute
		cfg.DefaultTTL = 20 * time.Millisecond
	})

	// Phase 1: seed a 200.
	fetch200 := func() (*Response, error) {
		return &Response{StatusCode: 200, Body: []byte(`{"user": "exists"}`)}, nil
	}
	r1, _ := c.Fetch("sudden-key", "TestEndpoint", fetch200)
	if r1.StatusCode != 200 {
		t.Fatalf("seed: got %d, want 200", r1.StatusCode)
	}

	// Wait for the 200 to go stale (but within the stale-if-error window).
	time.Sleep(30 * time.Millisecond)

	// Phase 2: upstream now returns 404. The stale 200 should be served.
	fetch404 := func() (*Response, error) {
		return &Response{StatusCode: 404, Body: []byte(`{"error": "not found"}`)}, nil
	}
	for i := range 10 {
		r, err := c.Fetch("sudden-key", "TestEndpoint", fetch404)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if r.StatusCode != 200 || r.Source != "stale" {
			t.Errorf("request %d: status=%d source=%s, want 200/stale", i, r.StatusCode, r.Source)
		}
	}
}

// TestChaos_AllNon2xxCodes verifies that no non-2xx status code ever gets
// cached, including edge cases like 3xx redirects and unusual 4xx codes.
func TestChaos_AllNon2xxCodes(t *testing.T) {
	codes := []int{
		100, 101, // 1xx informational
		301, 302, 304, 307, 308, // 3xx redirects
		400, 401, 403, 404, 405, 408, 410, 418, 429, 451, // 4xx client errors
		500, 502, 503, 504, // 5xx server errors
	}
	for _, code := range codes {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := testCache(t)

			var n atomic.Int32
			fetch := func() (*Response, error) {
				n.Add(1)
				return &Response{StatusCode: code, Body: []byte(`{}`)}, nil
			}

			c.Fetch(fmt.Sprintf("non2xx-%d", code), "TestEndpoint", fetch)
			time.Sleep(10 * time.Millisecond)
			r, _ := c.Fetch(fmt.Sprintf("non2xx-%d", code), "TestEndpoint", fetch)

			if r.Source == "cache" {
				t.Errorf("status %d was served from cache", code)
			}
			if n.Load() != 2 {
				t.Errorf("fetch count = %d, want 2 (status %d should not be cached)", n.Load(), code)
			}
		})
	}
}

// TestChaos_2xxVariantsCached verifies that 2xx codes other than 200 are
// cached correctly (200, 201, 202, 204, 206).
func TestChaos_2xxVariantsCached(t *testing.T) {
	for _, code := range []int{200, 201, 202, 204, 206} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := testCache(t)

			var n atomic.Int32
			fetch := func() (*Response, error) {
				n.Add(1)
				return &Response{StatusCode: code, Body: []byte(`{}`)}, nil
			}

			c.Fetch(fmt.Sprintf("ok-%d", code), "TestEndpoint", fetch)
			time.Sleep(10 * time.Millisecond)
			r, _ := c.Fetch(fmt.Sprintf("ok-%d", code), "TestEndpoint", fetch)

			if r.Source != "cache" {
				t.Errorf("status %d not served from cache (source=%s)", code, r.Source)
			}
			if r.StatusCode != code {
				t.Errorf("cached status = %d, want %d", r.StatusCode, code)
			}
			if n.Load() != 1 {
				t.Errorf("fetch count = %d, want 1 (status %d should be cached)", n.Load(), code)
			}
		})
	}
}

// TestChaos_ConcurrentMixed404And200 fires concurrent requests for different
// keys, some returning 200 and some 404. Verifies no cross-contamination:
// 200 keys must cache, 404 keys must not.
func TestChaos_ConcurrentMixed404And200(t *testing.T) {
	c := testCache(t)

	const keys = 20
	var wg sync.WaitGroup

	for i := range keys {
		wg.Go(func() {
			key := fmt.Sprintf("mixed-%d", i)
			code := 200
			if i%2 == 0 {
				code = 404
			}
			fetch := func() (*Response, error) {
				time.Sleep(time.Duration(rand.IntN(10)) * time.Millisecond)
				return &Response{StatusCode: code, Body: []byte(fmt.Sprintf(`{"i":%d}`, i))}, nil
			}

			// First request
			r1, err := c.Fetch(key, "TestEndpoint", fetch)
			if err != nil {
				t.Errorf("key %s: %v", key, err)
				return
			}
			if r1.StatusCode != code {
				t.Errorf("key %s: status = %d, want %d", key, r1.StatusCode, code)
			}

			time.Sleep(15 * time.Millisecond)

			// Second request
			r2, err := c.Fetch(key, "TestEndpoint", fetch)
			if err != nil {
				t.Errorf("key %s: %v", key, err)
				return
			}
			if code == 404 && r2.Source == "cache" {
				t.Errorf("key %s: 404 served from cache", key)
			}
			if code == 200 && r2.Source != "cache" {
				t.Errorf("key %s: 200 not served from cache (source=%s)", key, r2.Source)
			}
		})
	}
	wg.Wait()
}

// TestChaos_ErrorMetricsUnderLoad verifies ErrorsNotCached counter is
// accurate under concurrent non-2xx responses.
func TestChaos_ErrorMetricsUnderLoad(t *testing.T) {
	c := testCache(t)

	const goroutines = 30
	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Go(func() {
			key := fmt.Sprintf("metrics-chaos-%d", i)
			code := []int{404, 401, 500, 503}[i%4]
			fetch := func() (*Response, error) {
				return &Response{StatusCode: code, Body: []byte(`{}`)}, nil
			}
			c.Fetch(key, "TestEndpoint", fetch)
		})
	}
	wg.Wait()

	errCount := c.metrics.ErrorsNotCached.Load()
	// Each goroutine uses a unique key, so no singleflight coalescing.
	// Every one should increment ErrorsNotCached exactly once.
	if errCount != goroutines {
		t.Errorf("ErrorsNotCached = %d, want %d", errCount, goroutines)
	}
	// Nothing should have been admitted to cache.
	if accepted := c.metrics.AdmissionAccepted.Load(); accepted != 0 {
		t.Errorf("AdmissionAccepted = %d, want 0", accepted)
	}
}

// TestChaos_404CoalescingDoesNotPoison verifies that singleflight coalescing
// of 404 responses doesn't accidentally cache them. Multiple goroutines
// request the same 404 key simultaneously; the coalesced result must still
// not be cached on subsequent requests.
func TestChaos_404CoalescingDoesNotPoison(t *testing.T) {
	c := testCache(t)

	var upstreamCalls atomic.Int32
	fetch := func() (*Response, error) {
		upstreamCalls.Add(1)
		time.Sleep(30 * time.Millisecond) // long enough for coalescing
		return &Response{StatusCode: 404, Body: []byte(`{}`)}, nil
	}

	// Phase 1: burst of concurrent requests, all coalesced.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			c.Fetch("coalesce-404", "TestEndpoint", fetch)
		})
	}
	close(start)
	wg.Wait()

	if calls := upstreamCalls.Load(); calls != 1 {
		t.Errorf("phase 1: upstream calls = %d, want 1 (singleflight)", calls)
	}

	// Phase 2: after coalescing completes, another request must still go upstream.
	time.Sleep(10 * time.Millisecond)
	r, _ := c.Fetch("coalesce-404", "TestEndpoint", fetch)
	if r.Source == "cache" {
		t.Error("phase 2: 404 served from cache after coalescing")
	}
	if upstreamCalls.Load() != 2 {
		t.Errorf("phase 2: upstream calls = %d, want 2", upstreamCalls.Load())
	}
}

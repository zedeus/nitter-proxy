package cache

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestGracefulDegradation_NoRedis(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.RedisAddr = ""
	})

	var n atomic.Int32
	fetch := countingFetch(&n, `test`)

	c.Fetch("test-key", "TestEndpoint", fetch)
	c.Fetch("test-key", "TestEndpoint", fetch)

	if n.Load() != 2 {
		t.Errorf("Without Redis, expected 2 upstream calls, got %d", n.Load())
	}
}

func TestCacheDisabled(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.Enabled = false
	})

	var n atomic.Int32
	fetch := countingFetch(&n, `data`)

	c.Fetch("key", "TestEndpoint", fetch)
	c.Fetch("key", "TestEndpoint", fetch)

	if n.Load() != 2 {
		t.Errorf("Expected 2 upstream calls with cache disabled, got %d", n.Load())
	}
}

func TestMaxObjectSize(t *testing.T) {
	c := testCache(t, func(cfg *Config) {
		cfg.MaxObjectSize = 100
	})

	largeBody := make([]byte, 200)
	var n atomic.Int32
	fetch := func() (*Response, error) {
		n.Add(1)
		return &Response{StatusCode: 200, Body: largeBody}, nil
	}

	c.Fetch("large-key", "TestEndpoint", fetch)
	time.Sleep(10 * time.Millisecond)
	c.Fetch("large-key", "TestEndpoint", fetch)

	if n.Load() != 2 {
		t.Errorf("Large objects should not be cached, got %d calls", n.Load())
	}
}

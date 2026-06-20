package cache

import (
	"testing"
	"time"
)

func TestGracefulDegradation_NoRedis(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = ""
	cfg.PopularityThreshold = 0
	cfg.Whitelist = []string{"TestEndpoint"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	callCount := 0
	fetch := func() (*Response, error) {
		callCount++
		return &Response{StatusCode: 200, Body: []byte(`test`)}, nil
	}

	c.Fetch("test-key", "TestEndpoint", fetch)
	c.Fetch("test-key", "TestEndpoint", fetch)

	if callCount != 2 {
		t.Errorf("Without Redis, expected 2 upstream calls, got %d", callCount)
	}
}

func TestCacheDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	callCount := 0
	fetch := func() (*Response, error) {
		callCount++
		return &Response{StatusCode: 200, Body: []byte(`data`)}, nil
	}

	c.Fetch("key", "TestEndpoint", fetch)
	c.Fetch("key", "TestEndpoint", fetch)

	if callCount != 2 {
		t.Errorf("Expected 2 upstream calls with cache disabled, got %d", callCount)
	}
}

func TestMaxObjectSize(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = "test:maxsize:"
	cfg.PopularityThreshold = 0
	cfg.MaxObjectSize = 100
	cfg.Whitelist = []string{"TestEndpoint"}

	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.Close()

	largeBody := make([]byte, 200)
	callCount := 0
	fetch := func() (*Response, error) {
		callCount++
		return &Response{StatusCode: 200, Body: largeBody}, nil
	}

	c.Fetch("large-key", "TestEndpoint", fetch)
	time.Sleep(10 * time.Millisecond)
	c.Fetch("large-key", "TestEndpoint", fetch)

	if callCount != 2 {
		t.Errorf("Large objects should not be cached, got %d calls", callCount)
	}
}

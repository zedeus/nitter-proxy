package cache

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testCache creates a Cache with sensible test defaults (popularity=0,
// whitelist=TestEndpoint). Pass option funcs to tweak the config further.
func testCache(t *testing.T, opts ...func(*Config)) *Cache {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RedisAddr = getTestRedisAddr(t)
	cfg.RedisPrefix = fmt.Sprintf("test:%d:", time.Now().UnixNano())
	cfg.PopularityThreshold = 0
	cfg.Whitelist = []string{"TestEndpoint"}
	for _, fn := range opts {
		fn(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// countingFetch returns a fetch function that counts calls and returns
// a 200 response with the given body.
func countingFetch(n *atomic.Int32, body string) func() (*Response, error) {
	return func() (*Response, error) {
		n.Add(1)
		return &Response{StatusCode: 200, Body: []byte(body)}, nil
	}
}

// seedStale inserts a 200 entry that expired 1 minute ago into both primary
// and stale slots.
func seedStale(c *Cache, key string) {
	c.set(key, &entry{
		Status:   200,
		Body:     []byte(`{"good": true}`),
		CachedAt: time.Now().Add(-6 * time.Minute).UnixNano(),
		TTL:      5 * time.Minute,
		Endpoint: "TestEndpoint",
	})
}

func getTestRedisAddr(t *testing.T) string {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not available at %s: %v", addr, err)
	}
	rdb.Close()
	return addr
}

package cache

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Response is what the fetcher returns.
type Response struct {
	StatusCode int
	Body       []byte
	Headers    map[string]string
}

// Result is what Fetch returns to callers.
type Result struct {
	StatusCode int
	Body       []byte
	Source     string            // "cache", "upstream", "stale"
	Headers    map[string]string // Only set when Source == "upstream"
}

type entry struct {
	Status   int           `json:"s"`
	Body     []byte        `json:"b"`
	CachedAt int64         `json:"t"`
	TTL      time.Duration `json:"d"`
	Endpoint string        `json:"e"`
}

func (e *entry) isStale() bool {
	return time.Duration(time.Now().UnixNano()-e.CachedAt) > e.TTL
}

type Config struct {
	Enabled bool `toml:"enabled"`

	// Redis settings
	RedisAddr     string `toml:"redisAddr"`
	RedisPassword string `toml:"redisPassword"`
	RedisDB       int    `toml:"redisDB"`
	RedisPrefix   string `toml:"redisPrefix"`

	// TTL settings
	DefaultTTL   time.Duration            `toml:"defaultTTL"`
	EndpointTTLs map[string]time.Duration `toml:"endpointTTLs"`

	// Strategy flags
	EnableStaleIfError    bool          `toml:"enableStaleIfError"`
	StaleIfErrorWindow    time.Duration `toml:"staleIfErrorWindow"`
	EnableNegativeCaching bool          `toml:"enableNegativeCaching"`
	NegativeCacheTTL      time.Duration `toml:"negativeCacheTTL"`

	// Size limits
	MaxObjectSize int64 `toml:"maxObjectSize"`

	// Endpoint whitelist (only cache these)
	Whitelist []string `toml:"whitelist"`

	// Popularity-based admission control
	// Items must be accessed PopularityThreshold times within PopularityWindow
	// before they become eligible for caching. Set threshold to 0 to cache immediately.
	PopularityThreshold int            `toml:"popularityThreshold"`
	PopularityWindow    time.Duration  `toml:"popularityWindow"`
	EndpointThresholds  map[string]int `toml:"endpointThresholds"` // per-endpoint overrides
}

func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		RedisAddr:     "localhost:6379",
		RedisPassword: "",
		RedisDB:       0,
		RedisPrefix:   "np:",
		DefaultTTL:    5 * time.Minute,
		EndpointTTLs: map[string]time.Duration{
			"UserByScreenName":                       30 * time.Minute,
			"UserResultByScreenNameQuery":            30 * time.Minute,
			"UserResultByIdQuery":                    30 * time.Minute,
			"AboutAccountQuery":                      30 * time.Minute,
			"UserWithProfileTweetsQueryV2":           2 * time.Minute,
			"UserWithProfileTweetsAndRepliesQueryV2": 2 * time.Minute,
			"UserTweets":                             2 * time.Minute,
			"UserTweetsAndReplies":                   2 * time.Minute,
			"MediaTimelineV2":                        2 * time.Minute,
			"UserMedia":                              2 * time.Minute,
			"ConversationTimeline":                   5 * time.Minute,
			"TweetDetail":                            10 * time.Minute,
			"TweetResultByRestId":                    10 * time.Minute,
			"TweetResultByIdQuery":                   10 * time.Minute,
			"TweetResultsByRestIds":                  10 * time.Minute,
			"TweetEditHistory":                       10 * time.Minute,
			"SearchTimeline":                         1 * time.Minute,
			"ListByRestId":                           10 * time.Minute,
			"ListBySlug":                             10 * time.Minute,
			"ListMembers":                            5 * time.Minute,
			"ListTimeline":                           2 * time.Minute,
			"CommunityQuery":                         10 * time.Minute,
			"CommunityTweetsTimeline":                2 * time.Minute,
			"CommunityMediaTimeline":                 2 * time.Minute,
			"membersSliceTimeline_Query":             5 * time.Minute,
			"moderatorsSliceTimeline_Query":          5 * time.Minute,
			"CommunityHashtagsTimeline":              2 * time.Minute,
			"BroadcastQuery":                         1 * time.Minute,
			"AudioSpaceById":                         1 * time.Minute,
		},
		EnableStaleIfError:    true,
		StaleIfErrorWindow:    5 * time.Minute,
		EnableNegativeCaching: true,
		NegativeCacheTTL:      2 * time.Minute,
		MaxObjectSize:         1 << 20,
		Whitelist:             []string{},
		PopularityThreshold:   1,
		PopularityWindow:      2 * time.Minute,
		EndpointThresholds:    map[string]int{"SearchTimeline": 3},
	}
}

func (c *Cache) ThresholdFor(endpoint string) int {
	if t, ok := c.cfg.EndpointThresholds[endpoint]; ok {
		return t
	}
	return c.cfg.PopularityThreshold
}

type Cache struct {
	cfg          Config
	redis        *redis.Client
	sfg          singleflight.Group
	metrics      Metrics
	whitelistSet map[string]struct{}
	popularity   *popularityTracker
}

func New(cfg Config) (*Cache, error) {
	var rdb *redis.Client
	if cfg.RedisAddr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			slog.Warn("[CACHE] Redis unavailable, L2 disabled", "error", err)
			rdb = nil
		}
	}

	whiteset := make(map[string]struct{}, len(cfg.Whitelist))
	for _, ep := range cfg.Whitelist {
		whiteset[ep] = struct{}{}
	}

	var pop *popularityTracker
	if cfg.Enabled {
		pop = newPopularityTracker(cfg.PopularityWindow, cfg.PopularityThreshold)
	}

	c := &Cache{
		cfg:          cfg,
		redis:        rdb,
		whitelistSet: whiteset,
		popularity:   pop,
	}

	return c, nil
}

func (c *Cache) Close() {
	if c.popularity != nil {
		c.popularity.Close()
	}
	if c.redis != nil {
		c.redis.Close()
	}
}

func (c *Cache) IsCacheable(endpoint string) bool {
	if !c.cfg.Enabled {
		return false
	}
	if len(c.whitelistSet) == 0 {
		return true
	}
	_, ok := c.whitelistSet[endpoint]
	return ok
}

func (c *Cache) TTLFor(endpoint string) time.Duration {
	if ttl, ok := c.cfg.EndpointTTLs[endpoint]; ok {
		return ttl
	}
	return c.cfg.DefaultTTL
}

func (c *Cache) staleKey(key string) string {
	return c.cfg.RedisPrefix + "stale:" + key
}

func (c *Cache) get(key string) (*entry, bool) {
	if c.redis == nil {
		return nil, false
	}
	data, err := c.redis.Get(context.Background(), c.cfg.RedisPrefix+key).Bytes()
	if err != nil {
		return nil, false
	}
	var e entry
	if json.Unmarshal(data, &e) != nil {
		return nil, false
	}
	return &e, true
}

func (c *Cache) getStale(key string) (*entry, bool) {
	if e, ok := c.get(key); ok {
		return e, true
	}
	if c.redis == nil {
		return nil, false
	}
	data, err := c.redis.Get(context.Background(), c.staleKey(key)).Bytes()
	if err != nil {
		return nil, false
	}
	var e entry
	if json.Unmarshal(data, &e) != nil {
		return nil, false
	}
	return &e, true
}

func (c *Cache) set(key string, e *entry) {
	if c.redis == nil || int64(len(e.Body)) > c.cfg.MaxObjectSize {
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx := context.Background()
	c.redis.Set(ctx, c.cfg.RedisPrefix+key, data, e.TTL)
	if c.cfg.EnableStaleIfError {
		c.redis.Set(ctx, c.staleKey(key), data, e.TTL+c.cfg.StaleIfErrorWindow)
	}
}

func (c *Cache) Fetch(key, endpoint string, fetch func() (*Response, error)) (*Result, error) {
	if !c.IsCacheable(endpoint) {
		c.metrics.UpstreamRequests.Add(1)
		r, err := fetch()
		if err != nil {
			return nil, err
		}
		return &Result{r.StatusCode, r.Body, "upstream", r.Headers}, nil
	}

	if e, ok := c.get(key); ok && !e.isStale() {
		c.metrics.Hits.Add(1)
		c.metrics.UpstreamAvoided.Add(1)
		c.metrics.RecordEndpointHit(e.Endpoint)
		c.metrics.BytesServed.Add(uint64(len(e.Body)))
		return &Result{e.Status, e.Body, "cache", nil}, nil
	}
	c.metrics.Misses.Add(1)
	c.popularity.Record(key)
	c.metrics.RecordEndpointMiss(endpoint)

	v, err, shared := c.sfg.Do(key, func() (any, error) {
		if e, ok := c.get(key); ok && !e.isStale() {
			c.metrics.Hits.Add(1)
			c.metrics.RecordEndpointHit(e.Endpoint)
			c.metrics.BytesServed.Add(uint64(len(e.Body)))
			return &Result{e.Status, e.Body, "cache", nil}, nil
		}

		c.metrics.UpstreamRequests.Add(1)
		r, err := fetch()
		if err != nil {
			if c.cfg.EnableStaleIfError {
				if e, ok := c.getStale(key); ok {
					age := time.Duration(time.Now().UnixNano()-e.CachedAt) - e.TTL
					if age < c.cfg.StaleIfErrorWindow {
						c.metrics.StaleServed.Add(1)
						return &Result{e.Status, e.Body, "stale", nil}, nil
					}
				}
			}
			return nil, err
		}

		ttl := c.TTLFor(endpoint)
		if c.cfg.EnableNegativeCaching && r.StatusCode == http.StatusNotFound {
			ttl = c.cfg.NegativeCacheTTL
			c.metrics.NegativeCached.Add(1)
		}
		e := &entry{
			Status:   r.StatusCode,
			Body:     r.Body,
			CachedAt: time.Now().UnixNano(),
			TTL:      ttl,
			Endpoint: endpoint,
		}

		threshold := c.ThresholdFor(endpoint)
		if threshold == 0 || c.popularity.Count(key) >= threshold {
			c.set(key, e)
			c.metrics.AdmissionAccepted.Add(1)
		} else {
			c.metrics.AdmissionRejected.Add(1)
		}

		return &Result{r.StatusCode, r.Body, "upstream", r.Headers}, nil
	})
	if err != nil {
		return nil, err
	}
	if shared {
		c.metrics.CoalescedCount.Add(1)
	}
	return v.(*Result), nil
}

func (c *Cache) Metrics() *Metrics {
	return &c.metrics
}

func BuildCacheKey(u *url.URL) string {
	if u.RawQuery == "" {
		return hashKey(u.Path)
	}
	// GraphQL: only `variables` affects the response, features is ignored
	vars := extractParam(u.RawQuery, "variables")
	if vars == "" {
		return hashKey(u.Path)
	}
	return hashKey(u.Path + vars)
}

func extractParam(query, key string) string {
	for query != "" {
		var part string
		part, query, _ = strings.Cut(query, "&")
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			val, _ := url.QueryUnescape(v)
			return val
		}
	}
	return ""
}

func hashKey(s string) string {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], h)
	return hex.EncodeToString(buf[:])
}

func ExtractEndpoint(u *url.URL) string {
	path := u.Path
	// GraphQL: /graphql/{hash}/{endpoint}
	if i := strings.Index(path, "/graphql/"); i >= 0 {
		rest := path[i+9:]
		if _, after, ok := strings.Cut(rest, "/"); ok {
			endpoint, _, _ := strings.Cut(after, "/")
			return endpoint
		}
	}
	// REST: last path segment
	path = strings.TrimSuffix(path, "/")
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return ""
}

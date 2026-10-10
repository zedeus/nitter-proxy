package dashboard

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zedeus/nitter-proxy/cache"
)

//go:embed uplot.min.css
var uplotCSS string

//go:embed style.css
var styleCSS string

//go:embed page.html
var pageHTML string

//go:embed uplot.min.js
var uplotJS string

//go:embed app.js
var appJS string

// indexHTML is the assembled dashboard page, built once at init.
var indexHTML = func() []byte {
	return []byte(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>nitter-proxy</title><style>` + uplotCSS + styleCSS + `</style></head><body>` +
		pageHTML + `<script>` + uplotJS + `</script><script>` + appJS + `</script></body></html>`)
}()

const (
	recordInterval = 1 * time.Second
	maxSnapshots   = 3600 // 1 hour at 1s intervals
	writeWait      = 5 * time.Second
	reqBufSize     = 100
)

// Snapshot is a point-in-time capture of all metrics.
type Snapshot struct {
	Timestamp      int64             `json:"ts"`
	Hits           uint64            `json:"hits"`
	Misses         uint64            `json:"misses"`
	Upstream       uint64            `json:"upstream"`
	Avoided        uint64            `json:"avoided"`
	BytesServed    uint64            `json:"bytes"`
	Coalesced      uint64            `json:"coalesced"`
	Stale          uint64            `json:"stale"`
	Errors         uint64            `json:"errors"`
	AdmAccepted    uint64            `json:"admAccepted"`
	AdmRejected    uint64            `json:"admRejected"`
	FetchErrors    uint64            `json:"fetchErrors"`
	LatencyUsTotal uint64            `json:"latencyUsTotal"`
	LatencyCount   uint64            `json:"latencyCount"`
	HitRate        float64           `json:"hitRate"`
	EndpointHits   map[string]uint64 `json:"epHits"`
	EndpointMisses map[string]uint64 `json:"epMisses"`
	ResponseCodes  map[int]uint64    `json:"responseCodes"`
	EndpointErrors map[string]uint64 `json:"epErrors"`

	// Per-endpoint latency (cumulative us + count)
	EpLatencyUs map[string]uint64 `json:"epLatUs"`
	EpLatencyN  map[string]uint64 `json:"epLatN"`
	// Per-endpoint bytes
	EpBytes map[string]uint64 `json:"epBytes"`
	// Cache-hit response time (cumulative us + count)
	CacheLatencyUs uint64 `json:"cacheLatUs"`
	CacheLatencyN  uint64 `json:"cacheLatN"`
	// Latency percentiles (from ring buffer, in microseconds)
	LatP50 int64 `json:"latP50"`
	LatP95 int64 `json:"latP95"`
	LatP99 int64 `json:"latP99"`
}

// RequestRecord is a single proxied request, streamed to the dashboard live.
type RequestRecord struct {
	Timestamp  int64  `json:"ts"`
	Endpoint   string `json:"endpoint"`
	StatusCode int    `json:"status"`
	Source     string `json:"source"`
	LatencyUs  int64  `json:"latencyUs"`
}

type cacheInfo struct {
	Enabled             bool              `json:"enabled"`
	DefaultTTL          string            `json:"defaultTTL"`
	EndpointTTLs        map[string]string `json:"endpointTTLs"`
	PopularityThreshold int               `json:"popularityThreshold"`
	PopularityWindow    string            `json:"popularityWindow"`
	EndpointThresholds  map[string]int    `json:"endpointThresholds"`
	StaleIfError        bool              `json:"staleIfError"`
}

type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsClient) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteJSON(v)
}

func (c *wsClient) writeRaw(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// Dashboard records time-series metric snapshots and streams them to
// connected WebSocket clients.
type Dashboard struct {
	metrics   *cache.Metrics
	cacheInfo cacheInfo
	startedAt time.Time

	mu      sync.RWMutex
	clients map[*wsClient]struct{}

	// Snapshot history ring buffer (avoids O(n) copy-shift every second).
	histBuf  []Snapshot
	histHead int // next write position
	histLen  int // number of valid entries (0..maxSnapshots)

	// Live request ring buffer.
	reqBuf  []RequestRecord
	reqHead int
	reqLen  int

	upgrader websocket.Upgrader
	done     chan struct{}
}

func New(metrics *cache.Metrics, cfg cache.Config) *Dashboard {
	epTTLs := make(map[string]string, len(cfg.EndpointTTLs))
	for k, v := range cfg.EndpointTTLs {
		epTTLs[k] = v.String()
	}

	return &Dashboard{
		metrics: metrics,
		cacheInfo: cacheInfo{
			Enabled:             cfg.Enabled,
			DefaultTTL:          cfg.DefaultTTL.String(),
			EndpointTTLs:        epTTLs,
			PopularityThreshold: cfg.PopularityThreshold,
			PopularityWindow:    cfg.PopularityWindow.String(),
			EndpointThresholds:  cfg.EndpointThresholds,
			StaleIfError:        cfg.EnableStaleIfError,
		},
		startedAt: time.Now(),
		histBuf:   make([]Snapshot, maxSnapshots),
		clients:   make(map[*wsClient]struct{}),
		reqBuf:    make([]RequestRecord, reqBufSize),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		done: make(chan struct{}),
	}
}

// Run records snapshots at a fixed interval and broadcasts them.
// It blocks until Close is called.
func (d *Dashboard) Run() {
	d.record()
	ticker := time.NewTicker(recordInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.record()
		}
	}
}

func (d *Dashboard) Close() {
	close(d.done)
	d.mu.Lock()
	for c := range d.clients {
		_ = c.conn.Close()
	}
	d.mu.Unlock()
}

// PushRequest records a request and streams it to connected WebSocket clients.
func (d *Dashboard) PushRequest(rec RequestRecord) {
	d.mu.Lock()
	d.reqBuf[d.reqHead] = rec
	d.reqHead = (d.reqHead + 1) % len(d.reqBuf)
	if d.reqLen < len(d.reqBuf) {
		d.reqLen++
	}

	if len(d.clients) == 0 {
		d.mu.Unlock()
		return
	}
	clients := make([]*wsClient, 0, len(d.clients))
	for c := range d.clients {
		clients = append(clients, c)
	}
	d.mu.Unlock()

	msg := struct {
		Type   string        `json:"type"`
		Record RequestRecord `json:"record"`
	}{"request", rec}

	d.broadcast(clients, msg)
}

// broadcast JSON-encodes msg once and sends the raw bytes to each client.
func (d *Dashboard) broadcast(clients []*wsClient, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("[DASHBOARD] marshal", "error", err)
		return
	}
	for _, c := range clients {
		if err := c.writeRaw(data); err != nil {
			d.mu.Lock()
			delete(d.clients, c)
			d.mu.Unlock()
			_ = c.conn.Close()
		}
	}
}

// recentRequests returns the ring buffer contents in chronological order.
// Caller must hold d.mu.
func (d *Dashboard) recentRequests() []RequestRecord {
	out := make([]RequestRecord, d.reqLen)
	start := (d.reqHead - d.reqLen + len(d.reqBuf)) % len(d.reqBuf)
	for i := range d.reqLen {
		out[i] = d.reqBuf[(start+i)%len(d.reqBuf)]
	}
	return out
}

func (d *Dashboard) snapshot() Snapshot {
	m := d.metrics
	p50, p95, p99 := m.LatencyPercentiles()

	epLatUs := make(map[string]uint64)
	epLatN := make(map[string]uint64)
	epBytes := make(map[string]uint64)
	m.EndpointStats.Range(func(key, value any) bool {
		ep, ok := key.(string)
		if !ok {
			return true
		}
		s, ok := value.(*cache.EndpointStat)
		if !ok {
			return true
		}
		epLatUs[ep] = s.LatencyUs.Load()
		epLatN[ep] = s.LatencyN.Load()
		epBytes[ep] = s.Bytes.Load()
		return true
	})

	return Snapshot{
		Timestamp:      time.Now().UnixMilli(),
		Hits:           m.Hits.Load(),
		Misses:         m.Misses.Load(),
		Upstream:       m.UpstreamRequests.Load(),
		Avoided:        m.UpstreamAvoided.Load(),
		BytesServed:    m.BytesServed.Load(),
		Coalesced:      m.CoalescedCount.Load(),
		Stale:          m.StaleServed.Load(),
		Errors:         m.ErrorsNotCached.Load(),
		AdmAccepted:    m.AdmissionAccepted.Load(),
		AdmRejected:    m.AdmissionRejected.Load(),
		FetchErrors:    m.FetchErrors.Load(),
		LatencyUsTotal: m.LatencyUsTotal.Load(),
		LatencyCount:   m.LatencyCount.Load(),
		HitRate:        m.HitRate(),
		EndpointHits:   cache.SnapshotStringMap(&m.EndpointHits),
		EndpointMisses: cache.SnapshotStringMap(&m.EndpointMisses),
		ResponseCodes:  cache.SnapshotIntMap(&m.ResponseCodes),
		EndpointErrors: cache.SnapshotStringMap(&m.EndpointErrors),
		EpLatencyUs:    epLatUs,
		EpLatencyN:     epLatN,
		EpBytes:        epBytes,
		CacheLatencyUs: m.CacheLatencyUs.Load(),
		CacheLatencyN:  m.CacheLatencyN.Load(),
		LatP50:         p50,
		LatP95:         p95,
		LatP99:         p99,
	}
}

func (d *Dashboard) record() {
	snap := d.snapshot()

	d.mu.Lock()
	d.histBuf[d.histHead] = snap
	d.histHead = (d.histHead + 1) % maxSnapshots
	if d.histLen < maxSnapshots {
		d.histLen++
	}

	clients := make([]*wsClient, 0, len(d.clients))
	for c := range d.clients {
		clients = append(clients, c)
	}
	d.mu.Unlock()

	if len(clients) == 0 {
		return
	}

	msg := struct {
		Type     string   `json:"type"`
		Snapshot Snapshot `json:"snapshot"`
	}{"tick", snap}

	d.broadcast(clients, msg)
}

// ServeIndex serves the embedded dashboard HTML page.
func (*Dashboard) ServeIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

// ServeWebSocket upgrades the connection and streams metric snapshots.
func (d *Dashboard) ServeWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := d.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("[DASHBOARD] WebSocket upgrade", "error", err)
		return
	}

	client := &wsClient{conn: conn}

	// Send history + config + recent requests before registering for ticks.
	d.mu.RLock()
	// Read ring buffer in chronological order
	history := make([]Snapshot, d.histLen)
	start := (d.histHead - d.histLen + maxSnapshots) % maxSnapshots
	for i := range d.histLen {
		history[i] = d.histBuf[(start+i)%maxSnapshots]
	}
	recent := d.recentRequests()
	d.mu.RUnlock()

	init := struct {
		Type           string          `json:"type"`
		History        []Snapshot      `json:"history"`
		StartedAt      int64           `json:"startedAt"`
		Cache          cacheInfo       `json:"cache"`
		RecentRequests []RequestRecord `json:"recentRequests"`
	}{"init", history, d.startedAt.UnixMilli(), d.cacheInfo, recent}

	if err := client.writeJSON(init); err != nil {
		_ = conn.Close()
		return
	}

	d.mu.Lock()
	d.clients[client] = struct{}{}
	d.mu.Unlock()

	slog.Info("[DASHBOARD] Client connected", "addr", r.RemoteAddr)

	defer func() {
		d.mu.Lock()
		delete(d.clients, client)
		d.mu.Unlock()
		_ = conn.Close()
		slog.Info("[DASHBOARD] Client disconnected", "addr", r.RemoteAddr)
	}()

	// Read loop: keeps the connection alive and detects client disconnect.
	conn.SetReadLimit(512)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

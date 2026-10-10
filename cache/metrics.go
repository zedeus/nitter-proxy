package cache

import (
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// EndpointStat combines latency and bytes counters for a single endpoint,
// stored in one sync.Map entry to halve lookup cost on the hot path.
type EndpointStat struct {
	LatencyUs atomic.Uint64 // cumulative upstream microseconds
	LatencyN  atomic.Uint64 // upstream request count
	Bytes     atomic.Uint64 // cumulative bytes served
}

type Metrics struct {
	Hits              atomic.Uint64
	Misses            atomic.Uint64
	UpstreamRequests  atomic.Uint64
	UpstreamAvoided   atomic.Uint64
	BytesServed       atomic.Uint64
	CoalescedCount    atomic.Uint64
	StaleServed       atomic.Uint64
	NegativeCached    atomic.Uint64
	NegativeRejected  atomic.Uint64 // 404s not cached (valid 200 exists or below threshold)
	ErrorsNotCached   atomic.Uint64
	AdmissionRejected atomic.Uint64
	AdmissionAccepted atomic.Uint64
	FetchErrors       atomic.Uint64
	LatencyUsTotal    atomic.Uint64 // cumulative microseconds
	LatencyCount      atomic.Uint64
	EndpointHits      sync.Map
	EndpointMisses    sync.Map
	ResponseCodes     sync.Map      // int -> *atomic.Uint64
	EndpointErrors    sync.Map      // "endpoint\tcode" -> *atomic.Uint64 (non-2xx only)
	EndpointStats     sync.Map      // endpoint -> *EndpointStat (latency + bytes in one lookup)
	CacheLatencyUs    atomic.Uint64 // cumulative cache-hit response time (us)
	CacheLatencyN     atomic.Uint64 // cache-hit response count

	// Latency reservoir for percentile estimation (p50/p95/p99).
	// Fixed-size ring buffer of recent latency samples in microseconds.
	latencyMu      sync.Mutex
	latencyRing    [1024]int64
	latencyRingPos int
	latencyRingLen int
}

func (m *Metrics) RecordLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	us := d.Microseconds()
	m.LatencyUsTotal.Add(uint64(us))
	m.LatencyCount.Add(1)
	m.latencyMu.Lock()
	m.latencyRing[m.latencyRingPos] = us
	m.latencyRingPos = (m.latencyRingPos + 1) % len(m.latencyRing)
	if m.latencyRingLen < len(m.latencyRing) {
		m.latencyRingLen++
	}
	m.latencyMu.Unlock()
}

// addToMap atomically adds delta to the *atomic.Uint64 stored under key,
// creating the entry if it does not exist.
func addToMap(m *sync.Map, key any, delta uint64) {
	v, ok := m.Load(key)
	if !ok {
		v, _ = m.LoadOrStore(key, &atomic.Uint64{})
	}
	v.(*atomic.Uint64).Add(delta)
}

// snapshotMap returns a point-in-time copy of a sync.Map whose keys
// are K and values are *atomic.Uint64.
func snapshotMap[K comparable](m *sync.Map) map[K]uint64 {
	out := make(map[K]uint64)
	m.Range(func(key, value any) bool {
		if k, ok := key.(K); ok {
			if cnt, ok := value.(*atomic.Uint64); ok {
				out[k] = cnt.Load()
			}
		}
		return true
	})
	return out
}

// SnapshotStringMap returns a point-in-time copy of a sync.Map[string]*atomic.Uint64.
func SnapshotStringMap(m *sync.Map) map[string]uint64 { return snapshotMap[string](m) }

// SnapshotIntMap returns a point-in-time copy of a sync.Map[int]*atomic.Uint64.
func SnapshotIntMap(m *sync.Map) map[int]uint64 { return snapshotMap[int](m) }

func (m *Metrics) getEndpointStat(endpoint string) *EndpointStat {
	v, ok := m.EndpointStats.Load(endpoint)
	if !ok {
		v, _ = m.EndpointStats.LoadOrStore(endpoint, &EndpointStat{})
	}
	return v.(*EndpointStat)
}

// RecordEndpointLatency tracks per-endpoint latency (upstream only).
func (m *Metrics) RecordEndpointLatency(endpoint string, d time.Duration) {
	if d <= 0 {
		return
	}
	s := m.getEndpointStat(endpoint)
	s.LatencyUs.Add(uint64(d.Microseconds()))
	s.LatencyN.Add(1)
}

// RecordCacheLatency tracks cache-hit response time.
func (m *Metrics) RecordCacheLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	m.CacheLatencyUs.Add(uint64(d.Microseconds()))
	m.CacheLatencyN.Add(1)
}

// RecordEndpointBytes tracks bytes served per endpoint.
func (m *Metrics) RecordEndpointBytes(endpoint string, n uint64) {
	m.getEndpointStat(endpoint).Bytes.Add(n)
}

// LatencyPercentiles returns p50, p95, p99 from the ring buffer in microseconds.
func (m *Metrics) LatencyPercentiles() (p50, p95, p99 int64) {
	m.latencyMu.Lock()
	n := m.latencyRingLen
	if n == 0 {
		m.latencyMu.Unlock()
		return 0, 0, 0
	}
	sorted := make([]int64, n)
	copy(sorted, m.latencyRing[:n])
	m.latencyMu.Unlock()
	slices.Sort(sorted)
	p50 = sorted[(n-1)*50/100]
	p95 = sorted[(n-1)*95/100]
	p99 = sorted[(n-1)*99/100]
	return
}

// RecordResponse tracks a response status code globally and, for non-2xx
// codes, per endpoint. Call from the HTTP handler after writing the response.
func (m *Metrics) RecordResponse(endpoint string, code int) {
	addToMap(&m.ResponseCodes, code, 1)
	if code < 200 || code >= 300 {
		addToMap(&m.EndpointErrors, endpoint+"\t"+strconv.Itoa(code), 1)
	}
}

func (m *Metrics) RecordEndpointHit(endpoint string) {
	addToMap(&m.EndpointHits, endpoint, 1)
}

func (m *Metrics) RecordEndpointMiss(endpoint string) {
	addToMap(&m.EndpointMisses, endpoint, 1)
}

func (m *Metrics) HitRate() float64 {
	hits := m.Hits.Load()
	total := hits + m.Misses.Load()
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func (m *Metrics) Snapshot() map[string]any {
	codes := make(map[string]uint64)
	for code, cnt := range SnapshotIntMap(&m.ResponseCodes) {
		codes[strconv.Itoa(code)] = cnt
	}

	return map[string]any{
		"hits":               m.Hits.Load(),
		"misses":             m.Misses.Load(),
		"upstream_requests":  m.UpstreamRequests.Load(),
		"upstream_avoided":   m.UpstreamAvoided.Load(),
		"bytes_served":       m.BytesServed.Load(),
		"coalesced_count":    m.CoalescedCount.Load(),
		"stale_served":       m.StaleServed.Load(),
		"negative_cached":    m.NegativeCached.Load(),
		"negative_rejected":  m.NegativeRejected.Load(),
		"errors_not_cached":  m.ErrorsNotCached.Load(),
		"admission_rejected": m.AdmissionRejected.Load(),
		"admission_accepted": m.AdmissionAccepted.Load(),
		"fetch_errors":       m.FetchErrors.Load(),
		"latency_us_total":   m.LatencyUsTotal.Load(),
		"latency_count":      m.LatencyCount.Load(),
		"hit_rate":           m.HitRate(),
		"response_codes":     codes,
	}
}

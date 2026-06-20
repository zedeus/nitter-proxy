package cache

import (
	"sync"
	"sync/atomic"
)

type Metrics struct {
	Hits              atomic.Uint64
	Misses            atomic.Uint64
	UpstreamRequests  atomic.Uint64
	UpstreamAvoided   atomic.Uint64
	BytesServed       atomic.Uint64
	CoalescedCount    atomic.Uint64
	StaleServed       atomic.Uint64
	NegativeCached    atomic.Uint64
	AdmissionRejected atomic.Uint64
	AdmissionAccepted atomic.Uint64
	EndpointHits      sync.Map
	EndpointMisses    sync.Map
}

func (m *Metrics) RecordEndpointHit(endpoint string) {
	v, ok := m.EndpointHits.Load(endpoint)
	if !ok {
		v, _ = m.EndpointHits.LoadOrStore(endpoint, &atomic.Uint64{})
	}
	v.(*atomic.Uint64).Add(1)
}

func (m *Metrics) RecordEndpointMiss(endpoint string) {
	v, ok := m.EndpointMisses.Load(endpoint)
	if !ok {
		v, _ = m.EndpointMisses.LoadOrStore(endpoint, &atomic.Uint64{})
	}
	v.(*atomic.Uint64).Add(1)
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
	return map[string]any{
		"hits":               m.Hits.Load(),
		"misses":             m.Misses.Load(),
		"upstream_requests":  m.UpstreamRequests.Load(),
		"upstream_avoided":   m.UpstreamAvoided.Load(),
		"bytes_served":       m.BytesServed.Load(),
		"coalesced_count":    m.CoalescedCount.Load(),
		"stale_served":       m.StaleServed.Load(),
		"negative_cached":    m.NegativeCached.Load(),
		"admission_rejected": m.AdmissionRejected.Load(),
		"admission_accepted": m.AdmissionAccepted.Load(),
		"hit_rate":           m.HitRate(),
	}
}

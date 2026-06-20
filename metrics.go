package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

func (s *Server) metricsHandler(w http.ResponseWriter, req *http.Request) {
	accept := req.Header.Get("Accept")

	if strings.Contains(accept, "application/json") || req.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		snapshot := s.cache.Metrics().Snapshot()
		json.NewEncoder(w).Encode(snapshot)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	m := s.cache.Metrics()
	var b strings.Builder

	writeMetric(&b, "nitter_proxy_cache_hits_total", "Cache hits", m.Hits.Load())
	writeMetric(&b, "nitter_proxy_cache_misses_total", "Cache misses", m.Misses.Load())
	writeMetric(&b, "nitter_proxy_upstream_requests_total", "Total upstream requests", m.UpstreamRequests.Load())
	writeMetric(&b, "nitter_proxy_upstream_avoided_total", "Upstream requests avoided by cache", m.UpstreamAvoided.Load())
	writeMetric(&b, "nitter_proxy_cache_bytes_served_total", "Total bytes served from cache", m.BytesServed.Load())
	writeMetric(&b, "nitter_proxy_request_coalesced_total", "Requests coalesced via singleflight", m.CoalescedCount.Load())
	writeMetric(&b, "nitter_proxy_stale_served_total", "Stale responses served", m.StaleServed.Load())
	writeMetric(&b, "nitter_proxy_negative_cached_total", "Negative (404) responses cached", m.NegativeCached.Load())
	writeMetric(&b, "nitter_proxy_admission_accepted_total", "Items cached after crossing popularity threshold", m.AdmissionAccepted.Load())
	writeMetric(&b, "nitter_proxy_admission_rejected_total", "Items not cached due to low popularity", m.AdmissionRejected.Load())

	hitRate := m.HitRate()
	fmt.Fprintf(&b, "# HELP nitter_proxy_cache_hit_rate Cache hit rate (0-1)\n")
	fmt.Fprintf(&b, "# TYPE nitter_proxy_cache_hit_rate gauge\n")
	fmt.Fprintf(&b, "nitter_proxy_cache_hit_rate %.4f\n\n", hitRate)

	fmt.Fprintf(&b, "# HELP nitter_proxy_cache_endpoint_hits_total Cache hits per endpoint\n")
	fmt.Fprintf(&b, "# TYPE nitter_proxy_cache_endpoint_hits_total counter\n")
	m.EndpointHits.Range(func(key, value any) bool {
		endpoint := key.(string)
		count := value.(*atomic.Uint64).Load()
		fmt.Fprintf(&b, "nitter_proxy_cache_endpoint_hits_total{endpoint=%q} %d\n", endpoint, count)
		return true
	})
	b.WriteString("\n")

	fmt.Fprintf(&b, "# HELP nitter_proxy_cache_endpoint_misses_total Cache misses per endpoint\n")
	fmt.Fprintf(&b, "# TYPE nitter_proxy_cache_endpoint_misses_total counter\n")
	m.EndpointMisses.Range(func(key, value any) bool {
		endpoint := key.(string)
		count := value.(*atomic.Uint64).Load()
		fmt.Fprintf(&b, "nitter_proxy_cache_endpoint_misses_total{endpoint=%q} %d\n", endpoint, count)
		return true
	})

	w.Write([]byte(b.String()))
}

func writeMetric(b *strings.Builder, name, help string, value uint64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s %d\n\n", name, value)
}

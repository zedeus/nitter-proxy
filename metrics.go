package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/zedeus/nitter-proxy/cache"
)

func (s *Server) metricsHandler(w http.ResponseWriter, req *http.Request) {
	accept := req.Header.Get("Accept")

	if strings.Contains(accept, "application/json") || req.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		snapshot := s.cache.Metrics().Snapshot()
		_ = json.NewEncoder(w).Encode(snapshot)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	m := s.cache.Metrics()
	var b strings.Builder

	writeMetric(&b, "nitter_proxy_cache_hits_total", "Cache hits", m.Hits.Load())
	writeMetric(&b, "nitter_proxy_cache_misses_total", "Cache misses", m.Misses.Load())
	writeMetric(&b, "nitter_proxy_upstream_requests_total", "Total upstream requests", m.UpstreamRequests.Load())
	writeMetric(&b, "nitter_proxy_upstream_avoided_total", "Upstream requests avoided by cache", m.UpstreamAvoided.Load())
	writeMetric(&b, "nitter_proxy_bytes_served_total", "Total response bytes served to clients", m.BytesServed.Load())
	writeMetric(&b, "nitter_proxy_request_coalesced_total", "Requests coalesced via singleflight", m.CoalescedCount.Load())
	writeMetric(&b, "nitter_proxy_stale_served_total", "Stale responses served", m.StaleServed.Load())
	writeMetric(&b, "nitter_proxy_negative_cached_total", "Negative (404) responses cached", m.NegativeCached.Load())
	writeMetric(&b, "nitter_proxy_negative_rejected_total", "Negative (404) responses not cached (valid 200 exists or below threshold)", m.NegativeRejected.Load())
	writeMetric(&b, "nitter_proxy_errors_not_cached_total", "Upstream errors (401/403/429/5xx) not cached", m.ErrorsNotCached.Load())
	writeMetric(&b, "nitter_proxy_admission_accepted_total", "Items cached after crossing popularity threshold", m.AdmissionAccepted.Load())
	writeMetric(&b, "nitter_proxy_admission_rejected_total", "Items not cached due to low popularity", m.AdmissionRejected.Load())
	writeMetric(&b, "nitter_proxy_fetch_errors_total", "Upstream fetch errors (network/TLS/timeout)", m.FetchErrors.Load())

	hitRate := m.HitRate()
	fmt.Fprint(&b, "# HELP nitter_proxy_cache_hit_rate Cache hit rate (0-1)\n")
	fmt.Fprint(&b, "# TYPE nitter_proxy_cache_hit_rate gauge\n")
	fmt.Fprintf(&b, "nitter_proxy_cache_hit_rate %.4f\n\n", hitRate)

	fmt.Fprint(&b, "# HELP nitter_proxy_cache_endpoint_hits_total Cache hits per endpoint\n")
	fmt.Fprint(&b, "# TYPE nitter_proxy_cache_endpoint_hits_total counter\n")
	for ep, cnt := range cache.SnapshotStringMap(&m.EndpointHits) {
		fmt.Fprintf(&b, "nitter_proxy_cache_endpoint_hits_total{endpoint=%q} %d\n", ep, cnt)
	}
	b.WriteString("\n")

	fmt.Fprint(&b, "# HELP nitter_proxy_cache_endpoint_misses_total Cache misses per endpoint\n")
	fmt.Fprint(&b, "# TYPE nitter_proxy_cache_endpoint_misses_total counter\n")
	for ep, cnt := range cache.SnapshotStringMap(&m.EndpointMisses) {
		fmt.Fprintf(&b, "nitter_proxy_cache_endpoint_misses_total{endpoint=%q} %d\n", ep, cnt)
	}
	b.WriteString("\n")

	fmt.Fprint(&b, "# HELP nitter_proxy_response_code_total Responses by HTTP status code\n")
	fmt.Fprint(&b, "# TYPE nitter_proxy_response_code_total counter\n")
	for code, cnt := range cache.SnapshotIntMap(&m.ResponseCodes) {
		fmt.Fprintf(&b, "nitter_proxy_response_code_total{code=\"%d\"} %d\n", code, cnt)
	}
	b.WriteString("\n")

	fmt.Fprint(&b, "# HELP nitter_proxy_endpoint_errors_total Non-2xx responses per endpoint and status code\n")
	fmt.Fprint(&b, "# TYPE nitter_proxy_endpoint_errors_total counter\n")
	for k, cnt := range cache.SnapshotStringMap(&m.EndpointErrors) {
		parts := strings.SplitN(k, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(&b, "nitter_proxy_endpoint_errors_total{endpoint=%q,code=%q} %d\n", parts[0], parts[1], cnt)
	}

	_, _ = w.Write([]byte(b.String()))
}

func writeMetric(b *strings.Builder, name, help string, value uint64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	fmt.Fprintf(b, "%s %d\n\n", name, value)
}

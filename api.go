package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/sardanioss/httpcloak"
	"github.com/zedeus/nitter-proxy/cache"
)

const webOrigin = "https://x.com"

var skipHeaders = map[string]bool{
	"user-agent":                true,
	"accept":                    true,
	"accept-encoding":           true,
	"accept-language":           true,
	"priority":                  true,
	"origin":                    true,
	"referer":                   true,
	"upgrade-insecure-requests": true,
	"dnt":                       true,
	"connection":                true,
	"proxy-connection":          true,
	"keep-alive":                true,
	"transfer-encoding":         true,
	"te":                        true,
	"upgrade":                   true,
	"host":                      true,
	"content-length":            true,
}

func formatURL(u *url.URL) string {
	u.Scheme = "https"
	u.Path = strings.TrimLeft(u.Path, "/")
	return u.String()
}

// cloakHeaders forwards the caller's app-layer headers and sets the Sec-Fetch
// context for an XHR from the X web app. Sec-Fetch-Mode must be explicit or
// httpcloak treats the GET as a navigation, which WAFs flag on API endpoints.
// Referer is the bare origin because Chrome's referrer policy drops the path
// cross-origin.
func cloakHeaders(h http.Header, targetHost string) map[string][]string {
	headers := make(map[string][]string, len(h)+4)
	for k, v := range h {
		lower := strings.ToLower(k)
		if skipHeaders[lower] ||
			strings.HasPrefix(lower, "sec-ch-") ||
			strings.HasPrefix(lower, "sec-fetch-") {
			continue
		}
		headers[lower] = v
	}

	headers["sec-fetch-mode"] = []string{"cors"}
	headers["sec-fetch-dest"] = []string{"empty"}
	headers["referer"] = []string{webOrigin + "/"}

	webHost := strings.TrimPrefix(webOrigin, "https://")
	switch {
	case targetHost == webHost:
		headers["sec-fetch-site"] = []string{"same-origin"}
	case strings.HasSuffix(targetHost, "."+webHost):
		headers["sec-fetch-site"] = []string{"same-site"}
		headers["origin"] = []string{webOrigin}
	default:
		headers["sec-fetch-site"] = []string{"cross-site"}
		headers["origin"] = []string{webOrigin}
	}

	return headers
}

func (s *Server) apiProxyHandler(w http.ResponseWriter, req *http.Request) {
	path, err := url.PathUnescape(req.PathValue("url"))
	if err != nil {
		slog.Error("[API] Invalid URL", "error", err)
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	u := *req.URL
	u.Path = path

	endpoint := cache.ExtractEndpoint(&u)
	cacheKey := cache.BuildCacheKey(&u)

	targetURL := formatURL(&u)
	targetHost, targetPath := "", u.Path
	if parsed, err := url.Parse(targetURL); err == nil {
		targetHost, targetPath = parsed.Host, parsed.Path
	}
	reqHeaders := cloakHeaders(req.Header, targetHost)
	ctx := req.Context()

	fetch := func() (*cache.Response, error) {
		resp, err := s.session.Do(ctx, &httpcloak.Request{
			Method:  http.MethodGet,
			URL:     targetURL,
			Headers: reqHeaders,
		})
		if err != nil {
			return nil, err
		}
		defer resp.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		if !utf8.Valid(body) {
			slog.Warn("[API] non-UTF8 upstream body, not caching",
				"status", resp.StatusCode,
				"content-encoding", resp.GetHeader("content-encoding"),
				"len", len(body),
				"path", targetPath,
			)
			return nil, fmt.Errorf("non-UTF8 upstream body (ce=%q, %d bytes)", resp.GetHeader("content-encoding"), len(body))
		}

		headers := make(map[string]string)
		for _, h := range []string{"x-rate-limit-limit", "x-rate-limit-remaining", "x-rate-limit-reset"} {
			if v := resp.GetHeader(h); v != "" {
				headers[h] = v
			}
		}

		return &cache.Response{StatusCode: resp.StatusCode, Body: body, Headers: headers}, nil
	}

	result, err := s.cache.Fetch(cacheKey, endpoint, fetch)
	if err != nil {
		slog.Error("[API] Proxy error", "error", err, "endpoint", endpoint)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch result.Source {
	case "upstream":
		w.Header().Set("X-NP-Cache", "MISS")
		for k, v := range result.Headers {
			w.Header().Set(k, v)
		}
	case "cache":
		w.Header().Set("X-NP-Cache", "HIT")
	case "stale":
		w.Header().Set("X-NP-Cache", "STALE")
	}

	w.WriteHeader(result.StatusCode)
	if _, err := w.Write(result.Body); err != nil && !isClientDisconnect(err) {
		slog.Error("[API] Write error", "error", err)
	}
}

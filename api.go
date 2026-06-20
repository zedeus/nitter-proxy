package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Noooste/azuretls-client"
	"github.com/zedeus/nitter-proxy/cache"
)

func (s *Server) doWithPinRetry(r *azuretls.Request) (*azuretls.Response, error) {
	resp, err := s.session.Do(r)
	if err != nil && strings.Contains(err.Error(), "pin verification failed") {
		u, parseErr := url.Parse(r.Url)
		if parseErr == nil {
			if resp != nil && resp.RawBody != nil {
				resp.RawBody.Close()
			}
			slog.Warn("[API] Pin verification failed, clearing pins and retrying", "host", u.Host)
			s.session.ClearPins(u)
			// session.Do mutates request state, so we need a fresh one
			retry := &azuretls.Request{
				Method:     r.Method,
				Url:        r.Url,
				Header:     r.Header,
				IgnoreBody: r.IgnoreBody,
			}
			resp, err = s.session.Do(retry)
		}
	}
	return resp, err
}

func formatURL(u *url.URL) string {
	u.Scheme = "https"
	u.Path = strings.TrimLeft(u.Path, "/")
	return u.String()
}

func copyHeaders(h http.Header) map[string][]string {
	headers := make(map[string][]string, len(h))
	for k, v := range h {
		if k != "User-Agent" {
			headers[k] = v
		}
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
	reqHeaders := copyHeaders(req.Header)
	ctx := req.Context()

	fetch := func() (*cache.Response, error) {
		r := &azuretls.Request{
			Method:     http.MethodGet,
			Url:        targetURL,
			Header:     reqHeaders,
			IgnoreBody: true,
		}
		r.SetContext(ctx)
		resp, err := s.doWithPinRetry(r)
		if err != nil {
			return nil, err
		}
		defer resp.RawBody.Close()

		body, err := io.ReadAll(resp.RawBody)
		if err != nil {
			return nil, err
		}

		headers := make(map[string]string)
		for _, h := range []string{"x-rate-limit-limit", "x-rate-limit-remaining", "x-rate-limit-reset"} {
			if v := resp.Header.Get(h); v != "" {
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
	if len(result.Body) >= 2 && result.Body[0] == 0x1f && result.Body[1] == 0x8b {
		w.Header().Set("Content-Encoding", "gzip")
	}
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

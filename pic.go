package main

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Headers worth forwarding from twimg responses.
var picProxyHeaders = []string{
	"Content-Type",
	"Content-Length",
	"Cache-Control",
	"Last-Modified",
	"Expires",
}

func picBuildURL(path string, orig bool) (string, bool) {
	// Nitter passes image paths in three forms:
	//   1. Full URL:      https://pbs.twimg.com/profile_banners/...
	//   2. Domain + path: pbs.twimg.com/profile_images/...
	//   3. Bare path:     media/AbcDef.jpg:thumb
	var raw string
	switch {
	case strings.HasPrefix(path, "https://"):
		raw = path
	case strings.Contains(path, "twimg.com/"):
		raw = "https://" + path
	default:
		raw = "https://pbs.twimg.com/" + path
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false
	}

	host := parsed.Hostname()
	if host != "twimg.com" && !strings.HasSuffix(host, ".twimg.com") {
		return "", false
	}

	if orig {
		parsed.RawQuery = "name=orig"
	}

	return parsed.String(), true
}

func (s *Server) picProxyHandler(w http.ResponseWriter, req *http.Request) {
	path, err := url.PathUnescape(req.PathValue("url"))
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	picURL, ok := picBuildURL(path, strings.Contains(req.URL.Path, "orig/"))
	if !ok {
		http.Error(w, "Invalid Host", http.StatusBadRequest)
		return
	}

	etag := strconv.FormatUint(hash(picURL), 10)
	if req.Header.Get("If-None-Match") == etag {
		slog.Info("[PIC] ETag matched", "url", picURL)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	resp, err := s.httpClient.Get(picURL)
	if err != nil {
		slog.Error("[PIC] Proxy error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}
	defer checkClose(resp.Body, &err)

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Upstream Error", resp.StatusCode)
		return
	}

	for _, k := range picProxyHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}

	w.Header().Set("ETag", etag)

	w.WriteHeader(resp.StatusCode)
	_, err = copyBody(w, resp.Body)
	if err != nil && !isClientDisconnect(err) {
		slog.Error("[PIC] Write error", "error", err)
	}
}

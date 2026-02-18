package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Noooste/azuretls-client"
)

func formatURL(u *url.URL) string {
	u.Path = strings.TrimLeft(u.Path, "/")
	u.Scheme = "https"
	return u.String()
}

func copyHeaders(h http.Header) map[string][]string {
	headers := make(map[string][]string, len(h))
	for k, value := range h {
		if k != "User-Agent" {
			headers[k] = value
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

	url := req.URL
	url.Path = path

	resp, err := s.session.Do(&azuretls.Request{
		Method:     http.MethodGet,
		Url:        formatURL(url),
		Header:     copyHeaders(req.Header),
		IgnoreBody: true,
	})
	if err != nil {
		slog.Error("[API] Proxy error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}

	defer checkClose(resp.RawBody, &err)

	for k, v := range resp.Header {
		if k == "Content-Length" {
			continue
		}
		for _, val := range v {
			w.Header().Add(k, val)
		}
	}

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.RawBody)
	if err != nil && !isClientDisconnect(err) {
		slog.Error("[API] Write error", "error", err)
	}
}

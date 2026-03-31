package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Headers worth forwarding from twimg responses.
var videoProxyHeaders = []string{
	"Accept-Ranges",
	"Cache-Control",
	"Content-Type",
	"Content-Length",
	"Content-Range",
	"Expires",
	"Last-Modified",
}

func (s *Server) videoProxyHandler(w http.ResponseWriter, req *http.Request) {
	sig := req.PathValue("sig")
	rawURL := req.PathValue("url")

	decoded, err := url.PathUnescape(rawURL)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	// Also handle query-string encoded URLs (percent-encoded in query form).
	if u, err := url.QueryUnescape(decoded); err == nil {
		decoded = u
	}

	if !strings.Contains(decoded, "http") || !strings.Contains(decoded, ".") {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if createHMAC(s.hmacKey, decoded) != sig {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	lastDot := strings.LastIndex(decoded, ".")
	extension, _, _ := strings.Cut(decoded[lastDot+1:], "?")

	switch extension {
	case "mp4", "ts", "m4s":
		s.proxyVideoStream(w, req, decoded)
	case "m3u8":
		s.proxyM3U8(w, decoded)
	case "vmap":
		s.proxyVMAP(w, decoded)
	default:
		slog.Error("Unsupported format: " + extension + ", full: " + decoded)
		http.Error(w, "Unsupported format", http.StatusBadRequest)
	}
}

func (s *Server) proxyVideoStream(w http.ResponseWriter, req *http.Request, videoURL string) {
	etag := strconv.FormatUint(hash(videoURL), 10)
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	videoReq, err := http.NewRequest("GET", videoURL, nil)
	if err != nil {
		slog.Error("[VIDEO] Bad URL", "error", err)
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if accept := req.Header.Get("Accept"); len(accept) > 0 {
		videoReq.Header.Add("Accept", accept)
	}
	if encoding := req.Header.Get("Accept-Encoding"); len(encoding) > 0 {
		videoReq.Header.Add("Accept-Encoding", encoding)
	}
	if r := req.Header.Get("Range"); len(r) > 0 {
		videoReq.Header.Add("Range", r)
	}

	resp, err := s.httpClient.Do(videoReq)
	if err != nil {
		slog.Error("[VIDEO] Proxy error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}
	defer checkClose(resp.Body, &err)

	if resp.StatusCode > 299 {
		http.Error(w, "Upstream Error", resp.StatusCode)
		return
	}

	for _, k := range videoProxyHeaders {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}

	w.Header().Set("ETag", etag)

	w.WriteHeader(resp.StatusCode)
	_, err = copyBody(w, resp.Body)
	if err != nil && !isClientDisconnect(err) {
		slog.Error("[VIDEO] Write error", "error", err)
	}
}

func (s *Server) proxyM3U8(w http.ResponseWriter, m3u8URL string) {
	body, err := s.fetchBody(m3u8URL)
	if err != nil {
		slog.Error("[VIDEO] M3U8 fetch error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}

	rewritten := rewriteM3U8(body, s.hmacKey, m3u8URL)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if _, err := io.WriteString(w, rewritten); err != nil {
		slog.Error("[VIDEO] M3U8 write error", "error", err)
	}
}

var vmapM3U8Re = regexp.MustCompile(`url="([^"]+\.m3u8[^"]*)"`)

func (s *Server) proxyVMAP(w http.ResponseWriter, vmapURL string) {
	body, err := s.fetchBody(vmapURL)
	if err != nil {
		slog.Error("[VIDEO] VMAP fetch error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}

	matches := vmapM3U8Re.FindStringSubmatch(body)
	if len(matches) < 2 {
		slog.Error("[VIDEO] No M3U8 URL found in VMAP")
		http.Error(w, "No M3U8 in VMAP", http.StatusBadGateway)
		return
	}

	m3u8URL := matches[1]
	m3u8Body, err := s.fetchBody(m3u8URL)
	if err != nil {
		slog.Error("[VIDEO] VMAP->M3U8 fetch error", "error", err)
		http.Error(w, "Proxy Error", http.StatusBadGateway)
		return
	}

	rewritten := rewriteM3U8(m3u8Body, s.hmacKey, m3u8URL)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	if _, err := io.WriteString(w, rewritten); err != nil {
		slog.Error("[VIDEO] VMAP write error", "error", err)
	}
}

// resolveManifestBase extracts the origin and directory path from a manifest URL
// for resolving relative URLs within the manifest.
func resolveManifestBase(manifestURL string) (origin, dirPath string) {
	if manifestURL == "" {
		return "https://video.twimg.com", ""
	}
	u, err := url.Parse(manifestURL)
	if err != nil {
		return "https://video.twimg.com", ""
	}
	origin = u.Scheme + "://" + u.Host
	if idx := strings.LastIndex(u.Path, "/"); idx >= 0 {
		dirPath = origin + u.Path[:idx+1]
	}
	return
}

// rewriteM3U8 rewrites relative URLs in an M3U8 playlist to proxied video URLs.
// manifestURL is the URL the manifest was fetched from, used to resolve relative paths.
func rewriteM3U8(content, hmacKey, manifestURL string) string {
	origin, dirPath := resolveManifestBase(manifestURL)
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, "URI=") {
			lines[i] = rewriteExtXMapURI(line, hmacKey, origin, dirPath)
		} else if len(line) > 0 && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "http") {
			if strings.HasPrefix(line, "/") || (dirPath != "" && strings.Contains(line, ".")) {
				lines[i] = makeVideoProxyURL(resolveManifestPath(line, origin, dirPath), hmacKey)
			}
		}
	}
	return strings.Join(lines, "\n")
}

var extXMapURIRe = regexp.MustCompile(`URI="([^"]+)"`)

func rewriteExtXMapURI(line, hmacKey, origin, dirPath string) string {
	return extXMapURIRe.ReplaceAllStringFunc(line, func(match string) string {
		sub := extXMapURIRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		resolved := resolveManifestPath(sub[1], origin, dirPath)
		return `URI="` + makeVideoProxyURL(resolved, hmacKey) + `"`
	})
}

// resolveManifestPath resolves a URL from within a manifest against the
// manifest's origin and directory path.
func resolveManifestPath(rawURL, origin, dirPath string) string {
	if strings.HasPrefix(rawURL, "http") {
		return rawURL
	}
	if strings.HasPrefix(rawURL, "/") {
		return origin + rawURL
	}
	if dirPath != "" {
		return dirPath + rawURL
	}
	return origin + "/" + rawURL
}

func makeVideoProxyURL(fullURL, hmacKey string) string {
	return "/video/" + createHMAC(hmacKey, fullURL) +
		"/" + url.QueryEscape(fullURL)
}

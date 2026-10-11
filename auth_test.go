package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zedeus/nitter-proxy/cache"
)

func TestAuthMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware("s3cret", inner)

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantPath string
	}{
		{"valid prefix strips to subpath", "/s3cret/api/foo", 200, "/api/foo"},
		{"bare prefix strips to root", "/s3cret", 200, "/"},
		{"trailing slash", "/s3cret/", 200, "/"},
		{"no prefix", "/api/foo", 401, ""},
		{"wrong token", "/wrong/api/foo", 401, ""},
		{"token as substring", "/s3crets/api", 401, ""},
		{"root without token", "/", 401, ""},
		{"deeply nested path", "/s3cret/pic/orig/media/img.jpg", 200, "/pic/orig/media/img.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if tt.wantCode == 200 {
				if got := rec.Header().Get("X-Path"); got != tt.wantPath {
					t.Errorf("forwarded path = %q, want %q", got, tt.wantPath)
				}
			}
		})
	}
}

func TestAuthMiddlewareClearsRawPath(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			t.Errorf("RawPath = %q, want empty", r.URL.RawPath)
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := authMiddleware("tok", inner)

	req := httptest.NewRequest("GET", "/tok/api/some%2Fpath", nil)
	req.URL.RawPath = "/tok/api/some%2Fpath"
	req.URL.Path = "/tok/api/some/path"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// newTestServer returns a Server with no cache and no httpcloak session.
// Without a session, /api/ returns 503, which distinguishes "route matched"
// from 401 (auth) or 404 (no route).
func newTestServer(t *testing.T) *Server {
	t.Helper()
	c, err := cache.New(cache.Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Server{
		httpClient: http.DefaultClient,
		hmacKey:    "testkey",
		cache:      c,
	}
}

func TestNoAuthRoutesWork(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.buildHandler(&Config{})

	tests := []struct {
		path     string
		wantNot  int // must NOT get this status (401 = blocked, 404 = unmatched)
		wantBody string
	}{
		{"/api/api.x.com/2/timeline", http.StatusUnauthorized, ""},
		{"/pic/example.com/img.jpg", http.StatusUnauthorized, ""},
		{"/video/BAD/http://example.com/v.mp4", http.StatusUnauthorized, ""},
		{"/metrics", 0, "nitter_proxy_"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if tt.wantNot != 0 && rec.Code == tt.wantNot {
				t.Errorf("got status %d, should not be %d (route should be reachable)", rec.Code, tt.wantNot)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
		})
	}
}

func TestAuthRoutesBlocked(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.buildHandler(&Config{Auth: AuthConfig{Token: "mytoken"}})

	paths := []string{
		"/api/api.x.com/2/timeline",
		"/pic/pbs.twimg.com/media/img.jpg",
		"/video/SIG/http://example.com/v.mp4",
		"/metrics",
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			req := httptest.NewRequest("GET", p, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestAuthRoutesAllowed(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.buildHandler(&Config{Auth: AuthConfig{Token: "mytoken"}})

	tests := []struct {
		path     string
		wantNot  int
		wantBody string
	}{
		{"/mytoken/api/api.x.com/2/timeline", http.StatusUnauthorized, ""},
		{"/mytoken/pic/example.com/img.jpg", http.StatusUnauthorized, ""},
		{"/mytoken/video/BAD/http://example.com/v.mp4", http.StatusUnauthorized, ""},
		{"/mytoken/metrics", 0, "nitter_proxy_"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized {
				t.Fatal("got 401, want non-401")
			}
			if tt.wantNot != 0 && rec.Code == tt.wantNot {
				t.Errorf("got status %d, should not be %d", rec.Code, tt.wantNot)
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body missing %q", tt.wantBody)
			}
		})
	}
}

// TestAuthQueryStringPreserved checks that query params survive prefix stripping.
func TestAuthQueryStringPreserved(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.buildHandler(&Config{Auth: AuthConfig{Token: "tok"}})

	req := httptest.NewRequest("GET", "/tok/metrics?format=json", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (query param not forwarded?)", ct)
	}
}

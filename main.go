package main

import (
	"log"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sardanioss/httpcloak"
	"github.com/zedeus/nitter-proxy/cache"
	"github.com/zedeus/nitter-proxy/dashboard"
)

type Server struct {
	mu         sync.RWMutex
	session    *httpcloak.Session
	httpClient *http.Client
	hmacKey    string
	cache      *cache.Cache
	dash       *dashboard.Dashboard // nil when dashboard disabled
}

// copyBufPool holds reusable 32KB buffers for io.CopyBuffer.
var copyBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	if !slices.Contains(httpcloak.Presets(), cfg.Config.Fingerprint) {
		log.Fatalf("unknown fingerprint preset %q; see httpcloak.Presets()", cfg.Config.Fingerprint)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost:   20,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	c, err := cache.New(cfg.Cache)
	if err != nil {
		log.Fatal("initializing cache:", err)
	}
	defer c.Close()

	if cfg.Cache.Enabled {
		slog.Info("Cache enabled", "redis", cfg.Cache.RedisAddr)
	}

	srv := &Server{
		httpClient: httpClient,
		hmacKey:    cfg.Config.HMACKey,
		cache:      c,
	}

	if cfg.Dashboard.Enabled {
		srv.dash = dashboard.New(c.Metrics(), cfg.Cache)
		go srv.dash.Run()
		defer srv.dash.Close()
		slog.Info("Dashboard enabled", "path", "/dashboard")
	}

	// Init httpcloak session in background; server starts accepting requests immediately.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Fatalf("session init panic: %v", r)
			}
		}()
		session := httpcloak.NewSession(
			cfg.Config.Fingerprint,
			httpcloak.WithoutCookieJar(),
			httpcloak.WithoutConditionalCache(),
			httpcloak.WithDisableHTTP3(),
		)
		srv.mu.Lock()
		srv.session = session
		srv.mu.Unlock()
		slog.Info("Fingerprint", "preset", cfg.Config.Fingerprint)
	}()
	defer func() {
		srv.mu.RLock()
		s := srv.session
		srv.mu.RUnlock()
		if s != nil {
			s.Close()
		}
	}()

	handler := srv.buildHandler(cfg)
	if cfg.Auth.Token != "" {
		slog.Info("Auth enabled")
	}

	addr := cfg.Server.Address + ":" + strconv.Itoa(cfg.Server.Port)
	slog.Info("Serving", "addr", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

// buildHandler wires up the route mux and optionally wraps it with auth.
func (s *Server) buildHandler(cfg *Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/{url...}", s.apiProxyHandler)
	mux.HandleFunc("/pic/{url}", s.picProxyHandler)
	mux.HandleFunc("/pic/orig/{url}", s.picProxyHandler)
	mux.HandleFunc("/video/{sig}/{url...}", s.videoProxyHandler)
	mux.HandleFunc("/metrics", s.metricsHandler)

	if s.dash != nil {
		mux.HandleFunc("/dashboard", s.dash.ServeIndex)
		mux.HandleFunc("/dashboard/ws", s.dash.ServeWebSocket)
	}

	var handler http.Handler = mux
	if cfg.Auth.Token != "" {
		handler = authMiddleware(cfg.Auth.Token, mux)
	}
	return handler
}

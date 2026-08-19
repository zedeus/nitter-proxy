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
)

type Server struct {
	session    *httpcloak.Session
	httpClient *http.Client
	hmacKey    string
	cache      *cache.Cache
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

	session := httpcloak.NewSession(
		cfg.Config.Fingerprint,
		httpcloak.WithoutCookieJar(),
		httpcloak.WithoutConditionalCache(),
		httpcloak.WithDisableHTTP3(),
	)
	defer session.Close()
	slog.Info("Fingerprint", "preset", cfg.Config.Fingerprint)

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
		session:    session,
		httpClient: httpClient,
		hmacKey:    cfg.Config.HMACKey,
		cache:      c,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/{url...}", srv.apiProxyHandler)
	mux.HandleFunc("/pic/{url}", srv.picProxyHandler)
	mux.HandleFunc("/pic/orig/{url}", srv.picProxyHandler)
	mux.HandleFunc("/video/{sig}/{url...}", srv.videoProxyHandler)
	mux.HandleFunc("/metrics", srv.metricsHandler)

	addr := cfg.Server.Address + ":" + strconv.Itoa(cfg.Server.Port)
	slog.Info("Serving", "addr", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

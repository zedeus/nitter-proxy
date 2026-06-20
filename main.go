package main

import (
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Noooste/azuretls-client"
	"github.com/zedeus/nitter-proxy/cache"
)

type Server struct {
	session    *azuretls.Session
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

	session := azuretls.NewSession()
	session.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	session.EnableLog()
	defer session.Close()

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

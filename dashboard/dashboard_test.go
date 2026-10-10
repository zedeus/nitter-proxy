package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zedeus/nitter-proxy/cache"
)

func newTestDashboard() *Dashboard {
	m := &cache.Metrics{}
	m.Hits.Store(100)
	m.Misses.Store(30)
	m.UpstreamRequests.Store(25)
	m.UpstreamAvoided.Store(100)
	m.BytesServed.Store(50000)
	m.CoalescedCount.Store(5)
	m.AdmissionAccepted.Store(80)
	m.AdmissionRejected.Store(20)

	cfg := cache.DefaultConfig()
	return New(m, cfg)
}

func TestServeIndex(t *testing.T) {
	d := newTestDashboard()
	defer d.Close()

	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	d.ServeIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "nitter-proxy") {
		t.Error("body missing 'nitter-proxy' title")
	}
	if !strings.Contains(body, "WebSocket") {
		t.Error("body missing WebSocket code")
	}
}

func TestWebSocketInitMessage(t *testing.T) {
	d := newTestDashboard()
	go d.Run()
	defer d.Close()

	// Push a request before connecting so init includes it.
	d.PushRequest(RequestRecord{
		Timestamp: time.Now().UnixMilli(), Endpoint: "TweetDetail",
		StatusCode: 200, Source: "cache", LatencyUs: 1500,
	})

	time.Sleep(50 * time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(d.ServeWebSocket))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("read:", err)
	}

	var msg struct {
		Type           string          `json:"type"`
		History        []Snapshot      `json:"history"`
		StartedAt      int64           `json:"startedAt"`
		Cache          cacheInfo       `json:"cache"`
		RecentRequests []RequestRecord `json:"recentRequests"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal("unmarshal:", err)
	}

	if msg.Type != "init" {
		t.Errorf("type = %q, want init", msg.Type)
	}
	if len(msg.History) == 0 {
		t.Error("history is empty")
	}
	if msg.StartedAt == 0 {
		t.Error("startedAt is zero")
	}
	if !msg.Cache.Enabled {
		t.Error("cache.enabled is false")
	}
	if len(msg.RecentRequests) != 1 {
		t.Errorf("recentRequests length = %d, want 1", len(msg.RecentRequests))
	} else if msg.RecentRequests[0].Endpoint != "TweetDetail" {
		t.Errorf("recentRequests[0].endpoint = %q, want TweetDetail", msg.RecentRequests[0].Endpoint)
	}

	last := msg.History[len(msg.History)-1]
	if last.Hits != 100 {
		t.Errorf("hits = %d, want 100", last.Hits)
	}
	if last.HitRate < 0.76 || last.HitRate > 0.78 {
		t.Errorf("hitRate = %.4f, want ~0.769", last.HitRate)
	}
}

func TestSnapshotEndpoints(t *testing.T) {
	m := &cache.Metrics{}
	m.RecordEndpointHit("UserByScreenName")
	m.RecordEndpointHit("UserByScreenName")
	m.RecordEndpointMiss("SearchTimeline")

	d := New(m, cache.DefaultConfig())
	defer d.Close()

	snap := d.snapshot()

	if snap.EndpointHits["UserByScreenName"] != 2 {
		t.Errorf("endpoint hits = %d, want 2", snap.EndpointHits["UserByScreenName"])
	}
	if snap.EndpointMisses["SearchTimeline"] != 1 {
		t.Errorf("endpoint misses = %d, want 1", snap.EndpointMisses["SearchTimeline"])
	}
}

func TestSnapshotResponseCodes(t *testing.T) {
	m := &cache.Metrics{}
	m.RecordResponse("TweetDetail", 200)
	m.RecordResponse("TweetDetail", 200)
	m.RecordResponse("TweetDetail", 200)
	m.RecordResponse("SearchTimeline", 429)
	m.RecordResponse("SearchTimeline", 429)
	m.RecordResponse("UserByScreenName", 403)
	m.FetchErrors.Store(3)

	d := New(m, cache.DefaultConfig())
	defer d.Close()

	snap := d.snapshot()

	if snap.ResponseCodes[200] != 3 {
		t.Errorf("response 200 = %d, want 3", snap.ResponseCodes[200])
	}
	if snap.ResponseCodes[429] != 2 {
		t.Errorf("response 429 = %d, want 2", snap.ResponseCodes[429])
	}
	if snap.ResponseCodes[403] != 1 {
		t.Errorf("response 403 = %d, want 1", snap.ResponseCodes[403])
	}
	if snap.FetchErrors != 3 {
		t.Errorf("fetchErrors = %d, want 3", snap.FetchErrors)
	}
	if snap.EndpointErrors["SearchTimeline\t429"] != 2 {
		t.Errorf("endpoint error SearchTimeline 429 = %d, want 2", snap.EndpointErrors["SearchTimeline\t429"])
	}
	if snap.EndpointErrors["UserByScreenName\t403"] != 1 {
		t.Errorf("endpoint error UserByScreenName 403 = %d, want 1", snap.EndpointErrors["UserByScreenName\t403"])
	}
	if _, ok := snap.EndpointErrors["TweetDetail\t200"]; ok {
		t.Error("200 should not be in endpoint errors")
	}
}

func TestSnapshotLatency(t *testing.T) {
	m := &cache.Metrics{}
	m.RecordLatency(150 * time.Millisecond)
	m.RecordLatency(250 * time.Millisecond)

	d := New(m, cache.DefaultConfig())
	defer d.Close()

	snap := d.snapshot()

	wantUs := uint64(400_000)
	if snap.LatencyUsTotal != wantUs {
		t.Errorf("latencyUsTotal = %d, want %d", snap.LatencyUsTotal, wantUs)
	}
	if snap.LatencyCount != 2 {
		t.Errorf("latencyCount = %d, want 2", snap.LatencyCount)
	}
}

func TestPushRequestStreamsToClient(t *testing.T) {
	d := newTestDashboard()
	go d.Run()
	defer d.Close()

	time.Sleep(50 * time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(d.ServeWebSocket))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer conn.Close()

	// Drain the init message.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal("read init:", err)
	}

	// Push a request.
	d.PushRequest(RequestRecord{
		Timestamp: time.Now().UnixMilli(), Endpoint: "SearchTimeline",
		StatusCode: 429, Source: "upstream", LatencyUs: 5000,
	})

	// Read the streamed request message.
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatal("read request:", err)
	}

	var msg struct {
		Type   string        `json:"type"`
		Record RequestRecord `json:"record"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal("unmarshal:", err)
	}
	if msg.Type != "request" {
		t.Errorf("type = %q, want request", msg.Type)
	}
	if msg.Record.StatusCode != 429 {
		t.Errorf("status = %d, want 429", msg.Record.StatusCode)
	}
	if msg.Record.Endpoint != "SearchTimeline" {
		t.Errorf("endpoint = %q, want SearchTimeline", msg.Record.Endpoint)
	}
}

func TestRecentRequestsRingBuffer(t *testing.T) {
	d := newTestDashboard()
	defer d.Close()

	// Push 150 records into a 100-capacity ring buffer.
	for i := range 150 {
		d.PushRequest(RequestRecord{
			Timestamp: int64(i), Endpoint: "ep",
			StatusCode: 200, Source: "cache", LatencyUs: 100,
		})
	}

	d.mu.RLock()
	recent := d.recentRequests()
	d.mu.RUnlock()

	if len(recent) != reqBufSize {
		t.Fatalf("len = %d, want %d", len(recent), reqBufSize)
	}
	// Should contain records 50-149 in order.
	if recent[0].Timestamp != 50 {
		t.Errorf("first.ts = %d, want 50", recent[0].Timestamp)
	}
	if recent[99].Timestamp != 149 {
		t.Errorf("last.ts = %d, want 149", recent[99].Timestamp)
	}
}

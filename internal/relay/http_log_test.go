package relay

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"mcp-remoteaccess/internal/protocol"
)

func TestHTTPLoggingAndRedaction(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(Config{
		UserKey: "secret-user-key",
		DevKey:  "secret-dev-key",
		Logger:  log.New(&buf, "[relay] ", 0),
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 1. /health -> 200
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// 2. /update/manifest with sensitive query param key + normal params -> 401/200 with redaction
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/update/manifest?os=windows&arch=amd64&key=secret-user-key", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// 3. Unknown path -> 404
	resp, err = http.Get(ts.URL + "/no-such-route")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	logged := buf.String()
	if !strings.Contains(logged, "http: GET /health scheme=http status=200") {
		t.Fatalf("expected /health access log, got:\n%s", logged)
	}
	if !strings.Contains(logged, "GET /update/manifest?") || !strings.Contains(logged, "os=windows") || !strings.Contains(logged, "arch=amd64") {
		t.Fatalf("expected non-sensitive query params in log, got:\n%s", logged)
	}
	if strings.Contains(logged, "secret-user-key") {
		t.Fatalf("sensitive query key leaked into HTTP logs:\n%s", logged)
	}
	if !strings.Contains(logged, "key=REDACTED") {
		t.Fatalf("expected key=REDACTED in log, got:\n%s", logged)
	}
	if !strings.Contains(logged, "xff=203.0.113.9") {
		t.Fatalf("expected xff in log, got:\n%s", logged)
	}
	if !strings.Contains(logged, "http: GET /no-such-route scheme=http status=404") {
		t.Fatalf("expected 404 log for unknown path, got:\n%s", logged)
	}
}

func TestHTTPLoggingPreservesHijackerAndFlusher(t *testing.T) {
	var buf bytes.Buffer
	srv := NewServer(Config{
		UserKey:   "u-key",
		DevKey:    "d-key",
		EnableMCP: true,
		Logger:    log.New(&buf, "[relay] ", 0),
	})

	// Verify Flusher & ResponseController Unwrap on wrapped ResponseWriter
	rec := httptest.NewRecorder()
	var sawFlusher bool
	h := srv.withHTTPLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		sawFlusher = ok
		if ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
	if !sawFlusher {
		t.Fatal("loggingResponseWriter must implement http.Flusher")
	}
	if !rec.Flushed {
		t.Fatal("expected Flush() to forward to underlying ResponseWriter")
	}

	// Verify WebSocket upgrade (http.Hijacker) works cleanly over live httptest.Server
	// and does not emit duplicate HTTP access logs once hijacked.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Unauthenticated /ws/dev still logs 401
	unauthResp, err := http.Get(ts.URL + "/ws/dev")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", unauthResp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/dev"
	hdr := http.Header{}
	hdr.Set(protocol.HeaderDevKey, "d-key")
	wsConn, resp, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}
	_ = wsConn.Close()
	ts.Close()

	logged := buf.String()
	if !strings.Contains(logged, "http: GET /ws/dev scheme=http status=401") {
		t.Fatalf("expected 401 log for rejected /ws/dev, got:\n%s", logged)
	}
	if strings.Contains(logged, "status=101") {
		t.Fatalf("did not expect duplicate HTTP access log for hijacked WebSocket:\n%s", logged)
	}
}

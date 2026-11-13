package relay

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestFlexTLSListenerAcceptNotBlockedByIdleClient(t *testing.T) {
	tlsCfg, err := BuildTLSConfig("", "", true)
	if err != nil {
		t.Fatalf("BuildTLSConfig: %v", err)
	}

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := rawLn.Addr().String()

	flexLn := NewFlexTLSListener(rawLn, tlsCfg, true)
	srv := NewServer(Config{ReleasesDir: t.TempDir()})
	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() { _ = httpSrv.Serve(flexLn) }()
	defer httpSrv.Close()

	// 1. Open a raw TCP connection that sends zero bytes (simulating idle/slowloris client)
	idleConn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial idle conn: %v", err)
	}
	defer idleConn.Close()

	time.Sleep(50 * time.Millisecond)

	// 2. Normal plain HTTP request must not be blocked by idleConn
	plainClient := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := plainClient.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("plain HTTP request was blocked by idle client: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on HTTP, got %d", resp.StatusCode)
	}

	// 3. TLS request on the same FlexTLSListener must also succeed promptly
	tlsClient := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	tlsResp, err := tlsClient.Get("https://" + addr + "/health")
	if err != nil {
		t.Fatalf("HTTPS request failed on FlexTLSListener: %v", err)
	}
	_ = tlsResp.Body.Close()
	if tlsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on HTTPS, got %d", tlsResp.StatusCode)
	}
}

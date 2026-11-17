package agent

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/protocol"
)

func TestClassifyErrors(t *testing.T) {
	if !IsFatalError(&FatalError{Reason: "bad"}) {
		t.Fatal("expected FatalError to satisfy IsFatalError")
	}
	if IsFatalError(errors.New("transient network error")) {
		t.Fatal("expected plain error not to satisfy IsFatalError")
	}

	// ECONNREFUSED should be fatal
	refusedErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	if err := classifyDialError(refusedErr, nil); !IsFatalError(err) {
		t.Fatalf("expected ECONNREFUSED to be fatal, got %v", err)
	}

	// 401 / 403 / 404 should be fatal; 503 should be retryable
	if err := classifyDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: http.StatusUnauthorized}); !IsFatalError(err) {
		t.Fatalf("expected 401 to be fatal, got %v", err)
	}
	if err := classifyDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: http.StatusForbidden}); !IsFatalError(err) {
		t.Fatalf("expected 403 to be fatal, got %v", err)
	}
	if err := classifyDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: http.StatusNotFound}); !IsFatalError(err) {
		t.Fatalf("expected 404 to be fatal, got %v", err)
	}
	if err := classifyDialError(websocket.ErrBadHandshake, &http.Response{StatusCode: http.StatusServiceUnavailable}); IsFatalError(err) {
		t.Fatalf("expected 503 to be retryable, got fatal %v", err)
	}

	// Timeout should be retryable
	if err := classifyDialError(context.DeadlineExceeded, nil); IsFatalError(err) {
		t.Fatalf("expected DeadlineExceeded to be retryable, got fatal %v", err)
	}

	// WS close codes
	for _, code := range []int{
		protocol.CloseCodeKicked,
		protocol.CloseCodeRejected,
		protocol.CloseCodeUnauthorized,
		websocket.CloseNormalClosure,
		websocket.ClosePolicyViolation,
	} {
		if err := classifyWSCloseError(&websocket.CloseError{Code: code, Text: "reason"}); !IsFatalError(err) {
			t.Fatalf("expected close code %d to be fatal, got %v", code, err)
		}
	}
	for _, code := range []int{
		websocket.CloseAbnormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseInternalServerErr,
		websocket.CloseServiceRestart,
		websocket.CloseTryAgainLater,
	} {
		if err := classifyWSCloseError(&websocket.CloseError{Code: code, Text: "transient"}); IsFatalError(err) {
			t.Fatalf("expected close code %d to be retryable, got fatal %v", code, err)
		}
	}
}

func TestClientRunStopsOnConnectionRefused(t *testing.T) {
	// Bind and immediately close a listener to get an unused port that actively refuses connections.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var statuses []string
	client := NewClient(ClientOptions{
		ServerAddr: addr,
		UserKey:    "any-key",
		MachineID:  "refused-node",
		AutoUpdate: false,
		Insecure:   true,
		OnStatus: func(st string) {
			statuses = append(statuses, st)
		},
	})

	runErr := client.Run(ctx)
	if runErr == nil || !IsFatalError(runErr) {
		t.Fatalf("expected fatal error on connection refused, got %v (ctxErr=%v)", runErr, ctx.Err())
	}
	for _, st := range statuses {
		if strings.Contains(st, "retrying in") {
			t.Fatalf("expected no retry status on connection refused, got status %q", st)
		}
	}
}

func TestClientRunRetriesOn503AndAbruptDropButStopsOnNormalClose(t *testing.T) {
	var attempts int32
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/user" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// Attempt 1: transient 503 -> client should retry!
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		wsConn := protocol.NewWSConn(ws)
		sess, err := yamux.Client(wsConn, protocol.DefaultYamuxConfig())
		if err != nil {
			_ = wsConn.Close()
			return
		}
		regStream, err := sess.AcceptStream()
		if err != nil {
			_ = sess.Close()
			return
		}
		bs := protocol.NewBufferedStream(regStream)
		var hdr protocol.StreamHeader
		if err := bs.ReadLineJSON(&hdr); err != nil {
			_ = sess.Close()
			return
		}
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{OK: true})
		_ = regStream.Close()

		if n == 2 {
			// Attempt 2: hijack and abruptly close TCP socket without WebSocket Close frame (unexpected drop -> retry!)
			time.Sleep(50 * time.Millisecond)
			_ = ws.UnderlyingConn().Close()
			return
		}
		// Attempt 3: explicitly close with CloseCodeKicked -> fatal, no retry!
		time.Sleep(50 * time.Millisecond)
		_ = wsConn.CloseWithCode(protocol.CloseCodeKicked, "kicked by test")
		_ = sess.Close()
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	CustomRootCAs = pool
	defer func() { CustomRootCAs = nil }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := NewClient(ClientOptions{
		ServerAddr: srv.URL,
		UserKey:    "k",
		MachineID:  "retry-then-kick",
		AutoUpdate: false,
	})

	runErr := client.Run(ctx)
	if runErr == nil || !IsFatalError(runErr) {
		t.Fatalf("expected FatalError on final kick, got %v", runErr)
	}
	if !strings.Contains(runErr.Error(), "kicked by test") {
		t.Fatalf("expected kick reason in error, got %v", runErr)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("expected exactly 3 connection attempts (503 -> abrupt drop -> kick), got %d", got)
	}
}

func TestClientRunStopsOnUntrustedTLSCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	prevPool := CustomRootCAs
	CustomRootCAs = nil
	defer func() { CustomRootCAs = prevPool }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := NewClient(ClientOptions{
		ServerAddr: srv.URL,
		UserKey:    "k",
		MachineID:  "bad-tls",
		AutoUpdate: false,
		Insecure:   false,
	})
	runErr := client.Run(ctx)
	if runErr == nil || !IsFatalError(runErr) {
		t.Fatalf("expected fatal TLS error without retry, got %v (ctxErr=%v)", runErr, ctx.Err())
	}
	if !strings.Contains(fmt.Sprint(runErr), "TLS verification failed") {
		t.Fatalf("expected TLS verification failed message, got %v", runErr)
	}
}

func TestUDPReverseListenExitDeadlock(t *testing.T) {
	client := NewClient(ClientOptions{
		MachineID: "udp-deadlock-test",
		Workdir:   t.TempDir(),
	})
	defer client.Close()

	// Create an in-memory bidirectional stream pair via net.Pipe
	devSide, agentSide := net.Pipe()
	defer devSide.Close()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		bs := protocol.NewBufferedStream(agentSide)
		client.handleUDPReverseListenStream(bs, protocol.StreamHeader{
			StreamType: protocol.StreamTypeUDPReverseListen,
			RemoteAddr: "127.0.0.1:0",
		})
	}()

	devBS := protocol.NewBufferedStream(devSide)
	var ack protocol.StreamAck
	if err := devBS.ReadLineJSON(&ack); err != nil {
		t.Fatalf("read ack failed: %v", err)
	}
	if !ack.OK || ack.BoundAddr == "" {
		t.Fatalf("unexpected ack: %+v", ack)
	}

	// Send a UDP datagram to the bound reverse listener so an agentUDPSession is inserted into sessions map
	udpConn, err := net.Dial("udp", ack.BoundAddr)
	if err != nil {
		t.Fatalf("dial bound udp %s failed: %v", ack.BoundAddr, err)
	}
	defer udpConn.Close()
	if _, err := udpConn.Write([]byte("trigger-session")); err != nil {
		t.Fatalf("udp write failed: %v", err)
	}

	var ev protocol.ReverseConnEvent
	if err := devBS.ReadLineJSON(&ev); err != nil {
		t.Fatalf("read ReverseConnEvent failed: %v", err)
	}
	if ev.ConnID == "" {
		t.Fatalf("expected non-empty ConnID")
	}

	// Close the control stream while the UDP session is still in the sessions map.
	// Bug: defer in handleUDPReverseListenStream locks sessMu and calls s.Close() -> closeFn() -> sessMu.Lock() -> deadlock.
	_ = devSide.Close()

	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handleUDPReverseListenStream deadlocked on exit with active UDP session")
	}
}

func TestClassifyUpdateErrorReadOnlyAndPermission(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantFatal bool
	}{
		{
			name:      "os.ErrPermission wrapped",
			err:       fmt.Errorf("create update file failed: %w", &os.PathError{Op: "open", Path: "user-client.exe.new", Err: os.ErrPermission}),
			wantFatal: true,
		},
		{
			name:      "EACCES",
			err:       fmt.Errorf("rename running binary to .old failed: %w", syscall.EACCES),
			wantFatal: true,
		},
		{
			name:      "write protected message",
			err:       errors.New("create update file E:\\user-client.exe.new failed: The media is write protected."),
			wantFatal: true,
		},
		{
			name:      "read-only file system message",
			err:       errors.New("create update file /mnt/usb/user-client.new failed: read-only file system"),
			wantFatal: true,
		},
		{
			name:      "transient timeout",
			err:       context.DeadlineExceeded,
			wantFatal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyUpdateError(tc.err)
			if tc.wantFatal {
				if !IsFatalError(got) {
					t.Fatalf("expected FatalError for %v, got %v", tc.err, got)
				}
			} else if got != nil {
				t.Fatalf("expected nil (non-fatal) for %v, got %v", tc.err, got)
			}
		})
	}
}

func TestCheckAndDownloadUpdateStatusStages(t *testing.T) {
	newPayload := []byte("new-binary-content")
	sum := sha256.Sum256(newPayload)
	newSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update/manifest":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{
				Available: true,
				SHA256:    newSHA,
				Size:      int64(len(newPayload)),
			})
		case "/update/binary":
			_, _ = w.Write(newPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exePath := filepath.Join(t.TempDir(), "user-client.exe")
	if err := os.WriteFile(exePath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("write old exe: %v", err)
	}

	var statuses []string
	updated, err := checkAndDownloadUpdateWithStatus(srv.URL, "k", exePath, runtime.GOOS, runtime.GOARCH, true, func(st string) {
		statuses = append(statuses, st)
	})
	if err != nil {
		t.Fatalf("checkAndDownloadUpdateWithStatus failed: %v", err)
	}
	if !updated {
		t.Fatal("expected updated=true")
	}
	if len(statuses) != 2 || statuses[0] != "Checking for updates..." || statuses[1] != "Downloading update..." {
		t.Fatalf("unexpected statuses when update available: %v", statuses)
	}

	// Second call with matching SHA256 should only emit "Checking for updates..."
	statuses = nil
	updated, err = checkAndDownloadUpdateWithStatus(srv.URL, "k", exePath, runtime.GOOS, runtime.GOARCH, true, func(st string) {
		statuses = append(statuses, st)
	})
	if err != nil {
		t.Fatalf("second checkAndDownloadUpdateWithStatus failed: %v", err)
	}
	if updated {
		t.Fatal("expected updated=false when already on latest SHA256")
	}
	if len(statuses) != 1 || statuses[0] != "Checking for updates..." {
		t.Fatalf("unexpected statuses when up-to-date: %v", statuses)
	}
}

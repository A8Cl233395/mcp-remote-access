package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

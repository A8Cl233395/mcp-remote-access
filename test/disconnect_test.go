package test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/dev"
	"mcp-remoteaccess/internal/relay"
)

func TestFatalDisconnectsDoNotAutoReconnect(t *testing.T) {
	tempRoot := t.TempDir()
	const (
		userKey = "correct-user-key"
		devKey  = "correct-dev-key"
	)

	relaySrv := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: filepath.Join(tempRoot, "releases"),
	})
	httpSrv := httptest.NewTLSServer(relaySrv.Handler())
	defer httpSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(httpSrv.Certificate())
	agent.CustomRootCAs = pool
	defer func() { agent.CustomRootCAs = nil }()

	// 1. Wrong user key -> must exit immediately with FatalError without retrying
	t.Run("WrongUserKey", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		var retried bool
		c := agent.NewClient(agent.ClientOptions{
			ServerAddr: httpSrv.URL,
			UserKey:    "wrong-user-key",
			MachineID:  "bad-key-node",
			Workdir:    tempRoot,
			AutoUpdate: false,
			OnStatus: func(st string) {
				if strings.Contains(st, "retrying in") {
					retried = true
				}
			},
		})
		err := c.Run(ctx)
		if err == nil || !agent.IsFatalError(err) {
			t.Fatalf("expected fatal auth error, got %v (ctxErr=%v)", err, ctx.Err())
		}
		if !strings.Contains(err.Error(), "authentication failed") {
			t.Fatalf("expected authentication failed message, got %v", err)
		}
		if retried {
			t.Fatal("client should not auto-reconnect on wrong key")
		}
	})

	// 2. Duplicate MachineID -> first client is kicked offline and must NOT reconnect to kick the second client
	t.Run("KickedOfflineByDuplicateMachineID", func(t *testing.T) {
		ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel1()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()

		c1Ready := make(chan struct{}, 1)
		c1Done := make(chan error, 1)
		c1 := agent.NewClient(agent.ClientOptions{
			ServerAddr: httpSrv.URL,
			UserKey:    userKey,
			MachineID:  "shared-machine-id",
			Workdir:    tempRoot,
			AutoUpdate: false,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case c1Ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { c1Done <- c1.Run(ctx1) }()
		defer c1.Close()

		select {
		case <-c1Ready:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for client 1 to connect")
		}

		c2Ready := make(chan struct{}, 1)
		c2Done := make(chan error, 1)
		c2 := agent.NewClient(agent.ClientOptions{
			ServerAddr: httpSrv.URL,
			UserKey:    userKey,
			MachineID:  "shared-machine-id",
			Workdir:    tempRoot,
			AutoUpdate: false,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case c2Ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { c2Done <- c2.Run(ctx2) }()
		defer c2.Close()

		select {
		case <-c2Ready:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for client 2 to connect")
		}

		// Client 1 must exit immediately with FatalError ("kicked offline")
		select {
		case err := <-c1Done:
			if err == nil || !agent.IsFatalError(err) {
				t.Fatalf("expected client 1 to exit with FatalError when kicked, got %v", err)
			}
			if !strings.Contains(err.Error(), "kicked offline") {
				t.Fatalf("expected kicked offline error, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for kicked client 1 to exit")
		}

		// Wait past the old 1s backoff window and verify client 2 is still alive and serving RPCs!
		time.Sleep(1500 * time.Millisecond)
		select {
		case err := <-c2Done:
			t.Fatalf("client 2 should have remained connected, but exited with %v", err)
		default:
		}

		devSess := dev.NewSession(dev.SessionConfig{
			ServerAddr: httpSrv.URL,
			DevKey:     devKey,
		})
		defer devSess.Close()
		mcpSrv := dev.NewMCPServer(devSess)
		raw, _ := json.Marshal(map[string]any{"command": "echo still-alive"})
		res := mcpSrv.CallTool("shell", raw)
		if res.IsError || !strings.Contains(res.Content[0].Text, "still-alive") {
			t.Fatalf("expected client 2 to handle RPC after client 1 was kicked, got %+v", res)
		}
	})
}

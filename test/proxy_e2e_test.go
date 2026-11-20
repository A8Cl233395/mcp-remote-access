package test

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/dev"
	"mcp-remoteaccess/internal/relay"
)

// startTestHTTPConnectProxy starts a minimal HTTP CONNECT proxy that forwards
// CONNECT requests for expectedHost to targetAddr, counting successful tunnels.
func startTestHTTPConnectProxy(t *testing.T, expectedHost, targetAddr string) (proxyAddr string, count *int64) {
	t.Helper()
	var tunnels int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen http proxy: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(clientConn net.Conn) {
				defer clientConn.Close()
				br := bufio.NewReader(clientConn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Method != http.MethodConnect || req.Host != expectedHost {
					_, _ = io.WriteString(clientConn, "HTTP/1.1 403 Forbidden\r\n\r\n")
					return
				}
				upstream, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
				if err != nil {
					_, _ = io.WriteString(clientConn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer upstream.Close()
				atomic.AddInt64(&tunnels, 1)
				_, _ = io.WriteString(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n")

				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(upstream, br)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(clientConn, upstream)
					done <- struct{}{}
				}()
				<-done
			}(c)
		}
	}()

	return ln.Addr().String(), &tunnels
}

// startTestSOCKS5Proxy starts a minimal no-auth SOCKS5 proxy (RFC 1928) that forwards
// CONNECT requests for expectedHost (domain ATYP=0x03) to targetAddr.
func startTestSOCKS5Proxy(t *testing.T, expectedHost, targetAddr string) (proxyAddr string, count *int64) {
	t.Helper()
	var tunnels int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5 proxy: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(clientConn net.Conn) {
				defer clientConn.Close()
				// 1. Greeting: VER(1) NMETHODS(1) METHODS(NMETHODS)
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(clientConn, hdr); err != nil || hdr[0] != 0x05 {
					return
				}
				methods := make([]byte, int(hdr[1]))
				if _, err := io.ReadFull(clientConn, methods); err != nil {
					return
				}
				// Reply: VER=5, METHOD=0 (no auth)
				if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
					return
				}

				// 2. Request: VER(1) CMD(1) RSV(1) ATYP(1) ...
				reqHdr := make([]byte, 4)
				if _, err := io.ReadFull(clientConn, reqHdr); err != nil || reqHdr[0] != 0x05 || reqHdr[1] != 0x01 {
					return
				}
				var host string
				switch reqHdr[3] {
				case 0x03: // domain
					lenBuf := make([]byte, 1)
					if _, err := io.ReadFull(clientConn, lenBuf); err != nil {
						return
					}
					dom := make([]byte, int(lenBuf[0]))
					if _, err := io.ReadFull(clientConn, dom); err != nil {
						return
					}
					host = string(dom)
				default:
					_, _ = clientConn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				portBuf := make([]byte, 2)
				if _, err := io.ReadFull(clientConn, portBuf); err != nil {
					return
				}
				port := binary.BigEndian.Uint16(portBuf)
				if net.JoinHostPort(host, strconv.Itoa(int(port))) != expectedHost {
					_, _ = clientConn.Write([]byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}

				upstream, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
				if err != nil {
					_, _ = clientConn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer upstream.Close()
				atomic.AddInt64(&tunnels, 1)

				// Success reply
				if _, err := clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}); err != nil {
					return
				}

				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(upstream, clientConn)
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(clientConn, upstream)
					done <- struct{}{}
				}()
				<-done
			}(c)
		}
	}()

	return ln.Addr().String(), &tunnels
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
}

func TestProxyEndToEndHTTPConnectAndSOCKS5(t *testing.T) {
	tempRoot := t.TempDir()
	releasesDir := filepath.Join(tempRoot, "releases")
	if err := os.MkdirAll(releasesDir, 0755); err != nil {
		t.Fatalf("mkdir releases: %v", err)
	}

	const (
		userKey       = "proxy-user-key"
		devKey        = "proxy-dev-key"
		virtualTarget = "relay.proxytest.invalid:20055"
	)

	relaySrv := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: releasesDir,
	})
	httpSrv := httptest.NewTLSServer(relaySrv.Handler())
	defer httpSrv.Close()
	relayAddr := strings.TrimPrefix(httpSrv.URL, "https://")

	t.Run("HTTPConnectProxy", func(t *testing.T) {
		clearProxyEnv(t)
		proxyAddr, tunnels := startTestHTTPConnectProxy(t, virtualTarget, relayAddr)
		t.Setenv("HTTPS_PROXY", "http://"+proxyAddr)

		// 1. Auto-updater HTTP check goes through HTTP CONNECT proxy
		fakeExe := filepath.Join(tempRoot, "fake-agent.bin")
		if err := os.WriteFile(fakeExe, []byte("old-binary"), 0755); err != nil {
			t.Fatalf("write fake exe: %v", err)
		}
		updated, err := agent.CheckAndDownloadUpdateWithInsecure(virtualTarget, userKey, fakeExe, runtime.GOOS, runtime.GOARCH, true)
		if err != nil {
			t.Fatalf("CheckAndDownloadUpdate through HTTP proxy failed: %v", err)
		}
		if updated {
			t.Fatalf("expected no update available")
		}

		// 2. user-client WebSocket connects through HTTP CONNECT proxy
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 1)
		c := agent.NewClient(agent.ClientOptions{
			ServerAddr: virtualTarget,
			UserKey:    userKey,
			MachineID:  "proxy-http-node",
			Workdir:    tempRoot,
			AutoUpdate: false,
			Insecure:   true,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { _ = c.Run(ctx) }()
		defer c.Close()

		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for user-client to connect through HTTP CONNECT proxy")
		}

		// 3. dev-client WebSocket connects through HTTP CONNECT proxy and lists machines
		sess := dev.NewSession(dev.SessionConfig{
			ServerAddr: virtualTarget,
			DevKey:     devKey,
			Insecure:   true,
		})
		defer sess.Close()

		machines, err := sess.FetchMachines()
		if err != nil {
			t.Fatalf("dev FetchMachines through HTTP proxy failed: %v", err)
		}
		if len(machines) != 1 || machines[0].MachineID != "proxy-http-node" {
			t.Fatalf("unexpected machines via HTTP proxy: %+v", machines)
		}
		if got := atomic.LoadInt64(tunnels); got < 3 {
			t.Fatalf("expected at least 3 CONNECT tunnels (updater + user-client + dev-client), got %d", got)
		}
	})

	t.Run("SOCKS5Proxy", func(t *testing.T) {
		clearProxyEnv(t)
		proxyAddr, tunnels := startTestSOCKS5Proxy(t, virtualTarget, relayAddr)
		// Use socks5h:// to verify normalization to socks5:// for gorilla/websocket.
		t.Setenv("ALL_PROXY", "socks5h://"+proxyAddr)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 1)
		c := agent.NewClient(agent.ClientOptions{
			ServerAddr: virtualTarget,
			UserKey:    userKey,
			MachineID:  "proxy-socks5-node",
			Workdir:    tempRoot,
			AutoUpdate: false,
			Insecure:   true,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { _ = c.Run(ctx) }()
		defer c.Close()

		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for user-client to connect through SOCKS5 proxy")
		}

		sess := dev.NewSession(dev.SessionConfig{
			ServerAddr: virtualTarget,
			DevKey:     devKey,
			Insecure:   true,
		})
		defer sess.Close()

		machines, err := sess.FetchMachines()
		if err != nil {
			t.Fatalf("dev FetchMachines through SOCKS5 proxy failed: %v", err)
		}
		if len(machines) != 1 || machines[0].MachineID != "proxy-socks5-node" {
			t.Fatalf("unexpected machines via SOCKS5 proxy: %+v", machines)
		}
		if got := atomic.LoadInt64(tunnels); got < 2 {
			t.Fatalf("expected at least 2 SOCKS5 tunnels (user-client + dev-client), got %d", got)
		}
	})

	t.Run("NoProxyBypass", func(t *testing.T) {
		clearProxyEnv(t)
		proxyAddr, tunnels := startTestHTTPConnectProxy(t, virtualTarget, relayAddr)
		t.Setenv("HTTPS_PROXY", "http://"+proxyAddr)

		// 1. With NoProxy=true targeting the non-loopback virtual host, neither updater nor dev-client
		// should contact the configured proxy (tunnels must stay 0).
		fakeExe := filepath.Join(tempRoot, "fake-agent-noproxy.bin")
		if err := os.WriteFile(fakeExe, []byte("old-binary"), 0755); err != nil {
			t.Fatalf("write fake exe: %v", err)
		}
		if _, err := agent.CheckAndDownloadUpdateWithOptions(virtualTarget, userKey, fakeExe, runtime.GOOS, runtime.GOARCH, true, true); err == nil {
			t.Fatalf("expected updater with NoProxy=true to bypass proxy and fail resolving %s", virtualTarget)
		}

		badSess := dev.NewSession(dev.SessionConfig{
			ServerAddr: virtualTarget,
			DevKey:     devKey,
			Insecure:   true,
			NoProxy:    true,
		})
		defer badSess.Close()
		if _, err := badSess.FetchMachines(); err == nil {
			t.Fatalf("expected dev-client with NoProxy=true to bypass proxy and fail resolving %s", virtualTarget)
		}

		if got := atomic.LoadInt64(tunnels); got != 0 {
			t.Fatalf("expected 0 proxy tunnels when NoProxy=true, got %d", got)
		}

		// 2. Direct connection with NoProxy=true succeeds end-to-end without touching the proxy.
		if updated, err := agent.CheckAndDownloadUpdateWithOptions(relayAddr, userKey, fakeExe, runtime.GOOS, runtime.GOARCH, true, true); err != nil || updated {
			t.Fatalf("direct updater with NoProxy=true failed: updated=%v err=%v", updated, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 1)
		c := agent.NewClient(agent.ClientOptions{
			ServerAddr: relayAddr,
			UserKey:    userKey,
			MachineID:  "noproxy-node",
			Workdir:    tempRoot,
			AutoUpdate: false,
			Insecure:   true,
			NoProxy:    true,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { _ = c.Run(ctx) }()
		defer c.Close()

		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for direct user-client with NoProxy=true")
		}

		sess := dev.NewSession(dev.SessionConfig{
			ServerAddr: relayAddr,
			DevKey:     devKey,
			Insecure:   true,
			NoProxy:    true,
		})
		defer sess.Close()

		machines, err := sess.FetchMachines()
		if err != nil {
			t.Fatalf("direct dev FetchMachines with NoProxy=true failed: %v", err)
		}
		if len(machines) != 1 || machines[0].MachineID != "noproxy-node" {
			t.Fatalf("unexpected machines with NoProxy=true: %+v", machines)
		}
		if got := atomic.LoadInt64(tunnels); got != 0 {
			t.Fatalf("expected 0 proxy tunnels during direct NoProxy=true session, got %d", got)
		}
	})
}

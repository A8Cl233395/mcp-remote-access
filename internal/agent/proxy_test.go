package agent

import (
	"net/http"
	"net/url"
	"testing"
)

func TestParseProxyServerList(t *testing.T) {
	tests := []struct {
		name         string
		proxyStr     string
		targetScheme string
		want         string
	}{
		{
			name:         "single host:port for https",
			proxyStr:     "127.0.0.1:7890",
			targetScheme: "https",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "single host:port for wss",
			proxyStr:     "127.0.0.1:7890",
			targetScheme: "wss",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "windows per-protocol https",
			proxyStr:     "http=127.0.0.1:7890;https=127.0.0.1:7891;socks=127.0.0.1:7892",
			targetScheme: "https",
			want:         "http://127.0.0.1:7891",
		},
		{
			name:         "windows per-protocol http",
			proxyStr:     "http=127.0.0.1:7890;https=127.0.0.1:7891;socks=127.0.0.1:7892",
			targetScheme: "http",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "windows per-protocol fallback to http for https",
			proxyStr:     "http=127.0.0.1:7890;socks=127.0.0.1:7892",
			targetScheme: "https",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "windows socks only",
			proxyStr:     "socks=127.0.0.1:1080",
			targetScheme: "https",
			want:         "socks5://127.0.0.1:1080",
		},
		{
			name:         "pac proxy then direct",
			proxyStr:     "PROXY 127.0.0.1:7890; DIRECT",
			targetScheme: "https",
			want:         "http://127.0.0.1:7890",
		},
		{
			name:         "pac socks5",
			proxyStr:     "SOCKS5 127.0.0.1:1080; DIRECT",
			targetScheme: "https",
			want:         "socks5://127.0.0.1:1080",
		},
		{
			name:         "pac direct first",
			proxyStr:     "DIRECT; PROXY 127.0.0.1:7890",
			targetScheme: "https",
			want:         "",
		},
		{
			name:         "socks5h normalized to socks5",
			proxyStr:     "socks5h://user:pass@127.0.0.1:1080",
			targetScheme: "https",
			want:         "socks5://user:pass@127.0.0.1:1080",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProxyServerList(tc.proxyStr, tc.targetScheme)
			if err != nil {
				t.Fatalf("parseProxyServerList(%q, %q) error: %v", tc.proxyStr, tc.targetScheme, err)
			}
			gotStr := ""
			if got != nil {
				gotStr = got.String()
			}
			if gotStr != tc.want {
				t.Fatalf("got %q, want %q", gotStr, tc.want)
			}
		})
	}
}

func TestMatchBypassList(t *testing.T) {
	tests := []struct {
		name        string
		list        string
		target      string
		windowsMode bool
		want        bool
	}{
		{
			name:        "windows local unqualified host",
			list:        "localhost;127.*;<local>",
			target:      "https://intranet:20055",
			windowsMode: true,
			want:        true,
		},
		{
			name:        "windows local qualified domain not bypassed",
			list:        "localhost;127.*;<local>",
			target:      "https://relay.example.com:20055",
			windowsMode: true,
			want:        false,
		},
		{
			name:        "wildcard prefix",
			list:        "*.corp.local;192.168.*",
			target:      "https://api.corp.local:20055",
			windowsMode: true,
			want:        true,
		},
		{
			name:        "wildcard ip",
			list:        "*.corp.local;192.168.*",
			target:      "https://192.168.1.50:20055",
			windowsMode: true,
			want:        true,
		},
		{
			name:        "cidr match",
			list:        "10.0.0.0/8,172.16.0.0/12",
			target:      "https://10.20.30.40:20055",
			windowsMode: false,
			want:        true,
		},
		{
			name:        "no_proxy subdomain match",
			list:        "example.com",
			target:      "https://relay.example.com:20055",
			windowsMode: false,
			want:        true,
		},
		{
			name:        "no_proxy port specific match",
			list:        "relay.example.com:20055",
			target:      "https://relay.example.com:20055",
			windowsMode: false,
			want:        true,
		},
		{
			name:        "no_proxy port mismatch",
			list:        "relay.example.com:8443",
			target:      "https://relay.example.com:20055",
			windowsMode: false,
			want:        false,
		},
		{
			name:        "star bypasses all",
			list:        "*",
			target:      "https://relay.example.com:20055",
			windowsMode: false,
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			got := matchBypassList(tc.list, u, tc.windowsMode)
			if got != tc.want {
				t.Fatalf("matchBypassList(%q, %q, %v) = %v, want %v", tc.list, tc.target, tc.windowsMode, got, tc.want)
			}
		})
	}
}

func TestSystemProxyFuncResolutionOrder(t *testing.T) {
	// Clear proxy env vars for deterministic testing.
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}

	origOSFunc := osSystemProxyFunc
	t.Cleanup(func() { osSystemProxyFunc = origOSFunc })

	osProxy, _ := url.Parse("http://127.0.0.1:7890")
	osSystemProxyFunc = func(u *url.URL) (*url.URL, error) {
		return osProxy, nil
	}

	// 1. Loopback always bypasses both env and OS system proxy.
	for _, raw := range []string{"https://127.0.0.1:20055/ws/user", "https://localhost:20055/ws/user", "https://[::1]:20055/ws/user"} {
		req, _ := http.NewRequest("GET", raw, nil)
		got, err := SystemProxyFunc(req)
		if err != nil || got != nil {
			t.Fatalf("loopback %s should bypass proxy, got (%v, %v)", raw, got, err)
		}
	}

	// 2. No env vars -> falls back to OS system proxy.
	req, _ := http.NewRequest("GET", "https://relay.example.com:20055/ws/user", nil)
	got, err := SystemProxyFunc(req)
	if err != nil || got == nil || got.String() != "http://127.0.0.1:7890" {
		t.Fatalf("expected OS fallback http://127.0.0.1:7890, got (%v, %v)", got, err)
	}

	// 3. Env var overrides OS system proxy.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8888")
	got, err = SystemProxyFunc(req)
	if err != nil || got == nil || got.String() != "http://127.0.0.1:8888" {
		t.Fatalf("expected HTTPS_PROXY override http://127.0.0.1:8888, got (%v, %v)", got, err)
	}

	// 4. ALL_PROXY with socks5h:// is normalized to socks5://.
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("ALL_PROXY", "socks5h://127.0.0.1:1080")
	got, err = SystemProxyFunc(req)
	if err != nil || got == nil || got.String() != "socks5://127.0.0.1:1080" {
		t.Fatalf("expected ALL_PROXY socks5://127.0.0.1:1080, got (%v, %v)", got, err)
	}

	// 5. NO_PROXY suppresses both env proxy and OS system proxy fallback.
	t.Setenv("NO_PROXY", "example.com")
	got, err = SystemProxyFunc(req)
	if err != nil || got != nil {
		t.Fatalf("expected NO_PROXY to bypass all proxies, got (%v, %v)", got, err)
	}
}

func TestResolveOSSystemProxySmoke(t *testing.T) {
	// Exercise the real OS-level resolver to ensure it does not panic or fail on this platform.
	u, _ := url.Parse("https://relay.example.com:20055/ws/user")
	_, err := resolveOSSystemProxy(u)
	if err != nil {
		t.Fatalf("resolveOSSystemProxy returned unexpected error: %v", err)
	}
}

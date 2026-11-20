package agent

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// osSystemProxyFunc is a package-level hook so unit tests can mock OS-level proxy discovery.
var osSystemProxyFunc = resolveOSSystemProxy

// SystemProxyFunc resolves the outbound proxy URL for an HTTP or WebSocket request.
//
// Resolution order:
//  1. Loopback targets (localhost, 127.0.0.0/8, ::1) always connect directly.
//  2. Environment variables (HTTPS_PROXY/HTTP_PROXY/ALL_PROXY, respecting NO_PROXY).
//  3. OS-level system proxy settings (Windows WinHTTP IE/PAC settings; Linux GNOME proxy).
func SystemProxyFunc(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return resolveSystemProxy(req.URL)
}

func resolveSystemProxy(targetURL *url.URL) (*url.URL, error) {
	if targetURL == nil {
		return nil, nil
	}
	host := targetURL.Hostname()
	if host == "" || isLoopbackHost(host) {
		return nil, nil
	}

	// 1. Environment variables take precedence over OS system proxy settings.
	if proxyURL, handled, err := proxyFromEnv(targetURL); handled {
		return proxyURL, err
	}

	// 2. Fall back to OS-level system proxy settings.
	if osSystemProxyFunc != nil {
		return osSystemProxyFunc(targetURL)
	}
	return nil, nil
}

func proxyFromEnv(targetURL *url.URL) (*url.URL, bool, error) {
	noProxy := getEnvAny("NO_PROXY", "no_proxy")
	if noProxy != "" && matchBypassList(noProxy, targetURL, false) {
		return nil, true, nil
	}

	scheme := normalizeTargetScheme(targetURL.Scheme)
	var raw string
	switch scheme {
	case "https":
		raw = getEnvAny("HTTPS_PROXY", "https_proxy")
	case "http":
		raw = getEnvAny("HTTP_PROXY", "http_proxy")
	}
	if raw == "" {
		raw = getEnvAny("ALL_PROXY", "all_proxy")
	}
	if raw == "" {
		return nil, false, nil
	}

	u, err := parseProxyURL(raw, "http")
	if err != nil {
		return nil, true, err
	}
	return u, true, nil
}

func getEnvAny(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func normalizeTargetScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "wss", "https":
		return "https"
	case "ws", "http":
		return "http"
	default:
		return strings.ToLower(strings.TrimSpace(scheme))
	}
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// parseProxyURL parses a proxy specification into a *url.URL suitable for both
// net/http.Transport and gorilla/websocket.Dialer.
// Scheme-less host:port values default to defaultScheme (typically "http").
// "socks://" and "socks5h://" are normalized to "socks5://" because both net/http
// and gorilla/websocket send domain names through SOCKS5 when using "socks5://".
func parseProxyURL(raw, defaultScheme string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	if !strings.Contains(s, "://") {
		if defaultScheme == "" {
			defaultScheme = "http"
		}
		s = defaultScheme + "://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "socks", "socks5", "socks5h":
		u.Scheme = "socks5"
	case "http", "https":
		u.Scheme = strings.ToLower(u.Scheme)
	}
	return u, nil
}

// parseProxyServerList parses Windows/WinHTTP proxy strings into a *url.URL for targetScheme.
// Supported formats:
//   - Single host:port or URL: "127.0.0.1:7890", "http://127.0.0.1:7890"
//   - Protocol-mapped entries: "http=127.0.0.1:7890;https=127.0.0.1:7890;socks=127.0.0.1:7891"
//   - PAC / WinHTTP result entries: "PROXY 127.0.0.1:7890; DIRECT", "SOCKS5 127.0.0.1:1080"
func parseProxyServerList(proxyStr, targetScheme string) (*url.URL, error) {
	s := strings.TrimSpace(proxyStr)
	if s == "" {
		return nil, nil
	}
	target := normalizeTargetScheme(targetScheme)

	parts := strings.Split(s, ";")
	var (
		httpsProxy  string
		httpProxy   string
		socksProxy  string
		genericHTTP string
	)

	for _, rawPart := range parts {
		entry := strings.TrimSpace(rawPart)
		if entry == "" {
			continue
		}

		// Handle PAC-style prefixes: "DIRECT", "PROXY host:port", "HTTP host:port",
		// "HTTPS host:port", "SOCKS host:port", "SOCKS5 host:port".
		fields := strings.Fields(entry)
		if len(fields) >= 1 {
			kw := strings.ToUpper(fields[0])
			if kw == "DIRECT" {
				// If DIRECT is the first rule encountered before any proxy, connect directly.
				if httpsProxy == "" && httpProxy == "" && socksProxy == "" && genericHTTP == "" {
					return nil, nil
				}
				continue
			}
			if len(fields) >= 2 {
				val := fields[1]
				switch kw {
				case "PROXY", "HTTP":
					if genericHTTP == "" {
						genericHTTP = val
					}
					continue
				case "HTTPS":
					if !strings.Contains(val, "://") {
						val = "https://" + val
					}
					if httpsProxy == "" {
						httpsProxy = val
					}
					continue
				case "SOCKS", "SOCKS5":
					if socksProxy == "" {
						socksProxy = val
					}
					continue
				}
			}
		}

		// Handle Windows Internet Settings "scheme=host:port" mappings.
		if eqIdx := strings.IndexByte(entry, '='); eqIdx > 0 {
			proto := strings.ToLower(strings.TrimSpace(entry[:eqIdx]))
			val := strings.TrimSpace(entry[eqIdx+1:])
			if val == "" {
				continue
			}
			switch proto {
			case "https":
				if httpsProxy == "" {
					httpsProxy = val
				}
			case "http":
				if httpProxy == "" {
					httpProxy = val
				}
			case "socks", "socks5":
				if socksProxy == "" {
					socksProxy = val
				}
			}
			continue
		}

		// Plain "host:port" or "scheme://host:port" applies to all protocols.
		if genericHTTP == "" {
			genericHTTP = entry
		}
	}

	if target == "https" {
		if httpsProxy != "" {
			return parseProxyURL(httpsProxy, "http")
		}
		if httpProxy != "" {
			return parseProxyURL(httpProxy, "http")
		}
		if genericHTTP != "" {
			return parseProxyURL(genericHTTP, "http")
		}
		if socksProxy != "" {
			return parseProxyURL(socksProxy, "socks5")
		}
		return nil, nil
	}

	if httpProxy != "" {
		return parseProxyURL(httpProxy, "http")
	}
	if genericHTTP != "" {
		return parseProxyURL(genericHTTP, "http")
	}
	if socksProxy != "" {
		return parseProxyURL(socksProxy, "socks5")
	}
	return nil, nil
}

// matchBypassList checks whether targetURL matches a comma/semicolon-separated bypass list
// (NO_PROXY on Unix/env or ProxyOverride on Windows).
// When windowsMode is true, "<local>" bypasses unqualified hostnames (no dots) and loopback hosts.
func matchBypassList(list string, targetURL *url.URL, windowsMode bool) bool {
	if strings.TrimSpace(list) == "" || targetURL == nil {
		return false
	}
	host := strings.ToLower(targetURL.Hostname())
	if host == "" {
		return false
	}
	port := targetURL.Port()
	if port == "" {
		switch normalizeTargetScheme(targetURL.Scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	hostPort := net.JoinHostPort(host, port)
	ip := net.ParseIP(host)

	tokens := strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	for _, raw := range tokens {
		pat := strings.ToLower(strings.TrimSpace(raw))
		if pat == "" {
			continue
		}
		if pat == "*" {
			return true
		}
		if windowsMode && pat == "<local>" {
			if !strings.Contains(host, ".") || (ip != nil && ip.IsLoopback()) {
				return true
			}
			continue
		}

		// Strip optional scheme prefix from bypass pattern (e.g. "http://*.local").
		if idx := strings.Index(pat, "://"); idx >= 0 {
			pat = pat[idx+3:]
		}
		pat = strings.TrimRight(pat, "/")
		if pat == "" {
			continue
		}

		// CIDR match (e.g. 10.0.0.0/8)
		if ip != nil && strings.Contains(pat, "/") {
			if _, cidr, err := net.ParseCIDR(pat); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}

		// Exact host:port match
		if pat == hostPort {
			return true
		}

		// Split optional port from pattern (only when not a bare IPv6 literal)
		patHost := pat
		patPort := ""
		if h, p, err := net.SplitHostPort(pat); err == nil {
			patHost = h
			patPort = p
		}
		if patPort != "" && patPort != port {
			continue
		}

		// Wildcard pattern (e.g. "127.*", "*.local", "*corp*")
		if strings.Contains(patHost, "*") {
			if wildcardMatch(patHost, host) {
				return true
			}
			continue
		}

		// Leading dot suffix match (e.g. ".example.com" matches "example.com" and "a.example.com")
		if strings.HasPrefix(patHost, ".") {
			trimmed := patHost[1:]
			if host == trimmed || strings.HasSuffix(host, patHost) {
				return true
			}
			continue
		}

		// Exact host or subdomain match (standard NO_PROXY semantics: "example.com" matches "a.example.com")
		if host == patHost {
			return true
		}
		if ip == nil && strings.HasSuffix(host, "."+patHost) {
			return true
		}
	}
	return false
}

// wildcardMatch performs simple '*' wildcard matching (case-insensitive inputs expected).
func wildcardMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	last := parts[len(parts)-1]
	return strings.HasSuffix(s, last)
}

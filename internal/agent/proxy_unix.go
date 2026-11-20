//go:build !windows

package agent

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func resolveOSSystemProxy(targetURL *url.URL) (*url.URL, error) {
	if targetURL == nil || runtime.GOOS != "linux" {
		return nil, nil
	}
	// Only probe GNOME desktop proxy settings when running in a graphical session
	// and gsettings is available.
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return nil, nil
	}
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil, nil
	}

	mode := gsettingsGet("org.gnome.system.proxy", "mode")
	if mode != "manual" {
		return nil, nil
	}

	if ignore := gsettingsGetList("org.gnome.system.proxy", "ignore-hosts"); ignore != "" {
		if matchBypassList(ignore, targetURL, false) {
			return nil, nil
		}
	}

	targetScheme := normalizeTargetScheme(targetURL.Scheme)
	if targetScheme == "https" {
		if u := gsettingsProxyFor("org.gnome.system.proxy.https", "http"); u != nil {
			return u, nil
		}
	}
	if u := gsettingsProxyFor("org.gnome.system.proxy.http", "http"); u != nil {
		return u, nil
	}
	if u := gsettingsProxyFor("org.gnome.system.proxy.socks", "socks5"); u != nil {
		return u, nil
	}
	return nil, nil
}

func gsettingsProxyFor(schema, defaultScheme string) *url.URL {
	host := gsettingsGet(schema, "host")
	portStr := gsettingsGet(schema, "port")
	port, err := strconv.Atoi(portStr)
	if host == "" || err != nil || port <= 0 || port > 65535 {
		return nil
	}
	u, err := parseProxyURL(net.JoinHostPort(host, strconv.Itoa(port)), defaultScheme)
	if err != nil {
		return nil
	}
	return u
}

func gsettingsGet(schema, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gsettings", "get", schema, key).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	s = strings.TrimPrefix(s, "'")
	s = strings.TrimSuffix(s, "'")
	return strings.TrimSpace(s)
}

func gsettingsGetList(schema, key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gsettings", "get", schema, key).Output()
	if err != nil {
		return ""
	}
	// Format is e.g. "['localhost', '127.0.0.0/8', '::1']" or "@as []"
	s := strings.TrimSpace(string(out))
	idx := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if idx < 0 || end <= idx {
		return ""
	}
	inner := s[idx+1 : end]
	parts := strings.Split(inner, ",")
	var cleaned []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "'\"")
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return strings.Join(cleaned, ",")
}

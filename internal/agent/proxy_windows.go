//go:build windows

package agent

import (
	"net/url"
	"syscall"
	"unsafe"
)

const (
	winHTTPAccessTypeNoProxy    = 1
	winHTTPAccessTypeNamedProxy = 3
	winHTTPAutoProxyConfigURL   = 0x00000002
)

type winHTTPCurrentUserIEProxyConfig struct {
	AutoDetect    int32
	AutoConfigURL *uint16
	Proxy         *uint16
	ProxyBypass   *uint16
}

type winHTTPAutoProxyOptions struct {
	Flags                 uint32
	AutoDetectFlags       uint32
	AutoConfigURL         *uint16
	Reserved              uintptr
	Reserved2             uint32
	AutoLogonIfChallenged int32
}

type winHTTPProxyInfo struct {
	AccessType  uint32
	Proxy       *uint16
	ProxyBypass *uint16
}

var (
	winhttpDLL                              = syscall.NewLazyDLL("winhttp.dll")
	procWinHttpGetIEProxyConfigForCurrent   = winhttpDLL.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	procWinHttpOpen                         = winhttpDLL.NewProc("WinHttpOpen")
	procWinHttpCloseHandle                  = winhttpDLL.NewProc("WinHttpCloseHandle")
	procWinHttpSetTimeouts                  = winhttpDLL.NewProc("WinHttpSetTimeouts")
	procWinHttpGetProxyForUrl               = winhttpDLL.NewProc("WinHttpGetProxyForUrl")
	proxyKernel32DLL                        = syscall.NewLazyDLL("kernel32.dll")
	procGlobalFree                          = proxyKernel32DLL.NewProc("GlobalFree")
)

func globalFreeUTF16(p *uint16) {
	if p != nil {
		_, _, _ = procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
	}
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	const maxLen = 65536
	s := unsafe.Slice(p, maxLen)
	for i, v := range s {
		if v == 0 {
			return syscall.UTF16ToString(s[:i])
		}
	}
	return syscall.UTF16ToString(s)
}

func resolveOSSystemProxy(targetURL *url.URL) (*url.URL, error) {
	if targetURL == nil || procWinHttpGetIEProxyConfigForCurrent.Find() != nil {
		return nil, nil
	}

	var cfg winHTTPCurrentUserIEProxyConfig
	r, _, _ := procWinHttpGetIEProxyConfigForCurrent.Call(uintptr(unsafe.Pointer(&cfg)))
	defer globalFreeUTF16(cfg.AutoConfigURL)
	defer globalFreeUTF16(cfg.Proxy)
	defer globalFreeUTF16(cfg.ProxyBypass)

	if r == 0 {
		return nil, nil
	}

	proxyStr := utf16PtrToString(cfg.Proxy)
	bypassStr := utf16PtrToString(cfg.ProxyBypass)
	autoConfigURL := utf16PtrToString(cfg.AutoConfigURL)

	// 1. Static / manual Windows system proxy (Settings -> Proxy / Clash / v2rayN / etc.)
	if proxyStr != "" {
		if bypassStr != "" && matchBypassList(bypassStr, targetURL, true) {
			return nil, nil
		}
		return parseProxyServerList(proxyStr, targetURL.Scheme)
	}

	// 2. Explicit PAC script URL (AutoConfigURL) evaluated via WinHTTP.
	// Note: We intentionally do not run WPAD broadcast when only fAutoDetect is true
	// without AutoConfigURL, because Windows enables fAutoDetect by default and WPAD
	// lookups can stall connections for several seconds on networks without WPAD.
	if autoConfigURL != "" {
		return resolveWindowsPACProxy(targetURL, autoConfigURL)
	}

	return nil, nil
}

func resolveWindowsPACProxy(targetURL *url.URL, pacURL string) (*url.URL, error) {
	if procWinHttpOpen.Find() != nil || procWinHttpGetProxyForUrl.Find() != nil {
		return nil, nil
	}

	uaPtr, err := syscall.UTF16PtrFromString("mcp-remoteaccess")
	if err != nil {
		return nil, nil
	}
	hSession, _, _ := procWinHttpOpen.Call(
		uintptr(unsafe.Pointer(uaPtr)),
		winHTTPAccessTypeNoProxy,
		0,
		0,
		0,
	)
	if hSession == 0 {
		return nil, nil
	}
	defer procWinHttpCloseHandle.Call(hSession)

	if procWinHttpSetTimeouts.Find() == nil {
		// Short timeouts (2s) so an unreachable PAC server never hangs startup.
		_, _, _ = procWinHttpSetTimeouts.Call(hSession, 2000, 2000, 2000, 2000)
	}

	// WinHTTP expects http:// or https:// scheme.
	httpURL := *targetURL
	httpURL.Scheme = normalizeTargetScheme(httpURL.Scheme)
	if httpURL.Scheme != "http" && httpURL.Scheme != "https" {
		httpURL.Scheme = "https"
	}

	targetPtr, err := syscall.UTF16PtrFromString(httpURL.String())
	if err != nil {
		return nil, nil
	}
	pacPtr, err := syscall.UTF16PtrFromString(pacURL)
	if err != nil {
		return nil, nil
	}

	opts := winHTTPAutoProxyOptions{
		Flags:                 winHTTPAutoProxyConfigURL,
		AutoConfigURL:         pacPtr,
		AutoLogonIfChallenged: 1,
	}
	var info winHTTPProxyInfo
	r, _, _ := procWinHttpGetProxyForUrl.Call(
		hSession,
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(unsafe.Pointer(&opts)),
		uintptr(unsafe.Pointer(&info)),
	)
	defer globalFreeUTF16(info.Proxy)
	defer globalFreeUTF16(info.ProxyBypass)

	if r == 0 || info.AccessType != winHTTPAccessTypeNamedProxy {
		return nil, nil
	}

	bypassStr := utf16PtrToString(info.ProxyBypass)
	if bypassStr != "" && matchBypassList(bypassStr, targetURL, true) {
		return nil, nil
	}
	return parseProxyServerList(utf16PtrToString(info.Proxy), targetURL.Scheme)
}

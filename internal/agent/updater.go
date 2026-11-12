package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"mcp-remoteaccess/internal/protocol"
)

// CustomRootCAs is only used in unit tests to inject a test CA pool; in production it is nil (system CA pool).
var CustomRootCAs *x509.CertPool

func BuildClientTLSConfig(insecure bool) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: insecure,
		MinVersion:         tls.VersionTLS12,
		RootCAs:            CustomRootCAs,
	}
}

func DefaultTLSClientConfig() *tls.Config {
	return BuildClientTLSConfig(false)
}

func CleanupOldBinary() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
	_ = os.Remove(exe + ".new")
}

func ComputeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NormalizeHTTPBaseURL always returns an https:// URL in strict mode (insecure=false),
// even if the user omitted the scheme or typed http:// / ws://, preventing downgrade attacks.
// When insecure=true (explicit CLI -insecure flag), http:// is allowed if specified.
func NormalizeHTTPBaseURL(serverAddr string) (string, error) {
	return NormalizeHTTPBaseURLWithInsecure(serverAddr, false)
}

func NormalizeHTTPBaseURLWithInsecure(serverAddr string, insecure bool) (string, error) {
	s := strings.TrimSpace(serverAddr)
	if s == "" {
		return "", fmt.Errorf("empty server address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("invalid server host in %q", serverAddr)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), protocol.DefaultPort)
	}
	if insecure && (u.Scheme == "http" || u.Scheme == "ws") {
		u.Scheme = "http"
	} else {
		u.Scheme = "https"
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func NormalizeWSURL(serverAddr, wsPath string) (string, error) {
	return NormalizeWSURLWithInsecure(serverAddr, wsPath, false)
}

func NormalizeWSURLWithInsecure(serverAddr, wsPath string, insecure bool) (string, error) {
	base, err := NormalizeHTTPBaseURLWithInsecure(serverAddr, insecure)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if insecure && u.Scheme == "http" {
		u.Scheme = "ws"
	} else {
		u.Scheme = "wss"
	}
	u.Path = wsPath
	return u.String(), nil
}

func CheckAndDownloadUpdate(serverAddr, userKey, targetExePath, goos, goarch string) (bool, error) {
	return CheckAndDownloadUpdateWithInsecure(serverAddr, userKey, targetExePath, goos, goarch, false)
}

func CheckAndDownloadUpdateWithInsecure(serverAddr, userKey, targetExePath, goos, goarch string, insecure bool) (bool, error) {
	baseURL, err := NormalizeHTTPBaseURLWithInsecure(serverAddr, insecure)
	if err != nil {
		return false, err
	}
	curSHA, err := ComputeFileSHA256(targetExePath)
	if err != nil {
		return false, err
	}

	tr := &http.Transport{
		TLSClientConfig: BuildClientTLSConfig(insecure),
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: tr,
	}
	manifestURL := fmt.Sprintf("%s/update/manifest?os=%s&arch=%s", baseURL, url.QueryEscape(goos), url.QueryEscape(goarch))
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set(protocol.HeaderUserKey, userKey)

	resp, err := client.Do(req)
	if err != nil && insecure && strings.HasPrefix(baseURL, "https://") {
		httpBase := "http://" + strings.TrimPrefix(baseURL, "https://")
		manifestURL = fmt.Sprintf("%s/update/manifest?os=%s&arch=%s", httpBase, url.QueryEscape(goos), url.QueryEscape(goarch))
		if req2, reqErr := http.NewRequest("GET", manifestURL, nil); reqErr == nil {
			req2.Header.Set(protocol.HeaderUserKey, userKey)
			if resp2, err2 := client.Do(req2); err2 == nil {
				baseURL = httpBase
				resp = resp2
				err = nil
			}
		}
	}
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("manifest status %d", resp.StatusCode)
	}

	var manifest protocol.UpdateManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return false, err
	}
	if !manifest.Available || manifest.SHA256 == "" || strings.EqualFold(manifest.SHA256, curSHA) {
		return false, nil
	}

	dlClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: tr,
	}
	binURL := fmt.Sprintf("%s/update/binary?os=%s&arch=%s", baseURL, url.QueryEscape(goos), url.QueryEscape(goarch))
	dlReq, err := http.NewRequest("GET", binURL, nil)
	if err != nil {
		return false, err
	}
	dlReq.Header.Set(protocol.HeaderUserKey, userKey)

	dlResp, err := dlClient.Do(dlReq)
	if err != nil {
		return false, err
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("download status %d", dlResp.StatusCode)
	}

	newPath := targetExePath + ".new"
	_ = os.Remove(newPath)
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return false, err
	}
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	_, copyErr := io.Copy(mw, dlResp.Body)
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(newPath)
		return false, copyErr
	}

	gotSHA := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(gotSHA, manifest.SHA256) {
		_ = os.Remove(newPath)
		return false, fmt.Errorf("sha256 mismatch: expected %s, got %s", manifest.SHA256, gotSHA)
	}

	oldPath := targetExePath + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(targetExePath, oldPath); err != nil {
		_ = os.Remove(newPath)
		return false, fmt.Errorf("rename running binary to .old failed: %w", err)
	}
	if err := os.Rename(newPath, targetExePath); err != nil {
		_ = os.Rename(oldPath, targetExePath)
		return false, fmt.Errorf("rename .new to target failed: %w", err)
	}
	return true, nil
}

func SelfUpdateAndRestart(serverAddr, userKey string, insecure bool, onStatus func(string)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if onStatus != nil {
		onStatus("Checking for updates...")
	}
	updated, err := CheckAndDownloadUpdateWithInsecure(serverAddr, userKey, exe, runtime.GOOS, runtime.GOARCH, insecure)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	if onStatus != nil {
		onStatus("Update applied, restarting...")
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}

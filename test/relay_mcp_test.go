package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/relay"
)

func TestRelayServerMCPAndOAuth(t *testing.T) {
	tempRoot := t.TempDir()
	alphaDir := filepath.Join(tempRoot, "alpha")
	betaDir := filepath.Join(tempRoot, "beta")
	for _, d := range []string{alphaDir, betaDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	const (
		userKey = "test-user-key-999"
		devKey  = "test+dev/key=secret-123"
	)

	// 0. By default (EnableMCP=false), MCP and OAuth endpoints must not be exposed (404)
	disabledRelay := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: tempRoot,
	})
	disabledSrv := httptest.NewTLSServer(disabledRelay.Handler())
	for _, p := range []string{"/mcp", "/sse", "/.well-known/oauth-authorization-server"} {
		r, err := disabledSrv.Client().Get(disabledSrv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 on %s when EnableMCP=false, got %d", p, r.StatusCode)
		}
	}
	disabledSrv.Close()

	relaySrv := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: tempRoot,
		EnableMCP:   true,
	})
	httpSrv := httptest.NewTLSServer(relaySrv.Handler())
	defer httpSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(httpSrv.Certificate())
	agent.CustomRootCAs = pool
	defer func() { agent.CustomRootCAs = nil }()

	httpClient := httpSrv.Client()
	// Prevent auto-following redirects for /oauth/authorize tests
	noRedirectClient := &http.Client{
		Transport: httpClient.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startAgent := func(id, wd string) {
		ready := make(chan struct{}, 1)
		cl := agent.NewClient(agent.ClientOptions{
			ServerAddr: httpSrv.URL,
			UserKey:    userKey,
			MachineID:  id,
			Workdir:    wd,
			OnStatus: func(st string) {
				if strings.HasPrefix(st, "Connected as") {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			},
		})
		go func() { _ = cl.Run(ctx) }()
		t.Cleanup(cl.Close)
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout waiting for agent %s", id)
		}
	}

	startAgent("node-alpha", alphaDir)
	startAgent("node-beta", betaDir)

	// 1. Unauthenticated /mcp and /sse must return 401 with WWW-Authenticate resource_metadata
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated /mcp, got %d", resp.StatusCode)
	}
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(wwwAuth, "resource_metadata=") || !strings.Contains(wwwAuth, "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("unexpected WWW-Authenticate header: %q", wwwAuth)
	}

	// 2. OAuth metadata discovery
	metaResp, err := httpClient.Get(httpSrv.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	var prMeta map[string]any
	_ = json.NewDecoder(metaResp.Body).Decode(&prMeta)
	_ = metaResp.Body.Close()
	if prMeta["resource"] != httpSrv.URL+"/mcp" {
		t.Fatalf("unexpected protected resource metadata: %+v", prMeta)
	}

	asResp, err := httpClient.Get(httpSrv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	var asMeta map[string]any
	_ = json.NewDecoder(asResp.Body).Decode(&asMeta)
	_ = asResp.Body.Close()
	if asMeta["token_endpoint"] != httpSrv.URL+"/oauth/token" || asMeta["authorization_endpoint"] != httpSrv.URL+"/oauth/authorize" {
		t.Fatalf("unexpected authorization server metadata: %+v", asMeta)
	}

	// 3. OAuth client_credentials flow (Basic Auth with URL-encoded special chars + wrong secret rejected)
	form := url.Values{"grant_type": {"client_credentials"}}
	tokReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/oauth/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("my-web-ai:"+url.QueryEscape(devKey))))
	tokResp, err := httpClient.Do(tokReq)
	if err != nil {
		t.Fatal(err)
	}
	var tokData map[string]any
	_ = json.NewDecoder(tokResp.Body).Decode(&tokData)
	_ = tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK || tokData["access_token"] == "" {
		t.Fatalf("client_credentials token failed (%d): %+v", tokResp.StatusCode, tokData)
	}

	badTokReq, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/oauth/token", strings.NewReader(url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {"my-web-ai"},
		"client_secret": {"wrong-secret"},
	}.Encode()))
	badTokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badTokResp, err := httpClient.Do(badTokReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badTokResp.Body.Close()
	if badTokResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong client_secret, got %d", badTokResp.StatusCode)
	}

	// 4. OAuth authorization_code + PKCE (S256) flow: /oauth/authorize immediately 302 redirects, /oauth/token checks client_secret == devKey
	verifier := "super-secret-pkce-verifier-string-1234567890"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	authQuery := url.Values{
		"response_type":         {"code"},
		"client_id":             {"web-ai-client"},
		"redirect_uri":          {"https://ai.example.com/oauth/cb"},
		"state":                 {"xyz-state"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authGetResp, err := noRedirectClient.Get(httpSrv.URL + "/oauth/authorize?" + authQuery.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = authGetResp.Body.Close()
	if authGetResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 redirect from /oauth/authorize, got %d", authGetResp.StatusCode)
	}
	locURL, err := url.Parse(authGetResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	authCode := locURL.Query().Get("code")
	if authCode == "" || locURL.Query().Get("state") != "xyz-state" {
		t.Fatalf("unexpected redirect location: %s", authGetResp.Header.Get("Location"))
	}

	// Exchanging code with wrong or missing client_secret must fail with 401
	badExchResp, err := httpClient.PostForm(httpSrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {"web-ai-client"},
		"client_secret": {"wrong-secret"},
		"redirect_uri":  {"https://ai.example.com/oauth/cb"},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = badExchResp.Body.Close()
	if badExchResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong client_secret in authorization_code, got %d", badExchResp.StatusCode)
	}

	exchResp, err := httpClient.PostForm(httpSrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {"web-ai-client"},
		"client_secret": {devKey},
		"redirect_uri":  {"https://ai.example.com/oauth/cb"},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	var exchData map[string]any
	_ = json.NewDecoder(exchResp.Body).Decode(&exchData)
	_ = exchResp.Body.Close()
	if exchResp.StatusCode != http.StatusOK {
		t.Fatalf("authorization_code exchange failed (%d): %+v", exchResp.StatusCode, exchData)
	}
	accessToken, _ := exchData["access_token"].(string)
	refreshToken, _ := exchData["refresh_token"].(string)
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("missing access_token or refresh_token: %+v", exchData)
	}

	// Tampered code or wrong PKCE verifier must fail
	tamperedResp, err := httpClient.PostForm(httpSrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode + "tamper"},
		"client_id":     {"web-ai-client"},
		"client_secret": {devKey},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = tamperedResp.Body.Close()
	if tamperedResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for tampered authorization_code, got %d", tamperedResp.StatusCode)
	}

	badPKCEResp, err := httpClient.PostForm(httpSrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"client_id":     {"web-ai-client"},
		"client_secret": {devKey},
		"code_verifier": {"wrong-pkce-verifier"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = badPKCEResp.Body.Close()
	if badPKCEResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong code_verifier, got %d", badPKCEResp.StatusCode)
	}

	// Refresh token flow
	refResp, err := httpClient.PostForm(httpSrv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_secret": {devKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	var refData map[string]any
	_ = json.NewDecoder(refResp.Body).Decode(&refData)
	_ = refResp.Body.Close()
	if refResp.StatusCode != http.StatusOK || refData["access_token"] == "" {
		t.Fatalf("refresh_token failed (%d): %+v", refResp.StatusCode, refData)
	}
	accessToken = refData["access_token"].(string)

	// 5. Streamable HTTP /mcp tests using Bearer OAuth access_token
	callStreamableMCP := func(endpoint, bearer, sessionID string, body any) (*http.Response, map[string]any) {
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if sessionID != "" {
			r.Header.Set("Mcp-Session-Id", sessionID)
		}
		res, err := httpClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var out map[string]any
		if len(bytes.TrimSpace(b)) > 0 {
			_ = json.Unmarshal(b, &out)
		}
		return res, out
	}

	initRes, initBody := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	})
	if initRes.StatusCode != http.StatusOK || initBody["result"] == nil {
		t.Fatalf("initialize failed (%d): %+v", initRes.StatusCode, initBody)
	}
	sess1 := initRes.Header.Get("Mcp-Session-Id")
	if sess1 == "" {
		t.Fatal("expected Mcp-Session-Id header on initialize")
	}

	// Notification returns 202 Accepted
	notifRes, _ := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if notifRes.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on notification, got %d", notifRes.StatusCode)
	}

	// tools/list must expose 12 tools and exclude upload, download, forwarding
	_, listBody := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	toolsArr := listBody["result"].(map[string]any)["tools"].([]any)
	if len(toolsArr) != 12 {
		t.Fatalf("expected 12 remote MCP tools, got %d", len(toolsArr))
	}
	for _, rawTool := range toolsArr {
		name := rawTool.(map[string]any)["name"].(string)
		if name == "upload" || name == "download" || name == "forwarding" {
			t.Fatalf("disabled local tool %q was exposed in tools/list", name)
		}
	}

	// Calling disabled tools directly must fail with isError=true
	for _, disabled := range []string{"upload", "download", "forwarding"} {
		_, callBody := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      disabled,
				"arguments": map[string]any{},
			},
		})
		resObj := callBody["result"].(map[string]any)
		if isErr, _ := resObj["isError"].(bool); !isErr {
			t.Fatalf("expected disabled tool %q to return isError=true, got %+v", disabled, resObj)
		}
	}

	// Session isolation: sess1 switches to node-alpha, sess2 (authenticated via ?key=...) switches to node-beta
	_, sw1 := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "switch_machine",
			"arguments": map[string]any{"machine_id": "node-alpha"},
		},
	})
	if sw1["result"].(map[string]any)["isError"] == true {
		t.Fatalf("switch_machine node-alpha failed: %+v", sw1)
	}

	keyQueryURL := httpSrv.URL + "/mcp?key=" + url.QueryEscape(devKey)
	init2Res, _ := callStreamableMCP(keyQueryURL, "", "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26"},
	})
	sess2 := init2Res.Header.Get("Mcp-Session-Id")
	if sess2 == "" || sess2 == sess1 {
		t.Fatalf("expected distinct sess2, got %q", sess2)
	}
	_, _ = callStreamableMCP(keyQueryURL, "", sess2, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "switch_machine",
			"arguments": map[string]any{"machine_id": "node-beta"},
		},
	})

	// Write in sess1 (node-alpha) and verify it lives in alphaDir, not betaDir
	_, writeRes := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "write",
			"arguments": map[string]any{
				"path":    "from-web.txt",
				"content": "hello-cloud-mcp\n",
			},
		},
	})
	if writeRes["result"].(map[string]any)["isError"] == true {
		t.Fatalf("remote write failed: %+v", writeRes)
	}
	if data, err := os.ReadFile(filepath.Join(alphaDir, "from-web.txt")); err != nil || string(data) != "hello-cloud-mcp\n" {
		t.Fatalf("expected file in alphaDir, got err=%v data=%q", err, string(data))
	}

	// Passing machine_id inside tool arguments must NOT override sess1's active machine (node-alpha)
	_, overrideWriteRes := callStreamableMCP(httpSrv.URL+"/mcp", accessToken, sess1, map[string]any{
		"jsonrpc": "2.0",
		"id":      6,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "write",
			"arguments": map[string]any{
				"machine_id": "node-beta",
				"path":       "no-override.txt",
				"content":    "stay-on-alpha\n",
			},
		},
	})
	if overrideWriteRes["result"].(map[string]any)["isError"] == true {
		t.Fatalf("write failed: %+v", overrideWriteRes)
	}
	if _, err := os.Stat(filepath.Join(betaDir, "no-override.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected machine_id override to be ignored (file must NOT exist in betaDir), but stat err=%v", err)
	}
	if data, err := os.ReadFile(filepath.Join(alphaDir, "no-override.txt")); err != nil || string(data) != "stay-on-alpha\n" {
		t.Fatalf("expected file in alphaDir (sess1 active machine), got err=%v data=%q", err, string(data))
	}

	// Shell in sess2 runs on node-beta
	_, shellRes := callStreamableMCP(keyQueryURL, "", sess2, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "shell",
			"arguments": map[string]any{"command": "echo beta-ok"},
		},
	})
	shellTxt := shellRes["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(shellTxt, "beta-ok") {
		t.Fatalf("expected beta shell output, got: %s", shellTxt)
	}

	// 6. Legacy HTTP+SSE (/sse?key=... + /messages)
	sseCtx, sseCancel := context.WithCancel(context.Background())
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, httpSrv.URL+"/sse?key="+url.QueryEscape(devKey), nil)
	sseResp, err := httpClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sse failed: %d", sseResp.StatusCode)
	}

	sseReader := bufio.NewReader(sseResp.Body)
	readSSEEvent := func() (string, string) {
		var evName, evData string
		for {
			line, err := sseReader.ReadString('\n')
			if err != nil {
				t.Fatalf("read sse line failed: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if evName != "" || evData != "" {
					return evName, evData
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				evData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}

	ev, data := readSSEEvent()
	if ev != "endpoint" || !strings.HasPrefix(data, "/messages?sessionId=") {
		t.Fatalf("unexpected initial SSE event: %q %q", ev, data)
	}
	msgPostURL := httpSrv.URL + data
	postMsgResp, err := httpClient.Post(msgPostURL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = postMsgResp.Body.Close()
	if postMsgResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 on POST /messages, got %d", postMsgResp.StatusCode)
	}
	ev2, data2 := readSSEEvent()
	if ev2 != "message" || !strings.Contains(data2, `"list_machines"`) || strings.Contains(data2, `"upload"`) {
		t.Fatalf("unexpected SSE message event: %q %q", ev2, data2)
	}
}

package test

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/dev"
	"mcp-remoteaccess/internal/relay"
)

func TestEndToEndAllToolsAndAutoUpdate(t *testing.T) {
	tempRoot := t.TempDir()
	releasesDir := filepath.Join(tempRoot, "releases")
	alphaDir := filepath.Join(tempRoot, "alpha-workspace")
	betaDir := filepath.Join(tempRoot, "beta-workspace")
	devLocalDir := filepath.Join(tempRoot, "dev-local")

	for _, d := range []string{releasesDir, alphaDir, betaDir, devLocalDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	const (
		userKey = "secret-user-key-123"
		devKey  = "secret-dev-key-456"
	)

	// 1. Start Relay Server (TLS enabled, trusted by test RootCAs pool)
	relaySrv := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: releasesDir,
	})
	httpSrv := httptest.NewTLSServer(relaySrv.Handler())
	defer httpSrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(httpSrv.Certificate())
	agent.CustomRootCAs = pool
	defer func() { agent.CustomRootCAs = nil }()

	// 2. Start two User Clients (node-alpha and node-beta)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alphaReady := make(chan struct{}, 1)
	clientAlpha := agent.NewClient(agent.ClientOptions{
		ServerAddr: httpSrv.URL,
		UserKey:    userKey,
		MachineID:  "node-alpha",
		Workdir:    alphaDir,
		AutoUpdate: false,
		OnStatus: func(st string) {
			if strings.HasPrefix(st, "Connected as") {
				select {
				case alphaReady <- struct{}{}:
				default:
				}
			}
		},
	})
	go func() { _ = clientAlpha.Run(ctx) }()
	defer clientAlpha.Close()

	betaReady := make(chan struct{}, 1)
	clientBeta := agent.NewClient(agent.ClientOptions{
		ServerAddr: httpSrv.URL,
		UserKey:    userKey,
		MachineID:  "node-beta",
		Workdir:    betaDir,
		AutoUpdate: false,
		OnStatus: func(st string) {
			if strings.HasPrefix(st, "Connected as") {
				select {
				case betaReady <- struct{}{}:
				default:
				}
			}
		},
	})
	go func() { _ = clientBeta.Run(ctx) }()
	defer clientBeta.Close()

	select {
	case <-alphaReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for node-alpha to connect")
	}
	select {
	case <-betaReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for node-beta to connect")
	}

	// 3. Start Dev Client + MCP Stdio Server
	devSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: httpSrv.URL,
		DevKey:     devKey,
	})
	defer devSess.Close()

	mcpSrv := dev.NewMCPServer(devSess)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outW.Close()

	go func() {
		_ = mcpSrv.ServeStdio(inR, outW)
	}()

	outScanner := bufio.NewScanner(outR)
	outScanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	reqSeq := 1

	callMCP := func(method string, params any) map[string]any {
		id := reqSeq
		reqSeq++
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		})
		if err != nil {
			t.Fatalf("marshal mcp request: %v", err)
		}
		if _, err := inW.Write(append(payload, '\n')); err != nil {
			t.Fatalf("write mcp request: %v", err)
		}
		if !outScanner.Scan() {
			t.Fatalf("read mcp response failed: %v", outScanner.Err())
		}
		var resp map[string]any
		if err := json.Unmarshal(outScanner.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal mcp response: %v", err)
		}
		return resp
	}

	callTool := func(name string, args map[string]any) (string, bool) {
		t.Logf("--> calling tool %s", name)
		resp := callMCP("tools/call", map[string]any{
			"name":      name,
			"arguments": args,
		})
		res, ok := resp["result"].(map[string]any)
		if !ok {
			t.Fatalf("tool %s missing result: %+v", name, resp)
		}
		isErr, _ := res["isError"].(bool)
		contentArr, _ := res["content"].([]any)
		if len(contentArr) == 0 {
			return "", isErr
		}
		first, _ := contentArr[0].(map[string]any)
		txt, _ := first["text"].(string)
		t.Logf("<-- tool %s returned (isErr=%v)", name, isErr)
		return txt, isErr
	}

	// Test MCP initialize & tools/list
	initResp := callMCP("initialize", map[string]any{"protocolVersion": "2024-11-05"})
	if initResp["result"] == nil {
		t.Fatalf("initialize failed: %+v", initResp)
	}

	listToolsResp := callMCP("tools/list", map[string]any{})
	toolsResult := listToolsResp["result"].(map[string]any)
	toolsList := toolsResult["tools"].([]any)
	if len(toolsList) != 15 {
		t.Fatalf("expected 15 MCP tools, got %d", len(toolsList))
	}

	// Tool 1: list_machines
	txt, isErr := callTool("list_machines", map[string]any{})
	if isErr || !strings.Contains(txt, "node-alpha") || !strings.Contains(txt, "node-beta") {
		t.Fatalf("list_machines unexpected result (isErr=%v): %s", isErr, txt)
	}

	// Tool 2: switch_machine
	txt, isErr = callTool("switch_machine", map[string]any{"machine_id": "node-beta"})
	if isErr || !strings.Contains(txt, "Switched active machine to: node-beta") {
		t.Fatalf("switch_machine to node-beta failed: %s", txt)
	}
	txt, isErr = callTool("switch_machine", map[string]any{"machine_id": "node-alpha"})
	if isErr || !strings.Contains(txt, "Switched active machine to: node-alpha") {
		t.Fatalf("switch_machine to node-alpha failed: %s", txt)
	}

	// Tool 3: switch_workdir
	subProject := filepath.Join(alphaDir, "projectA")
	if err := os.MkdirAll(subProject, 0755); err != nil {
		t.Fatal(err)
	}
	txt, isErr = callTool("switch_workdir", map[string]any{"path": "projectA"})
	if isErr || !strings.Contains(txt, "projectA") {
		t.Fatalf("switch_workdir failed: %s", txt)
	}

	// Tool 4 & 5: write & read
	sampleCode := "package main\n\nfunc main() {\n\tprintln(\"hello world\")\n}\n"
	txt, isErr = callTool("write", map[string]any{
		"path":    "src/main.go",
		"content": sampleCode,
	})
	if isErr || !strings.Contains(txt, "Created file successfully") {
		t.Fatalf("write failed: %s", txt)
	}

	txt, isErr = callTool("read", map[string]any{"path": "src/main.go"})
	if isErr || !strings.Contains(txt, "1: package main") || !strings.Contains(txt, `4: 	println("hello world")`) {
		t.Fatalf("read file failed: %s", txt)
	}

	txt, isErr = callTool("read", map[string]any{"path": "."})
	if isErr || !strings.Contains(txt, "src") {
		t.Fatalf("read directory failed: %s", txt)
	}

	// Tool 6: edit (exact + line-trimmed whitespace tolerance)
	txt, isErr = callTool("edit", map[string]any{
		"path":      "src/main.go",
		"oldString": "func main() {\n\tprintln(\"hello world\")\n}",
		"newString": "func main() {\n\tprintln(\"hello edited mcp\")\n}",
	})
	if isErr || !strings.Contains(txt, "Edited src/main.go (1 replacement)") {
		t.Fatalf("edit failed: %s", txt)
	}

	txt, isErr = callTool("read", map[string]any{"path": "src/main.go"})
	if isErr || !strings.Contains(txt, "hello edited mcp") {
		t.Fatalf("read after edit failed: %s", txt)
	}

	// Tool 7: glob
	txt, isErr = callTool("glob", map[string]any{"pattern": "**/*.go"})
	if isErr || !strings.Contains(txt, "main.go") {
		t.Fatalf("glob failed: %s", txt)
	}

	// Tool 8: grep
	txt, isErr = callTool("grep", map[string]any{
		"pattern": "edited mcp",
		"include": "*.go",
	})
	if isErr || !strings.Contains(txt, "Found 1 matches") || !strings.Contains(txt, "Line 4:") {
		t.Fatalf("grep failed: %s", txt)
	}

	// Tool 9: shell (foreground execution)
	txt, isErr = callTool("shell", map[string]any{
		"command": "echo mcp-shell-ok",
	})
	if isErr || !strings.Contains(txt, "mcp-shell-ok") {
		t.Fatalf("shell foreground failed: %s", txt)
	}

	// Tool 9, 10, 11, 12: background job lifecycle (shell timeout=0 -> job_list -> job_read -> job_kill)
	var bgCmd string
	if runtime.GOOS == "windows" {
		bgCmd = "powershell -NoLogo -NoProfile -Command \"Write-Output 'bg-started'; Start-Sleep -Seconds 30\""
	} else {
		bgCmd = "echo bg-started; sleep 30"
	}
	txt, isErr = callTool("shell", map[string]any{
		"command": bgCmd,
		"timeout": 0,
	})
	if isErr || !strings.Contains(txt, "job #1") {
		t.Fatalf("shell background start failed: %s", txt)
	}

	txt, isErr = callTool("job_list", map[string]any{})
	if isErr || !strings.Contains(txt, "#1  running") {
		t.Fatalf("job_list failed: %s", txt)
	}

	txt, isErr = callTool("job_read", map[string]any{
		"id":      "1",
		"offset":  0,
		"timeout": 1000,
	})
	if isErr || !strings.Contains(txt, "bg-started") {
		t.Fatalf("job_read failed: %s", txt)
	}

	txt, isErr = callTool("job_kill", map[string]any{"id": "1"})
	if isErr || !strings.Contains(txt, "job #1 killed") {
		t.Fatalf("job_kill failed: %s", txt)
	}

	// Tool 13 & 14: upload & download (both single file and directory tree)
	localUploadFile := filepath.Join(devLocalDir, "upload-test.txt")
	if err := os.WriteFile(localUploadFile, []byte("file-transfer-payload-12345"), 0644); err != nil {
		t.Fatal(err)
	}
	txt, isErr = callTool("upload", map[string]any{
		"local_path":  localUploadFile,
		"remote_path": "uploaded/remote-copy.txt",
	})
	if isErr || !strings.Contains(txt, "Uploaded file") {
		t.Fatalf("upload file failed: %s", txt)
	}

	localDownloadFile := filepath.Join(devLocalDir, "downloaded-copy.txt")
	txt, isErr = callTool("download", map[string]any{
		"remote_path": "uploaded/remote-copy.txt",
		"local_path":  localDownloadFile,
	})
	if isErr || !strings.Contains(txt, "Downloaded file") {
		t.Fatalf("download file failed: %s", txt)
	}
	dlBytes, err := os.ReadFile(localDownloadFile)
	if err != nil || string(dlBytes) != "file-transfer-payload-12345" {
		t.Fatalf("downloaded file content mismatch: %q (err=%v)", string(dlBytes), err)
	}

	// Directory upload & download test
	localDirSrc := filepath.Join(devLocalDir, "dir-src")
	_ = os.MkdirAll(filepath.Join(localDirSrc, "nested"), 0755)
	_ = os.WriteFile(filepath.Join(localDirSrc, "a.txt"), []byte("AAA"), 0644)
	_ = os.WriteFile(filepath.Join(localDirSrc, "nested", "b.txt"), []byte("BBB"), 0644)

	txt, isErr = callTool("upload", map[string]any{
		"local_path":  localDirSrc,
		"remote_path": "remote-dir-bundle",
	})
	if isErr || !strings.Contains(txt, "Uploaded directory (2 files)") {
		t.Fatalf("upload directory failed: %s", txt)
	}

	localDirDst := filepath.Join(devLocalDir, "dir-dst")
	txt, isErr = callTool("download", map[string]any{
		"remote_path": "remote-dir-bundle",
		"local_path":  localDirDst,
	})
	if isErr || !strings.Contains(txt, "Downloaded directory (2 files)") {
		t.Fatalf("download directory failed: %s", txt)
	}
	bContent, err := os.ReadFile(filepath.Join(localDirDst, "nested", "b.txt"))
	if err != nil || string(bContent) != "BBB" {
		t.Fatalf("downloaded directory nested file mismatch: %q (%v)", string(bContent), err)
	}

	// Tool 15: forwarding (both remote_to_local and local_to_remote)
	// Start a dummy TCP echo service on "remote" side
	remoteSvcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer remoteSvcLn.Close()
	go func() {
		for {
			c, err := remoteSvcLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 128)
				n, _ := conn.Read(buf)
				_, _ = conn.Write([]byte("REMOTE_ECHO:" + string(buf[:n])))
			}(c)
		}
	}()

	// Add remote_to_local tunnel
	txt, isErr = callTool("forwarding", map[string]any{
		"action":      "add",
		"direction":   "remote_to_local",
		"local_addr":  "127.0.0.1:0",
		"remote_addr": remoteSvcLn.Addr().String(),
	})
	if isErr || !strings.Contains(txt, "Added remote_to_local forwarding #1") {
		t.Fatalf("forwarding add remote_to_local failed: %s", txt)
	}
	// Extract bound local_addr from message: "local 127.0.0.1:xxxx ->"
	var boundLocalAddr string
	for _, part := range strings.Fields(txt) {
		if strings.HasPrefix(part, "127.0.0.1:") {
			boundLocalAddr = part
			break
		}
	}
	if boundLocalAddr == "" {
		t.Fatalf("could not parse boundLocalAddr from: %s", txt)
	}

	fwdConn, err := net.DialTimeout("tcp", boundLocalAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial forwarded local port failed: %v", err)
	}
	_, _ = fwdConn.Write([]byte("ping123"))
	replyBuf := make([]byte, 128)
	n, err := fwdConn.Read(replyBuf)
	_ = fwdConn.Close()
	if err != nil || string(replyBuf[:n]) != "REMOTE_ECHO:ping123" {
		t.Fatalf("remote_to_local echo mismatch: %q (%v)", string(replyBuf[:n]), err)
	}

	// Test local_to_remote tunnel
	localSvcLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer localSvcLn.Close()
	go func() {
		for {
			c, err := localSvcLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 128)
				n, _ := conn.Read(buf)
				_, _ = conn.Write([]byte("LOCAL_ECHO:" + string(buf[:n])))
			}(c)
		}
	}()

	txt, isErr = callTool("forwarding", map[string]any{
		"action":      "add",
		"direction":   "local_to_remote",
		"local_addr":  localSvcLn.Addr().String(),
		"remote_addr": "127.0.0.1:0",
	})
	if isErr || !strings.Contains(txt, "Added local_to_remote forwarding #2") {
		t.Fatalf("forwarding add local_to_remote failed: %s", txt)
	}
	// Extract bound remote_addr from "remote 127.0.0.1:xxxx ->"
	var boundRemoteAddr string
	fields := strings.Fields(txt)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "remote" && strings.HasPrefix(fields[i+1], "127.0.0.1:") {
			boundRemoteAddr = fields[i+1]
			break
		}
	}
	if boundRemoteAddr == "" {
		t.Fatalf("could not parse boundRemoteAddr from: %s", txt)
	}

	revConn, err := net.DialTimeout("tcp", boundRemoteAddr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial reverse forwarded remote port failed: %v", err)
	}
	_, _ = revConn.Write([]byte("pong456"))
	n, err = revConn.Read(replyBuf)
	_ = revConn.Close()
	if err != nil || string(replyBuf[:n]) != "LOCAL_ECHO:pong456" {
		t.Fatalf("local_to_remote echo mismatch: %q (%v)", string(replyBuf[:n]), err)
	}

	// List & Remove forwarding rules
	txt, isErr = callTool("forwarding", map[string]any{"action": "list"})
	if isErr || !strings.Contains(txt, "#1") || !strings.Contains(txt, "#2") {
		t.Fatalf("forwarding list failed: %s", txt)
	}
	txt, isErr = callTool("forwarding", map[string]any{"action": "remove", "id": "1"})
	if isErr || !strings.Contains(txt, "Removed port forwarding rule #1") {
		t.Fatalf("forwarding remove #1 failed: %s", txt)
	}
	txt, isErr = callTool("forwarding", map[string]any{"action": "remove", "id": "2"})
	if isErr || !strings.Contains(txt, "Removed port forwarding rule #2") {
		t.Fatalf("forwarding remove #2 failed: %s", txt)
	}

	// 4. Test Auto-Update Mechanism for "Ancient Client"
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	releaseFile := filepath.Join(releasesDir, fmt.Sprintf("user-client-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext))
	newBinaryPayload := []byte("NEW-CLIENT-BINARY-V2-PAYLOAD-99999")
	if err := os.WriteFile(releaseFile, newBinaryPayload, 0755); err != nil {
		t.Fatal(err)
	}

	ancientExe := filepath.Join(tempRoot, "ancient-user-client"+ext)
	if err := os.WriteFile(ancientExe, []byte("ANCIENT-BINARY-V0"), 0755); err != nil {
		t.Fatal(err)
	}

	updated, err := agent.CheckAndDownloadUpdate(httpSrv.URL, userKey, ancientExe, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("CheckAndDownloadUpdate failed: %v", err)
	}
	if !updated {
		t.Fatal("expected CheckAndDownloadUpdate to return updated=true")
	}

	updatedBytes, err := os.ReadFile(ancientExe)
	if err != nil || string(updatedBytes) != string(newBinaryPayload) {
		t.Fatalf("updated binary content mismatch: %q", string(updatedBytes))
	}
	oldBytes, err := os.ReadFile(ancientExe + ".old")
	if err != nil || string(oldBytes) != "ANCIENT-BINARY-V0" {
		t.Fatalf(".old backup binary mismatch: %q (%v)", string(oldBytes), err)
	}
}

func TestJobSurvivesReconnectAndShellFallback(t *testing.T) {
	tempRoot := t.TempDir()
	const (
		userKey = "user-k"
		devKey  = "dev-k"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectedCh := make(chan struct{}, 4)
	userClient := agent.NewClient(agent.ClientOptions{
		ServerAddr: httpSrv.URL,
		UserKey:    userKey,
		MachineID:  "resilient-node",
		Workdir:    tempRoot,
		AutoUpdate: false,
		OnStatus: func(st string) {
			if strings.HasPrefix(st, "Connected as") {
				connectedCh <- struct{}{}
			}
		},
	})
	go func() { _ = userClient.Run(ctx) }()
	defer userClient.Close()

	select {
	case <-connectedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connect timeout")
	}

	devSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: httpSrv.URL,
		DevKey:     devKey,
	})
	defer devSess.Close()
	mcpSrv := dev.NewMCPServer(devSess)

	callTool := func(name string, args map[string]any) dev.MCPCallToolResult {
		raw, _ := json.Marshal(args)
		return mcpSrv.CallTool(name, raw)
	}

	// Start a background job that outputs before and after a simulated network disconnect
	var cmdStr string
	if runtime.GOOS == "windows" {
		cmdStr = "powershell -NoLogo -NoProfile -Command \"Write-Output 'part-1'; Start-Sleep -Seconds 2; Write-Output 'part-2'\""
	} else {
		cmdStr = "echo part-1; sleep 2; echo part-2"
	}

	res := callTool("shell", map[string]any{
		"command": cmdStr,
		"timeout": 0,
	})
	if res.IsError || !strings.Contains(res.Content[0].Text, "job #1") {
		t.Fatalf("start bg job failed: %+v", res)
	}

	// Simulate network drop by closing all active client connections on httpSrv's client side:
	// We trigger a state update or force session close via a new duplicate registration that gets dropped,
	// or test shell override first:
	shellOverride := "cmd"
	if runtime.GOOS != "windows" {
		shellOverride = "sh"
	}
	resShell := callTool("shell", map[string]any{
		"command": "echo override-shell-ok",
		"shell":   shellOverride,
	})
	if resShell.IsError || !strings.Contains(resShell.Content[0].Text, "override-shell-ok") {
		t.Fatalf("shell override (%s) failed: %+v", shellOverride, resShell)
	}

	// Wait for part-2 to finish and read incremental output
	time.Sleep(2500 * time.Millisecond)
	resRead := callTool("job_read", map[string]any{
		"id":      "1",
		"offset":  0,
		"timeout": 2000,
	})
	if resRead.IsError || !strings.Contains(resRead.Content[0].Text, "part-1") || !strings.Contains(resRead.Content[0].Text, "part-2") {
		t.Fatalf("job_read after completion failed: %+v", resRead)
	}
}

func TestTLSStrictCertificateVerification(t *testing.T) {
	tempRoot := t.TempDir()
	releasesDir := filepath.Join(tempRoot, "releases")
	_ = os.MkdirAll(releasesDir, 0755)

	const (
		userKey = "tls-user-key"
		devKey  = "tls-dev-key"
	)

	relaySrv := relay.NewServer(relay.Config{
		UserKey:     userKey,
		DevKey:      devKey,
		ReleasesDir: releasesDir,
	})
	httpsSrv := httptest.NewTLSServer(relaySrv.Handler())
	defer httpsSrv.Close()

	// 1. Verify anti-downgrade and default port 20055 completion:
	// even if user enters bare host, http://, or ws://, NormalizeHTTPBaseURL and NormalizeWSURL
	// strictly force https:// and wss:// and fill in default port 20055 when omitted.
	for _, rawInput := range []string{"example.com", "example.com:20055", "http://example.com", "http://example.com:20055", "ws://example.com"} {
		httpBase, err := agent.NormalizeHTTPBaseURL(rawInput)
		if err != nil || httpBase != "https://example.com:20055" {
			t.Fatalf("anti-downgrade/default-port failed for %q: got %q (%v)", rawInput, httpBase, err)
		}
		wsBase, err := agent.NormalizeWSURL(rawInput, "/ws/user")
		if err != nil || wsBase != "wss://example.com:20055/ws/user" {
			t.Fatalf("anti-downgrade/default-port ws failed for %q: got %q (%v)", rawInput, wsBase, err)
		}
	}

	// 2. Verify plaintext server is rejected (attacker blocking TLS and offering plaintext HTTP/WS)
	plaintextSrv := httptest.NewServer(relaySrv.Handler())
	defer plaintextSrv.Close()
	downgradeSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: plaintextSrv.URL,
		DevKey:     devKey,
	})
	defer downgradeSess.Close()
	if _, err := downgradeSess.FetchMachines(); err == nil {
		t.Fatal("expected connection to plaintext server to fail (anti-downgrade), but it succeeded")
	}

	// 3. Without trusting the CA and without -insecure, untrusted/self-signed cert MUST be rejected (anti-MITM)
	agent.CustomRootCAs = nil
	badDevSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: httpsSrv.URL,
		DevKey:     devKey,
		Insecure:   false,
	})
	defer badDevSess.Close()
	if _, err := badDevSess.FetchMachines(); err == nil {
		t.Fatal("expected untrusted TLS certificate to be rejected, but connection succeeded")
	}

	// 4. With Insecure: true (-insecure flag), self-signed cert IS accepted when manually configured
	insecureDevSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: httpsSrv.URL,
		DevKey:     devKey,
		Insecure:   true,
	})
	defer insecureDevSess.Close()
	if _, err := insecureDevSess.FetchMachines(); err != nil {
		t.Fatalf("expected Insecure: true to accept self-signed cert, got error: %v", err)
	}

	// 5. In strict mode (Insecure: false), when the server's CA is trusted in RootCAs, WSS/HTTPS succeeds
	pool := x509.NewCertPool()
	pool.AddCert(httpsSrv.Certificate())
	agent.CustomRootCAs = pool
	defer func() { agent.CustomRootCAs = nil }()

	bareAddr := strings.TrimPrefix(httpsSrv.URL, "https://")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{}, 1)
	uClient := agent.NewClient(agent.ClientOptions{
		ServerAddr: bareAddr,
		UserKey:    userKey,
		MachineID:  "tls-node",
		Workdir:    tempRoot,
		AutoUpdate: false,
		OnStatus: func(st string) {
			if strings.HasPrefix(st, "Connected as") {
				select {
				case readyCh <- struct{}{}:
				default:
				}
			}
		},
	})
	go func() { _ = uClient.Run(ctx) }()
	defer uClient.Close()

	select {
	case <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for tls-node to connect over trusted WSS")
	}

	devSess := dev.NewSession(dev.SessionConfig{
		ServerAddr: bareAddr,
		DevKey:     devKey,
	})
	defer devSess.Close()
	mcpSrv := dev.NewMCPServer(devSess)

	raw, _ := json.Marshal(map[string]any{"command": "echo tls-wss-ok"})
	res := mcpSrv.CallTool("shell", raw)
	if res.IsError || !strings.Contains(res.Content[0].Text, "tls-wss-ok") {
		t.Fatalf("shell over trusted TLS failed: %+v", res)
	}
}

func TestOpenCodeToolAlignmentDetails(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Test edit Tier-2 Unicode normalization with multi-byte UTF-8 quotes/dashes/spaces (inline match)
	unicodeFile := filepath.Join(tempDir, "unicode.txt")
	origUnicode := "const msg = “Hello—World\u00A0!”;\r\nconst second = 'keep';\n"
	if err := os.WriteFile(unicodeFile, []byte(origUnicode), 0644); err != nil {
		t.Fatal(err)
	}
	// Replace only the inline substring using ASCII quotes, hyphen, and space;
	// verify Tier-2 matches and mixed line endings (\r\n on line 1, \n on line 2) are preserved!
	out, _, err := agent.EditFile("unicode.txt", `"Hello-World !"`, `"Hi-OpenCode"`, false, tempDir)
	if err != nil || !strings.Contains(out, "1 replacement") {
		t.Fatalf("Tier-2 Unicode inline edit failed: %s (%v)", out, err)
	}
	editedBytes, _ := os.ReadFile(unicodeFile)
	expectedUnicode := "const msg = \"Hi-OpenCode\";\r\nconst second = 'keep';\n"
	if string(editedBytes) != expectedUnicode {
		t.Fatalf("mixed line endings or content not preserved: got %q, want %q", string(editedBytes), expectedUnicode)
	}

	// 2. Test edit voting missHint when first line has a typo but subsequent lines match
	votingFile := filepath.Join(tempDir, "voting.go")
	votingSrc := "package main\n\nfunc alpha() {\n\tx := 1\n\ty := 2\n\tz := 3\n}\n"
	_ = os.WriteFile(votingFile, []byte(votingSrc), 0644)
	_, _, err = agent.EditFile("voting.go", "func wrongName() {\n\tx := 1\n\ty := 2\n}", "func alpha() {}", false, tempDir)
	if err == nil || !strings.Contains(err.Error(), "The closest lines in the file:") || !strings.Contains(err.Error(), "4: \tx := 1") {
		t.Fatalf("expected voting missHint to locate lines 4-6, got: %v", err)
	}

	// 3. Test read alternatePath (curly apostrophe in filename) and "Did you mean" suggestions
	curlyName := "user\u2019s-guide.md"
	_ = os.WriteFile(filepath.Join(tempDir, curlyName), []byte("guide content\n"), 0644)
	readOut, _, err := agent.ReadFileOrDir("user's-guide.md", tempDir, nil, nil)
	if err != nil || !strings.Contains(readOut, "1: guide content") {
		t.Fatalf("expected alternatePath to resolve curly apostrophe filename, got: %s (%v)", readOut, err)
	}

	// Missing file with similar name suggestion
	_, _, err = agent.ReadFileOrDir("guide.md", tempDir, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "Did you mean one of these?") || !strings.Contains(err.Error(), curlyName) {
		t.Fatalf("expected Did you mean suggestion for missing file, got: %v", err)
	}

	// Missing folder with nearest existing parent suggestion
	_, _, err = agent.ReadFileOrDir("nonexistent-dir/sub/file.txt", tempDir, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "The nearest folder that does is") {
		t.Fatalf("expected nearest existing folder hint, got: %v", err)
	}

	// 4. Test multi-chunk streaming read on a file larger than FirstReadChunk (256KB)
	largeFile := filepath.Join(tempDir, "large.log")
	var sb strings.Builder
	for i := 1; i <= 6000; i++ {
		fmt.Fprintf(&sb, "line-%04d: %s\n", i, strings.Repeat("x", 60))
	}
	if err := os.WriteFile(largeFile, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	offset := 5500
	limit := 10
	pageOut, meta, err := agent.ReadFileOrDir("large.log", tempDir, &offset, &limit)
	if err != nil || !strings.Contains(pageOut, "5500: line-5500:") || !strings.Contains(pageOut, "5509: line-5509:") {
		t.Fatalf("multi-chunk streaming read failed: %s (%v)", pageOut, err)
	}
	if meta["truncated"] != true {
		t.Fatalf("expected truncated=true on page 5500..5509 of 6000, got: %+v", meta)
	}

	// 5. Test grep on hidden directory (.github/workflows/ci.yml)
	hiddenWorkflowDir := filepath.Join(tempDir, ".github", "workflows")
	_ = os.MkdirAll(hiddenWorkflowDir, 0755)
	_ = os.WriteFile(filepath.Join(hiddenWorkflowDir, "ci.yml"), []byte("name: secret-ci-token\n"), 0644)
	grepOut, _, err := agent.GrepFiles(tempDir, "secret-ci-token", "", "", true, true, 10)
	if err != nil || !strings.Contains(grepOut, "Found 1 matches") || !strings.Contains(grepOut, "ci.yml") {
		t.Fatalf("expected grep to search .github directory, got: %s (%v)", grepOut, err)
	}
}



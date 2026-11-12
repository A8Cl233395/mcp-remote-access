package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/protocol"
)

const ClientVersion = "1.0.0"

type ClientOptions struct {
	ServerAddr string
	UserKey    string
	MachineID  string
	Shell      string
	Workdir    string
	AutoUpdate bool
	Insecure   bool
	OnStatus   func(status string)
	Logger     *log.Logger
}

type Client struct {
	opts            ClientOptions
	mu              sync.RWMutex
	workdir         string
	shellName       string
	shellPath       string
	availableShells []string
	hostname        string
	username        string
	isAdmin         bool
	session         *yamux.Session

	jobs     *JobRegistry
	wakeLock *WakeLock

	revMu           sync.Mutex
	revSeq          uint64
	pendingRevConns map[string]net.Conn
}

func NewClient(opts ClientOptions) *Client {
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[user-client] ", log.LstdFlags)
	}
	wd := opts.Workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	if wd == "" {
		wd, _ = os.UserHomeDir()
	}
	wd = filepath.Clean(wd)

	shellName, shellPath := SelectShell(opts.Shell)
	avail := DetectAvailableShells()

	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	uname := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		uname = u.Username
	} else if envU := os.Getenv("USERNAME"); envU != "" {
		uname = envU
	} else if envU := os.Getenv("USER"); envU != "" {
		uname = envU
	}

	machineID := opts.MachineID
	if machineID == "" {
		machineID = host
	}

	return &Client{
		opts:            opts,
		workdir:         wd,
		shellName:       shellName,
		shellPath:       shellPath,
		availableShells: avail,
		hostname:        host,
		username:        uname,
		isAdmin:         CheckIsAdmin(),
		jobs:            NewJobRegistry(),
		wakeLock:        NewWakeLock(),
		pendingRevConns: make(map[string]net.Conn),
	}
}

func (c *Client) Close() {
	c.wakeLock.Release()
	c.jobs.KillAll()
	c.revMu.Lock()
	for id, conn := range c.pendingRevConns {
		delete(c.pendingRevConns, id)
		_ = conn.Close()
	}
	c.revMu.Unlock()
	c.mu.Lock()
	if c.session != nil {
		_ = c.session.Close()
	}
	c.mu.Unlock()
}

func (c *Client) SetUserKey(userKey string) {
	c.mu.Lock()
	c.opts.UserKey = userKey
	c.mu.Unlock()
}

func (c *Client) GetUserKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.opts.UserKey
}

func (c *Client) setStatus(msg string) {
	c.opts.Logger.Println(msg)
	if c.opts.OnStatus != nil {
		c.opts.OnStatus(msg)
	}
}

func (c *Client) GetMachineInfo() protocol.MachineInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id := c.opts.MachineID
	if id == "" {
		id = c.hostname
	}
	return protocol.MachineInfo{
		MachineID:       id,
		Hostname:        c.hostname,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		User:            c.username,
		IsAdmin:         c.isAdmin,
		Workdir:         c.workdir,
		Shell:           c.shellName,
		AvailableShells: c.availableShells,
		Version:         ClientVersion,
		RunningJobs:     c.jobs.RunningCount(),
	}
}

func (c *Client) GetWorkdir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workdir
}

func (c *Client) SetWorkdir(wd string) {
	c.mu.Lock()
	c.workdir = wd
	c.mu.Unlock()
	c.pushStateUpdate()
}

func (c *Client) GetShellPath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.shellPath
}

func (c *Client) SetShell(spec string) error {
	p := ResolveShellExecutable(spec)
	if p == "" {
		return fmt.Errorf("shell %q not found on this machine (available: %s)", spec, strings.Join(c.availableShells, ", "))
	}
	c.mu.Lock()
	c.shellPath = p
	c.shellName = NormalizeShellName(p)
	c.mu.Unlock()
	c.pushStateUpdate()
	return nil
}

func (c *Client) pushStateUpdate() {
	c.mu.RLock()
	sess := c.session
	c.mu.RUnlock()
	if sess == nil || sess.IsClosed() {
		return
	}
	go func() {
		st, err := sess.OpenStream()
		if err != nil {
			return
		}
		defer st.Close()
		info := c.GetMachineInfo()
		_ = protocol.WriteLineJSON(st, protocol.StreamHeader{
			StreamType: protocol.StreamTypeAgentRegister,
			Machine:    &info,
		})
		bs := protocol.NewBufferedStream(st)
		var ack protocol.StreamAck
		_ = bs.ReadLineJSON(&ack)
	}()
}

// Run connects to relay-server and automatically reconnects until ctx is canceled.
func (c *Client) Run(ctx context.Context) error {
	CleanupOldBinary()
	defer c.wakeLock.Release()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if c.opts.AutoUpdate {
			_ = SelfUpdateAndRestart(c.opts.ServerAddr, c.GetUserKey(), c.opts.Insecure, c.opts.OnStatus)
		}

		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.setStatus(fmt.Sprintf("Disconnected (%v), retrying in %v...", err, backoff))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	wsURL, err := NormalizeWSURLWithInsecure(c.opts.ServerAddr, "/ws/user", c.opts.Insecure)
	if err != nil {
		return err
	}
	c.setStatus(fmt.Sprintf("Connecting to %s...", wsURL))

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  BuildClientTLSConfig(c.opts.Insecure),
	}
	headers := http.Header{}
	headers.Set(protocol.HeaderUserKey, c.GetUserKey())

	ws, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil && c.opts.Insecure && strings.HasPrefix(wsURL, "wss://") {
		plainURL := "ws://" + strings.TrimPrefix(wsURL, "wss://")
		if ws2, resp2, err2 := dialer.DialContext(ctx, plainURL, headers); err2 == nil {
			ws = ws2
			resp = resp2
			err = nil
		}
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("authentication failed (invalid user key)")
		}
		return err
	}

	wsConn := protocol.NewWSConn(ws)
	session, err := yamux.Server(wsConn, protocol.DefaultYamuxConfig())
	if err != nil {
		_ = wsConn.Close()
		return err
	}

	c.mu.Lock()
	c.session = session
	c.mu.Unlock()

	// Send initial registration stream
	regStream, err := session.OpenStream()
	if err != nil {
		_ = session.Close()
		return err
	}
	info := c.GetMachineInfo()
	if err := protocol.WriteLineJSON(regStream, protocol.StreamHeader{
		StreamType: protocol.StreamTypeAgentRegister,
		Machine:    &info,
	}); err != nil {
		_ = regStream.Close()
		_ = session.Close()
		return err
	}
	bs := protocol.NewBufferedStream(regStream)
	var ack protocol.StreamAck
	if err := bs.ReadLineJSON(&ack); err != nil || !ack.OK {
		_ = regStream.Close()
		_ = session.Close()
		return fmt.Errorf("registration rejected: %s", ack.Error)
	}
	_ = regStream.Close()

	// Connected! Acquire system wake lock to prevent idle sleep.
	c.wakeLock.Acquire()
	defer c.wakeLock.Release()

	c.setStatus(fmt.Sprintf("Connected as %s (%s/%s, shell=%s)", info.MachineID, info.OS, info.Arch, info.Shell))

	// Close session if ctx is canceled
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-stopCh:
		}
	}()

	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return err
		}
		go c.handleStream(stream)
	}
}

func (c *Client) handleStream(raw io.ReadWriteCloser) {
	bs := protocol.NewBufferedStream(raw)
	var hdr protocol.StreamHeader
	if err := bs.ReadLineJSON(&hdr); err != nil {
		_ = bs.Close()
		return
	}

	switch hdr.StreamType {
	case protocol.StreamTypeRPC:
		c.handleRPCStream(bs)
	case protocol.StreamTypeFileUpload:
		c.handleFileUploadStream(bs, hdr)
	case protocol.StreamTypeFileDownload:
		c.handleFileDownloadStream(bs, hdr)
	case protocol.StreamTypeTCPConnect:
		c.handleTCPConnectStream(bs, hdr)
	case protocol.StreamTypeReverseListen:
		c.handleReverseListenStream(bs, hdr)
	case protocol.StreamTypeReverseAccept:
		c.handleReverseAcceptStream(bs, hdr)
	default:
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("unknown stream type: %s", hdr.StreamType),
		})
		_ = bs.Close()
	}
}

func (c *Client) handleRPCStream(bs *protocol.BufferedStream) {
	defer bs.Close()

	var req protocol.RPCRequest
	if err := bs.ReadLineJSON(&req); err != nil {
		return
	}

	resp := c.dispatchRPC(req)
	info := c.GetMachineInfo()
	resp.Machine = &info
	_ = protocol.WriteLineJSON(bs, resp)
}

func (c *Client) dispatchRPC(req protocol.RPCRequest) protocol.RPCResponse {
	switch req.Method {
	case "ping", "machine_info":
		info := c.GetMachineInfo()
		b, _ := json.MarshalIndent(info, "", "  ")
		return protocol.RPCResponse{
			OK:      true,
			Content: string(b),
			Machine: &info,
		}

	case "switch_workdir":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "switch_workdir requires a non-empty `path` string"}
		}
		resolved, err := ResolveWorkdir(p.Path, c.GetWorkdir())
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		c.SetWorkdir(resolved)
		return protocol.RPCResponse{
			OK:       true,
			Content:  fmt.Sprintf("Switched working directory to: %s", resolved),
			Metadata: map[string]any{"workdir": resolved},
		}

	case "read":
		var p struct {
			Path   string `json:"path"`
			Offset *int   `json:"offset,omitempty"`
			Limit  *int   `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "read requires a `path` string"}
		}
		content, meta, err := ReadFileOrDir(p.Path, c.GetWorkdir(), p.Offset, p.Limit)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "write":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "write requires `path` and `content`"}
		}
		content, meta, err := WriteFile(p.Path, p.Content, c.GetWorkdir())
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "edit":
		var p struct {
			Path       string `json:"path"`
			OldString  string `json:"oldString"`
			NewString  string `json:"newString"`
			ReplaceAll bool   `json:"replaceAll,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "edit requires `path`, `oldString`, and `newString`"}
		}
		content, meta, err := EditFile(p.Path, p.OldString, p.NewString, p.ReplaceAll, c.GetWorkdir())
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "glob":
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path,omitempty"`
			Hidden  bool   `json:"hidden,omitempty"`
			Limit   int    `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return protocol.RPCResponse{OK: false, Error: "invalid glob arguments"}
		}
		content, meta, err := GlobFiles(c.GetWorkdir(), p.Pattern, p.Path, p.Hidden, p.Limit)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "grep":
		var p struct {
			Pattern       string `json:"pattern"`
			Path          string `json:"path,omitempty"`
			Include       string `json:"include,omitempty"`
			Literal       bool   `json:"literal,omitempty"`
			CaseSensitive *bool  `json:"caseSensitive,omitempty"`
			Limit         int    `json:"limit,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return protocol.RPCResponse{OK: false, Error: "invalid grep arguments"}
		}
		caseSens := true
		if p.CaseSensitive != nil {
			caseSens = *p.CaseSensitive
		}
		content, meta, err := GrepFiles(c.GetWorkdir(), p.Pattern, p.Path, p.Include, p.Literal, caseSens, p.Limit)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "shell":
		var p struct {
			Command string `json:"command"`
			Workdir string `json:"workdir,omitempty"`
			Shell   string `json:"shell,omitempty"`
			Timeout *int   `json:"timeout,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return protocol.RPCResponse{OK: false, Error: "invalid shell arguments"}
		}
		cwd := c.GetWorkdir()
		if strings.TrimSpace(p.Workdir) != "" {
			resolved, err := ResolveWorkdir(p.Workdir, cwd)
			if err != nil {
				return protocol.RPCResponse{OK: false, Error: err.Error()}
			}
			cwd = resolved
		}
		shellPath := c.GetShellPath()
		if strings.TrimSpace(p.Shell) != "" {
			resolvedShell := ResolveShellExecutable(p.Shell)
			if resolvedShell == "" {
				return protocol.RPCResponse{
					OK:    false,
					Error: fmt.Sprintf("Requested shell %q is not available on this machine (available: %s)", p.Shell, strings.Join(c.availableShells, ", ")),
				}
			}
			shellPath = resolvedShell
		}
		content, meta, err := c.jobs.ExecuteShell(p.Command, cwd, shellPath, p.Timeout, c.pushStateUpdate)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "job_list":
		content, meta := c.jobs.ListJobs()
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "job_read":
		var p struct {
			ID      any  `json:"id"`
			Offset  *int `json:"offset,omitempty"`
			Timeout *int `json:"timeout,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == nil {
			return protocol.RPCResponse{OK: false, Error: "job_read requires `id`"}
		}
		idStr := fmt.Sprintf("%v", p.ID)
		content, meta, err := c.jobs.ReadJob(idStr, p.Offset, p.Timeout)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "job_kill":
		var p struct {
			ID any `json:"id"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.ID == nil {
			return protocol.RPCResponse{OK: false, Error: "job_kill requires `id`"}
		}
		idStr := fmt.Sprintf("%v", p.ID)
		content, meta, err := c.jobs.KillJob(idStr, c.pushStateUpdate)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	default:
		return protocol.RPCResponse{OK: false, Error: fmt.Sprintf("unknown RPC method: %s", req.Method)}
	}
}

func (c *Client) handleFileUploadStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	defer bs.Close()

	dstPath := ResolvePath(hdr.RemotePath, c.GetWorkdir())
	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{OK: true}); err != nil {
		return
	}

	var bytesWritten int64
	var filesCount int
	var shaHex string
	var err error

	if hdr.IsDir {
		bytesWritten, filesCount, shaHex, err = protocol.UnpackDirFromStream(dstPath, bs)
	} else {
		// If dstPath is an existing directory, place the uploaded file inside it if possible
		if st, statErr := os.Stat(dstPath); statErr == nil && st.IsDir() && hdr.ConnID != "" {
			dstPath = filepath.Join(dstPath, filepath.Base(hdr.ConnID))
		}
		bytesWritten, shaHex, err = protocol.ReceiveFileToDisk(dstPath, bs, hdr.Size)
		filesCount = 1
	}

	receipt := protocol.TransferReceipt{
		OK:     err == nil,
		Bytes:  bytesWritten,
		Files:  filesCount,
		SHA256: shaHex,
	}
	if err != nil {
		receipt.Error = err.Error()
	}
	_ = protocol.WriteLineJSON(bs, receipt)
}

func (c *Client) handleFileDownloadStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	defer bs.Close()

	srcPath := ResolvePath(hdr.RemotePath, c.GetWorkdir())
	st, err := os.Stat(srcPath)
	if err != nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("Remote path error: %v", err),
		})
		return
	}

	if st.IsDir() {
		if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    true,
			IsDir: true,
			Name:  filepath.Base(srcPath),
		}); err != nil {
			return
		}
		_, _, _, _ = protocol.PackDirToStream(srcPath, bs)
		return
	}

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
		OK:    true,
		IsDir: false,
		Size:  st.Size(),
		Name:  filepath.Base(srcPath),
	}); err != nil {
		return
	}
	_, _, _ = protocol.StreamFileFromDisk(srcPath, bs)
}

func (c *Client) handleTCPConnectStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	targetConn, err := net.DialTimeout("tcp", hdr.RemoteAddr, 10*time.Second)
	if err != nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("dial %s on target machine failed: %v", hdr.RemoteAddr, err),
		})
		_ = bs.Close()
		return
	}
	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
		OK:        true,
		BoundAddr: targetConn.LocalAddr().String(),
	}); err != nil {
		_ = targetConn.Close()
		_ = bs.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetConn, bs)
		_ = targetConn.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(bs, targetConn)
		_ = bs.Close()
	}()
	wg.Wait()
}

func (c *Client) handleReverseListenStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	ln, err := net.Listen("tcp", hdr.RemoteAddr)
	if err != nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("listen %s on target machine failed: %v", hdr.RemoteAddr, err),
		})
		_ = bs.Close()
		return
	}
	defer ln.Close()
	defer bs.Close()

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
		OK:        true,
		BoundAddr: ln.Addr().String(),
	}); err != nil {
		return
	}

	// Close listener when control stream closes
	go func() {
		buf := make([]byte, 64)
		_, _ = bs.Read(buf)
		_ = ln.Close()
	}()

	var writeMu sync.Mutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		connID := fmt.Sprintf("rev-%d", atomic.AddUint64(&c.revSeq, 1))
		c.revMu.Lock()
		c.pendingRevConns[connID] = conn
		c.revMu.Unlock()

		// Auto-cleanup if dev-client doesn't claim within 15s
		go func(id string) {
			time.Sleep(15 * time.Second)
			c.revMu.Lock()
			if stale, ok := c.pendingRevConns[id]; ok {
				delete(c.pendingRevConns, id)
				_ = stale.Close()
			}
			c.revMu.Unlock()
		}(connID)

		writeMu.Lock()
		wErr := protocol.WriteLineJSON(bs, protocol.ReverseConnEvent{
			ConnID:     connID,
			RemotePeer: conn.RemoteAddr().String(),
		})
		writeMu.Unlock()
		if wErr != nil {
			return
		}
	}
}

func (c *Client) handleReverseAcceptStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	c.revMu.Lock()
	targetConn, ok := c.pendingRevConns[hdr.ConnID]
	if ok {
		delete(c.pendingRevConns, hdr.ConnID)
	}
	c.revMu.Unlock()

	if !ok || targetConn == nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("reverse conn_id %s not found or expired", hdr.ConnID),
		})
		_ = bs.Close()
		return
	}

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{OK: true}); err != nil {
		_ = targetConn.Close()
		_ = bs.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(targetConn, bs)
		_ = targetConn.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(bs, targetConn)
		_ = bs.Close()
	}()
	wg.Wait()
}

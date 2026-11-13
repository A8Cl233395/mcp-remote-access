package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/protocol"
)

// FatalError represents a non-unexpected connection failure or explicit remote rejection
// that should not trigger automatic reconnection.
type FatalError struct {
	Reason string
	Err    error
}

func (e *FatalError) Error() string {
	if e.Reason != "" && e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Reason, e.Err)
	}
	if e.Reason != "" {
		return e.Reason
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "fatal connection error"
}

func (e *FatalError) Unwrap() error {
	return e.Err
}

// IsFatalError reports whether err represents a non-unexpected disconnection or
// explicit rejection that should stop the client rather than auto-reconnect.
func IsFatalError(err error) bool {
	if err == nil {
		return false
	}
	var fe *FatalError
	return errors.As(err, &fe)
}

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

	revMu              sync.Mutex
	revSeq             uint64
	pendingRevConns    map[string]net.Conn
	pendingUDPRevConns map[string]*agentUDPSession
}

type agentUDPSession struct {
	connID    string
	peerAddr  net.Addr
	toDev     chan []byte
	writeTo   func([]byte) error
	closeFn   func()
	done      chan struct{}
	closeOnce sync.Once
}

func (s *agentUDPSession) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.closeFn != nil {
			s.closeFn()
		}
	})
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
		pendingRevConns:    make(map[string]net.Conn),
		pendingUDPRevConns: make(map[string]*agentUDPSession),
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
	for id, sess := range c.pendingUDPRevConns {
		delete(c.pendingUDPRevConns, id)
		sess.Close()
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
		Encoding:        SystemFileEncoding(),
		ConsoleEncoding: SystemConsoleEncoding(),
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

// Run connects to relay-server and automatically reconnects on unexpected disconnections until ctx is canceled.
// Non-unexpected disconnections (such as connection refused, invalid key, kicked offline, or server rejection)
// stop immediately and return a *FatalError without reconnecting.
func (c *Client) Run(ctx context.Context) error {
	CleanupOldBinary()
	defer c.wakeLock.Release()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if c.opts.AutoUpdate {
			info := c.GetMachineInfo()
			if updErr := SelfUpdateAndRestartWithState(RestartState{
				ServerAddr: c.opts.ServerAddr,
				UserKey:    c.GetUserKey(),
				MachineID:  info.MachineID,
				Shell:      info.Shell,
				Workdir:    info.Workdir,
				Insecure:   c.opts.Insecure,
			}, c.opts.OnStatus); updErr != nil {
				if fatalErr := classifyUpdateError(updErr); fatalErr != nil {
					c.setStatus(fmt.Sprintf("Error: %v", fatalErr))
					return fatalErr
				}
			}
		}

		attemptStart := time.Now()
		err := c.connectAndServe(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if IsFatalError(err) {
				c.setStatus(fmt.Sprintf("Error: %v", err))
				return err
			}
			if time.Since(attemptStart) > 15*time.Second {
				backoff = time.Second
			}
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
		return &FatalError{Reason: "invalid server address", Err: err}
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
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			ws = ws2
			resp = resp2
			err = nil
		} else if resp2 != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			resp = resp2
			err = err2
		}
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return classifyDialError(err, resp)
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
	defer func() {
		_ = session.Close()
		c.mu.Lock()
		if c.session == session {
			c.session = nil
		}
		c.mu.Unlock()
	}()

	// Send initial registration stream
	regStream, err := session.OpenStream()
	if err != nil {
		return err
	}
	info := c.GetMachineInfo()
	if err := protocol.WriteLineJSON(regStream, protocol.StreamHeader{
		StreamType: protocol.StreamTypeAgentRegister,
		Machine:    &info,
	}); err != nil {
		_ = regStream.Close()
		return err
	}
	bs := protocol.NewBufferedStream(regStream)
	var ack protocol.StreamAck
	if err := bs.ReadLineJSON(&ack); err != nil {
		_ = regStream.Close()
		if fatalErr := classifyWSCloseError(wsConn.RemoteCloseError()); fatalErr != nil {
			return fatalErr
		}
		return fmt.Errorf("registration handshake failed: %w", err)
	}
	if !ack.OK {
		_ = regStream.Close()
		return &FatalError{Reason: fmt.Sprintf("registration rejected: %s", ack.Error)}
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
			if fatalErr := classifyWSCloseError(wsConn.RemoteCloseError()); fatalErr != nil {
				return fatalErr
			}
			return err
		}
		go c.handleStream(stream)
	}
}

func classifyUpdateError(err error) error {
	if err == nil {
		return nil
	}
	if isConnectionRefusedError(err) {
		return &FatalError{Reason: "connection actively refused by remote server", Err: err}
	}
	if isTLSError(err) {
		return &FatalError{Reason: "TLS verification failed", Err: err}
	}
	msg := err.Error()
	if strings.Contains(msg, "status 401") {
		return &FatalError{Reason: "authentication failed (invalid user key)"}
	}
	if strings.Contains(msg, "status 403") {
		return &FatalError{Reason: "connection forbidden by server (HTTP 403)"}
	}
	return nil
}

func classifyDialError(err error, resp *http.Response) error {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return &FatalError{Reason: "authentication failed (invalid user key)"}
		case http.StatusForbidden:
			return &FatalError{Reason: "connection forbidden by server (HTTP 403)"}
		default:
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return &FatalError{Reason: fmt.Sprintf("connection rejected by server (HTTP %d)", resp.StatusCode), Err: err}
			}
		}
	}
	if isConnectionRefusedError(err) {
		return &FatalError{Reason: "connection actively refused by remote server", Err: err}
	}
	if isTLSError(err) {
		return &FatalError{Reason: "TLS verification failed", Err: err}
	}
	return err
}

func classifyWSCloseError(ce *websocket.CloseError) error {
	if ce == nil {
		return nil
	}
	reason := strings.TrimSpace(ce.Text)
	switch ce.Code {
	case protocol.CloseCodeKicked:
		if reason == "" {
			reason = "another client connected with the same machine ID"
		}
		return &FatalError{Reason: fmt.Sprintf("kicked offline: %s", reason)}
	case protocol.CloseCodeUnauthorized:
		if reason == "" {
			reason = "authentication failed or revoked"
		}
		return &FatalError{Reason: reason}
	case protocol.CloseCodeRejected:
		if reason == "" {
			reason = "connection rejected by server"
		}
		return &FatalError{Reason: reason}
	case websocket.CloseNormalClosure:
		if reason != "" {
			return &FatalError{Reason: fmt.Sprintf("connection closed by server: %s", reason)}
		}
		return &FatalError{Reason: "connection closed by server"}
	case websocket.ClosePolicyViolation:
		if reason != "" {
			return &FatalError{Reason: fmt.Sprintf("connection rejected by server policy: %s", reason)}
		}
		return &FatalError{Reason: "connection rejected by server policy"}
	default:
		if ce.Code >= 4000 && ce.Code <= 4999 {
			if reason != "" {
				return &FatalError{Reason: fmt.Sprintf("disconnected by server (code %d): %s", ce.Code, reason)}
			}
			return &FatalError{Reason: fmt.Sprintf("disconnected by server (code %d)", ce.Code)}
		}
		return nil
	}
}

func isConnectionRefusedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10061)) { // WSAECONNREFUSED
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "connection refused") || strings.Contains(lower, "actively refused")
}

func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	var certInvalid x509.CertificateInvalidError
	var unknownAuth x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var sysRootsErr x509.SystemRootsError
	var recHdrErr tls.RecordHeaderError
	var certVerifyErr *tls.CertificateVerificationError
	return errors.As(err, &certInvalid) ||
		errors.As(err, &unknownAuth) ||
		errors.As(err, &hostErr) ||
		errors.As(err, &sysRootsErr) ||
		errors.As(err, &recHdrErr) ||
		errors.As(err, &certVerifyErr)
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
	case protocol.StreamTypeUDPConnect:
		c.handleUDPConnectStream(bs, hdr)
	case protocol.StreamTypeUDPReverseListen:
		c.handleUDPReverseListenStream(bs, hdr)
	case protocol.StreamTypeUDPReverseAccept:
		c.handleUDPReverseAcceptStream(bs, hdr)
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
			Path     string `json:"path"`
			Offset   *int   `json:"offset,omitempty"`
			Limit    *int   `json:"limit,omitempty"`
			Encoding string `json:"encoding,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "read requires a `path` string"}
		}
		content, meta, err := ReadFileOrDirWithEncoding(p.Path, c.GetWorkdir(), p.Offset, p.Limit, p.Encoding)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "write":
		var p struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "write requires `path` and `content`"}
		}
		content, meta, err := WriteFileWithEncoding(p.Path, p.Content, c.GetWorkdir(), p.Encoding)
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
			Encoding   string `json:"encoding,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || strings.TrimSpace(p.Path) == "" {
			return protocol.RPCResponse{OK: false, Error: "edit requires `path`, `oldString`, and `newString`"}
		}
		content, meta, err := EditFileWithEncoding(p.Path, p.OldString, p.NewString, p.ReplaceAll, c.GetWorkdir(), p.Encoding)
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
			Encoding      string `json:"encoding,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return protocol.RPCResponse{OK: false, Error: "invalid grep arguments"}
		}
		caseSens := true
		if p.CaseSensitive != nil {
			caseSens = *p.CaseSensitive
		}
		content, meta, err := GrepFilesWithEncoding(c.GetWorkdir(), p.Pattern, p.Path, p.Include, p.Literal, caseSens, p.Limit, p.Encoding)
		if err != nil {
			return protocol.RPCResponse{OK: false, Error: err.Error()}
		}
		return protocol.RPCResponse{OK: true, Content: content, Metadata: meta}

	case "shell":
		var p struct {
			Command  string `json:"command"`
			Workdir  string `json:"workdir,omitempty"`
			Shell    string `json:"shell,omitempty"`
			Timeout  *int   `json:"timeout,omitempty"`
			Encoding string `json:"encoding,omitempty"`
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
		content, meta, err := c.jobs.ExecuteShellWithEncoding(p.Command, cwd, shellPath, p.Timeout, p.Encoding, c.pushStateUpdate)
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

	protocol.RelayBidirectional(targetConn, bs)
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

	protocol.RelayBidirectional(targetConn, bs)
}

func (c *Client) handleUDPConnectStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	defer bs.Close()

	uconn, err := net.Dial("udp", hdr.RemoteAddr)
	if err != nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("dial udp %s on target machine failed: %v", hdr.RemoteAddr, err),
		})
		return
	}
	defer uconn.Close()

	bound := ""
	if la := uconn.LocalAddr(); la != nil {
		bound = la.String()
	}

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
		OK:        true,
		BoundAddr: bound,
	}); err != nil {
		return
	}

	protocol.RelayPacketConn(bs, uconn)
}

func (c *Client) handleUDPReverseListenStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	ln, err := net.ListenPacket("udp", hdr.RemoteAddr)
	if err != nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("listen udp %s on target machine failed: %v", hdr.RemoteAddr, err),
		})
		_ = bs.Close()
		return
	}
	defer ln.Close()
	defer bs.Close()

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{
		OK:        true,
		BoundAddr: ln.LocalAddr().String(),
	}); err != nil {
		return
	}

	// Close listener when control stream closes
	go func() {
		buf := make([]byte, 64)
		_, _ = bs.Read(buf)
		_ = ln.Close()
	}()

	var sessMu sync.Mutex
	sessions := make(map[string]*agentUDPSession)
	var writeMu sync.Mutex

	defer func() {
		sessMu.Lock()
		for _, s := range sessions {
			s.Close()
		}
		sessions = nil
		sessMu.Unlock()
	}()

	buf := make([]byte, protocol.MaxUDPPacketSize)
	for {
		n, peerAddr, err := ln.ReadFrom(buf)
		if err != nil {
			return
		}
		peerKey := peerAddr.String()

		sessMu.Lock()
		sess, ok := sessions[peerKey]
		if !ok {
			connID := fmt.Sprintf("rev-udp-%d", atomic.AddUint64(&c.revSeq, 1))
			sess = &agentUDPSession{
				connID:   connID,
				peerAddr: peerAddr,
				toDev:    make(chan []byte, 128),
				done:     make(chan struct{}),
				writeTo: func(p []byte) error {
					_, wErr := ln.WriteTo(p, peerAddr)
					return wErr
				},
				closeFn: func() {
					sessMu.Lock()
					delete(sessions, peerKey)
					sessMu.Unlock()
					c.revMu.Lock()
					delete(c.pendingUDPRevConns, connID)
					c.revMu.Unlock()
				},
			}
			sessions[peerKey] = sess

			c.revMu.Lock()
			c.pendingUDPRevConns[connID] = sess
			c.revMu.Unlock()

			// Auto-cleanup if not claimed in 15s
			go func(s *agentUDPSession, id string) {
				select {
				case <-time.After(15 * time.Second):
					c.revMu.Lock()
					if _, pending := c.pendingUDPRevConns[id]; pending {
						s.Close()
					}
					c.revMu.Unlock()
				case <-s.done:
				}
			}(sess, connID)

			writeMu.Lock()
			wErr := protocol.WriteLineJSON(bs, protocol.ReverseConnEvent{
				ConnID:     connID,
				RemotePeer: peerKey,
			})
			writeMu.Unlock()
			if wErr != nil {
				sessMu.Unlock()
				return
			}
		}

		pktCopy := make([]byte, n)
		copy(pktCopy, buf[:n])
		select {
		case sess.toDev <- pktCopy:
		default:
		}
		sessMu.Unlock()
	}
}

func (c *Client) handleUDPReverseAcceptStream(bs *protocol.BufferedStream, hdr protocol.StreamHeader) {
	c.revMu.Lock()
	sess, ok := c.pendingUDPRevConns[hdr.ConnID]
	if ok {
		delete(c.pendingUDPRevConns, hdr.ConnID)
	}
	c.revMu.Unlock()

	if !ok || sess == nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("reverse udp conn_id %s not found or expired", hdr.ConnID),
		})
		_ = bs.Close()
		return
	}
	defer bs.Close()
	defer sess.Close()

	if err := protocol.WriteLineJSON(bs, protocol.StreamAck{OK: true}); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-sess.done:
				_ = bs.Close()
				return
			case pkt, open := <-sess.toDev:
				if !open {
					_ = bs.Close()
					return
				}
				if err := protocol.WritePacket(bs, pkt); err != nil {
					_ = bs.Close()
					return
				}
			}
		}
	}()

	go func() {
		defer wg.Done()
		buf := make([]byte, protocol.MaxUDPPacketSize)
		for {
			n, err := protocol.ReadPacket(bs, buf)
			if err != nil {
				break
			}
			if err := sess.writeTo(buf[:n]); err != nil {
				break
			}
		}
		sess.Close()
		_ = bs.Close()
	}()

	wg.Wait()
}

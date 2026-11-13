package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/protocol"
)

type Config struct {
	ListenAddr  string
	UserKey     string
	DevKey      string
	ReleasesDir string
	EnableMCP   bool
	Logger      *log.Logger
}

type AgentSession struct {
	Info        protocol.MachineInfo
	Session     *yamux.Session
	WSConn      *protocol.WSConn
	ConnectedAt time.Time
	mu          sync.RWMutex
}

func (a *AgentSession) Kick(code int, reason string) {
	if a.WSConn != nil {
		_ = a.WSConn.CloseWithCode(code, reason)
	}
	if a.Session != nil {
		_ = a.Session.Close()
	}
}

func (a *AgentSession) GetInfo() protocol.MachineInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Info
}

func (a *AgentSession) UpdateInfo(info protocol.MachineInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if info.ConnectedAt == 0 {
		info.ConnectedAt = a.ConnectedAt.Unix()
	}
	a.Info = info
}

type cachedManifest struct {
	modTime int64
	size    int64
	sha256  string
}

type Server struct {
	cfg           Config
	upgrader      websocket.Upgrader
	mu            sync.RWMutex
	agents        map[string]*AgentSession
	manifestMu    sync.Mutex
	manifestCache map[string]cachedManifest

	mcpMu       sync.Mutex
	mcpSessions map[string]*mcpSession
}

func NewServer(cfg Config) *Server {
	if cfg.ReleasesDir == "" {
		cfg.ReleasesDir = "./releases"
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "[relay-server] ", log.LstdFlags)
	}
	return &Server{
		cfg: cfg,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  64 * 1024,
			WriteBufferSize: 64 * 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		agents:        make(map[string]*AgentSession),
		manifestCache: make(map[string]cachedManifest),
		mcpSessions:   make(map[string]*mcpSession),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/update/manifest", s.handleUpdateManifest)
	mux.HandleFunc("/update/binary", s.handleUpdateBinary)
	mux.HandleFunc("/ws/user", s.handleWSUser)
	mux.HandleFunc("/ws/dev", s.handleWSDev)

	if s.cfg.EnableMCP {
		// Server-side MCP endpoints (Streamable HTTP + Legacy HTTP+SSE)
		mux.HandleFunc("/mcp", s.withCORS(s.handleMCP))
		mux.HandleFunc("/sse", s.withCORS(s.handleMCPSSE))
		mux.HandleFunc("/messages", s.withCORS(s.handleMCPMessages))

		// Minimal OAuth 2.0 shim for MCP clients (validates client_secret against DevKey)
		mux.HandleFunc("/.well-known/oauth-protected-resource", s.withCORS(s.handleOAuthProtectedResource))
		mux.HandleFunc("/.well-known/oauth-protected-resource/", s.withCORS(s.handleOAuthProtectedResource))
		mux.HandleFunc("/.well-known/oauth-authorization-server", s.withCORS(s.handleOAuthAuthorizationServer))
		mux.HandleFunc("/.well-known/oauth-authorization-server/", s.withCORS(s.handleOAuthAuthorizationServer))
		mux.HandleFunc("/.well-known/openid-configuration", s.withCORS(s.handleOAuthAuthorizationServer))
		mux.HandleFunc("/.well-known/openid-configuration/", s.withCORS(s.handleOAuthAuthorizationServer))
		mux.HandleFunc("/oauth/authorize", s.withCORS(s.handleOAuthAuthorize))
		mux.HandleFunc("/oauth/token", s.withCORS(s.handleOAuthToken))
	}
	return mux
}

func (s *Server) checkUserAuth(r *http.Request) bool {
	key := r.Header.Get(protocol.HeaderUserKey)
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	return s.cfg.UserKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.UserKey)) == 1
}

func (s *Server) checkDevAuth(r *http.Request) bool {
	key := r.Header.Get(protocol.HeaderDevKey)
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	return s.cfg.DevKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.DevKey)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) resolveReleaseBinary(goos, goarch string) (string, string, error) {
	if goos == "" || goarch == "" {
		return "", "", fmt.Errorf("missing os or arch")
	}
	if goarch == "i386" || goarch == "x86" {
		goarch = "386"
	}
	if goos == "windows7" || goos == "win7" {
		goos = "windows"
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	candidates := []string{
		fmt.Sprintf("user-client-%s-%s%s", goos, goarch, ext),
		fmt.Sprintf("user-client-%s%s", goos, ext),
		fmt.Sprintf("user-client%s", ext),
	}
	for _, name := range candidates {
		full := filepath.Join(s.cfg.ReleasesDir, name)
		st, err := os.Stat(full)
		if err == nil && !st.IsDir() && st.Size() > 0 {
			return full, name, nil
		}
	}
	return "", "", os.ErrNotExist
}

func (s *Server) handleUpdateManifest(w http.ResponseWriter, r *http.Request) {
	if !s.checkUserAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	goos := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("os")))
	goarch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))

	w.Header().Set("Content-Type", "application/json")
	fullPath, filename, err := s.resolveReleaseBinary(goos, goarch)
	if err != nil {
		_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{Available: false})
		return
	}

	st, err := os.Stat(fullPath)
	if err != nil {
		_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{Available: false})
		return
	}
	modNano := st.ModTime().UnixNano()
	fileSize := st.Size()

	s.manifestMu.Lock()
	if cached, ok := s.manifestCache[fullPath]; ok && cached.modTime == modNano && cached.size == fileSize {
		s.manifestMu.Unlock()
		_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{
			Available: true,
			Filename:  filename,
			SHA256:    cached.sha256,
			Size:      cached.size,
		})
		return
	}
	s.manifestMu.Unlock()

	f, err := os.Open(fullPath)
	if err != nil {
		_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{Available: false})
		return
	}
	defer f.Close()

	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{Available: false})
		return
	}
	shaHex := hex.EncodeToString(hasher.Sum(nil))

	s.manifestMu.Lock()
	s.manifestCache[fullPath] = cachedManifest{
		modTime: modNano,
		size:    size,
		sha256:  shaHex,
	}
	s.manifestMu.Unlock()

	_ = json.NewEncoder(w).Encode(protocol.UpdateManifest{
		Available: true,
		Filename:  filename,
		SHA256:    shaHex,
		Size:      size,
	})
}

func (s *Server) handleUpdateBinary(w http.ResponseWriter, r *http.Request) {
	if !s.checkUserAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	goos := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("os")))
	goarch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))

	fullPath, filename, err := s.resolveReleaseBinary(goos, goarch)
	if err != nil {
		http.Error(w, "release binary not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	http.ServeFile(w, r, fullPath)
}

func (s *Server) handleWSUser(w http.ResponseWriter, r *http.Request) {
	if !s.checkUserAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.cfg.Logger.Printf("upgrade user ws failed: %v", err)
		return
	}

	wsConn := protocol.NewWSConn(ws)
	session, err := yamux.Client(wsConn, protocol.DefaultYamuxConfig())
	if err != nil {
		_ = wsConn.Close()
		s.cfg.Logger.Printf("yamux client init failed: %v", err)
		return
	}

	// Wait for the agent's initial registration stream
	regStream, err := session.AcceptStream()
	if err != nil {
		_ = session.Close()
		return
	}
	bs := protocol.NewBufferedStream(regStream)
	var hdr protocol.StreamHeader
	if err := bs.ReadLineJSON(&hdr); err != nil || hdr.StreamType != protocol.StreamTypeAgentRegister || hdr.Machine == nil {
		_ = protocol.WriteLineJSON(bs, protocol.StreamAck{OK: false, Error: "expected agent_register header with machine info"})
		_ = regStream.Close()
		_ = wsConn.CloseWithCode(protocol.CloseCodeRejected, "expected agent_register header with machine info")
		_ = session.Close()
		return
	}

	info := *hdr.Machine
	if info.MachineID == "" {
		info.MachineID = fmt.Sprintf("%s-%d", info.Hostname, time.Now().UnixNano()%10000)
	}
	now := time.Now()
	info.ConnectedAt = now.Unix()

	agent := &AgentSession{
		Info:        info,
		Session:     session,
		WSConn:      wsConn,
		ConnectedAt: now,
	}

	var kicked *AgentSession
	s.mu.Lock()
	if old, exists := s.agents[info.MachineID]; exists {
		kicked = old
	}
	s.agents[info.MachineID] = agent
	s.mu.Unlock()

	if kicked != nil {
		s.cfg.Logger.Printf("kicking previous user-client session for id=%s (replaced by new connection)", info.MachineID)
		go kicked.Kick(protocol.CloseCodeKicked, "another client connected with the same machine ID")
	}

	_ = protocol.WriteLineJSON(bs, protocol.StreamAck{OK: true})
	_ = regStream.Close()

	s.cfg.Logger.Printf("user-client connected: id=%s host=%s os=%s/%s shell=%s", info.MachineID, info.Hostname, info.OS, info.Arch, info.Shell)

	defer func() {
		s.mu.Lock()
		if cur, ok := s.agents[info.MachineID]; ok && cur == agent {
			delete(s.agents, info.MachineID)
		}
		s.mu.Unlock()
		_ = session.Close()
		s.cfg.Logger.Printf("user-client disconnected: id=%s", info.MachineID)
	}()

	// Accept subsequent state-update streams from user-client until session closes
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go func(st io.ReadWriteCloser) {
			defer st.Close()
			bst := protocol.NewBufferedStream(st)
			var h protocol.StreamHeader
			if err := bst.ReadLineJSON(&h); err != nil {
				return
			}
			if h.StreamType == protocol.StreamTypeAgentRegister && h.Machine != nil {
				agent.UpdateInfo(*h.Machine)
				_ = protocol.WriteLineJSON(bst, protocol.StreamAck{OK: true})
			}
		}(stream)
	}
}

func (s *Server) handleWSDev(w http.ResponseWriter, r *http.Request) {
	if !s.checkDevAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.cfg.Logger.Printf("upgrade dev ws failed: %v", err)
		return
	}

	wsConn := protocol.NewWSConn(ws)
	session, err := yamux.Server(wsConn, protocol.DefaultYamuxConfig())
	if err != nil {
		_ = wsConn.Close()
		return
	}
	defer session.Close()

	s.cfg.Logger.Printf("dev-client connected from %s", r.RemoteAddr)
	defer s.cfg.Logger.Printf("dev-client disconnected from %s", r.RemoteAddr)

	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go s.handleDevStream(stream)
	}
}

func (s *Server) listMachines() []protocol.MachineInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]protocol.MachineInfo, 0, len(s.agents))
	for _, ag := range s.agents {
		if ag.Session.IsClosed() {
			continue
		}
		list = append(list, ag.GetInfo())
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].MachineID < list[j].MachineID
	})
	return list
}

func (s *Server) findAgent(target string) (*AgentSession, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Filter active agents
	var active []*AgentSession
	for _, ag := range s.agents {
		if !ag.Session.IsClosed() {
			active = append(active, ag)
		}
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("no user-client machines are currently connected to relay-server")
	}
	if target == "" {
		if len(active) == 1 {
			return active[0], nil
		}
		// Pick deterministically by MachineID or ask caller to specify
		sort.Slice(active, func(i, j int) bool {
			return active[i].GetInfo().MachineID < active[j].GetInfo().MachineID
		})
		return active[0], nil
	}

	if ag, ok := s.agents[target]; ok && !ag.Session.IsClosed() {
		return ag, nil
	}

	// Fallback: match by hostname or prefix (case-insensitive)
	targetLower := strings.ToLower(target)
	var matches []*AgentSession
	for _, ag := range active {
		info := ag.GetInfo()
		if strings.EqualFold(info.Hostname, target) ||
			strings.HasPrefix(strings.ToLower(info.MachineID), targetLower) {
			matches = append(matches, ag)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("ambiguous machine target %q matched %d machines; run list_machines to see exact IDs", target, len(matches))
	}
	return nil, fmt.Errorf("machine %q is not connected; run list_machines to see online machines", target)
}

func (s *Server) handleDevStream(devRaw io.ReadWriteCloser) {
	devStream := protocol.NewBufferedStream(devRaw)
	var hdr protocol.StreamHeader
	if err := devStream.ReadLineJSON(&hdr); err != nil {
		_ = devStream.Close()
		return
	}

	if hdr.StreamType == protocol.StreamTypeListMachines {
		machines := s.listMachines()
		data, _ := json.Marshal(machines)
		_ = protocol.WriteLineJSON(devStream, protocol.StreamAck{
			OK:   true,
			Data: data,
		})
		_ = devStream.Close()
		return
	}

	agent, err := s.findAgent(hdr.TargetMachineID)
	if err != nil {
		_ = protocol.WriteLineJSON(devStream, protocol.StreamAck{
			OK:    false,
			Error: err.Error(),
		})
		_ = devStream.Close()
		return
	}

	agentRaw, err := agent.Session.OpenStream()
	if err != nil {
		_ = protocol.WriteLineJSON(devStream, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("open stream to target machine %s failed: %v", agent.GetInfo().MachineID, err),
		})
		_ = devStream.Close()
		return
	}

	hdr.TargetMachineID = agent.GetInfo().MachineID
	if err := protocol.WriteLineJSON(agentRaw, hdr); err != nil {
		_ = agentRaw.Close()
		_ = protocol.WriteLineJSON(devStream, protocol.StreamAck{
			OK:    false,
			Error: fmt.Sprintf("send header to target machine failed: %v", err),
		})
		_ = devStream.Close()
		return
	}

	// Bridge devStream and agentRaw bidirectionally
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(agentRaw, devStream)
		_ = agentRaw.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(devStream, agentRaw)
		_ = devStream.Close()
	}()
	wg.Wait()
}

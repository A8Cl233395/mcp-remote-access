package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"mcp-remoteaccess/internal/dev"
	"mcp-remoteaccess/internal/protocol"
)

const (
	mcpMaxBodyBytes    = 32 * 1024 * 1024
	mcpSessionIdleTTL  = 2 * time.Hour
	mcpMaxSessions     = 1024
	mcpKeepalivePeriod = 15 * time.Second
)

type mcpSession struct {
	id              string
	mu              sync.Mutex
	activeMachineID string
	lastSeen        time.Time
	mcpSrv          *dev.MCPServer

	sseCh     chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (ms *mcpSession) close() {
	ms.closeOnce.Do(func() {
		if ms.done != nil {
			close(ms.done)
		}
	})
}

func (ms *mcpSession) getActiveMachineID() string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.activeMachineID
}

func (ms *mcpSession) setActiveMachineID(id string) {
	ms.mu.Lock()
	ms.activeMachineID = id
	ms.mu.Unlock()
}

func (ms *mcpSession) touch() {
	ms.mu.Lock()
	ms.lastSeen = time.Now()
	ms.mu.Unlock()
}

type relayMCPBackend struct {
	srv  *Server
	sess *mcpSession
}

func (b *relayMCPBackend) ListMachinesFormatted() (string, map[string]any, error) {
	machines := b.srv.listMachines()
	if b.sess.getActiveMachineID() == "" && len(machines) > 0 {
		b.sess.setActiveMachineID(machines[0].MachineID)
	}
	text, meta := dev.FormatMachinesList(machines, b.sess.getActiveMachineID())
	return text, meta, nil
}

func (b *relayMCPBackend) SwitchMachine(targetID string) (string, map[string]any, error) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return b.ListMachinesFormatted()
	}
	resp, err := b.callRPCOnMachine(targetID, "machine_info", nil)
	if err != nil {
		return "", nil, err
	}
	if !resp.OK {
		return "", nil, fmt.Errorf("%s", resp.Error)
	}
	if resp.Machine == nil {
		return "", nil, fmt.Errorf("target machine did not return machine info")
	}
	m := *resp.Machine
	b.sess.setActiveMachineID(m.MachineID)
	text, meta := dev.FormatSwitchMachine(m)
	return text, meta, nil
}

func (b *relayMCPBackend) CallRPC(method string, params any) (*protocol.RPCResponse, error) {
	targetID := b.sess.getActiveMachineID()
	if pm, ok := params.(map[string]any); ok {
		if override, ok := pm["machine_id"].(string); ok && strings.TrimSpace(override) != "" {
			targetID = strings.TrimSpace(override)
		}
	}
	return b.callRPCOnMachine(targetID, method, params)
}

func (b *relayMCPBackend) callRPCOnMachine(targetMachineID, method string, params any) (*protocol.RPCResponse, error) {
	agent, err := b.srv.findAgent(targetMachineID)
	if err != nil {
		return nil, err
	}
	agentRaw, err := agent.Session.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open stream to target machine %s failed: %w", agent.GetInfo().MachineID, err)
	}
	defer agentRaw.Close()

	if err := protocol.WriteLineJSON(agentRaw, protocol.StreamHeader{
		StreamType:      protocol.StreamTypeRPC,
		TargetMachineID: agent.GetInfo().MachineID,
	}); err != nil {
		return nil, fmt.Errorf("write stream header failed: %w", err)
	}

	resp, err := protocol.SendRPC(agentRaw, method, params)
	if err != nil {
		return nil, err
	}
	if resp.Machine != nil && b.sess.getActiveMachineID() == "" {
		b.sess.setActiveMachineID(resp.Machine.MachineID)
	}
	return resp, nil
}

func (s *Server) newMCPSession(id string, withSSE bool) *mcpSession {
	ms := &mcpSession{
		id:       id,
		lastSeen: time.Now(),
	}
	if withSSE {
		ms.sseCh = make(chan []byte, 64)
		ms.done = make(chan struct{})
	}
	backend := &relayMCPBackend{srv: s, sess: ms}
	ms.mcpSrv = dev.NewMCPServerWithBackend(backend, dev.MCPServerOptions{
		DisableLocalTools: true,
	})
	return ms
}

func (s *Server) getOrCreateMCPSession(id string, create bool) *mcpSession {
	now := time.Now()
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()

	if ms, ok := s.mcpSessions[id]; ok {
		ms.mu.Lock()
		ms.lastSeen = now
		ms.mu.Unlock()
		return ms
	}
	if !create {
		return nil
	}
	// Evict idle non-SSE sessions if table grows large
	if len(s.mcpSessions) >= mcpMaxSessions {
		for k, v := range s.mcpSessions {
			v.mu.Lock()
			idle := v.sseCh == nil && now.Sub(v.lastSeen) > mcpSessionIdleTTL
			v.mu.Unlock()
			if idle {
				delete(s.mcpSessions, k)
			}
		}
	}
	ms := s.newMCPSession(id, false)
	s.mcpSessions[id] = ms
	return ms
}

func (s *Server) deleteMCPSession(id string) {
	if id == "" {
		return
	}
	s.mcpMu.Lock()
	ms, ok := s.mcpSessions[id]
	if ok {
		delete(s.mcpSessions, id)
	}
	s.mcpMu.Unlock()
	if ok && ms != nil {
		ms.close()
	}
}

func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Dev-Key, Mcp-Session-Id, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func splitJSONRPCMessages(body []byte) (msgs []json.RawMessage, isBatch bool, err error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false, fmt.Errorf("empty request body")
	}
	if trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, true, err
		}
		return batch, true, nil
	}
	return []json.RawMessage{trimmed}, false, nil
}

func isInitializeRequest(raw json.RawMessage) bool {
	var hdr struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return false
	}
	return hdr.Method == "initialize"
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.checkMCPAuth(r) {
		s.writeMCPUnauthorized(w, r)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		sessID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		s.deleteMCPSession(sessID)
		w.WriteHeader(http.StatusNoContent)
		return

	case http.MethodGet:
		if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
			http.Error(w, "Accept: text/event-stream required for GET /mcp", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, ": ok\n\n")
		flusher.Flush()

		ticker := time.NewTicker(mcpKeepalivePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
			return
		}
		msgs, isBatch, err := splitJSONRPCMessages(body)
		if err != nil {
			errResp, _ := (&dev.MCPServer{}).HandleJSONRPC([]byte("{invalid"))
			writeJSON(w, http.StatusBadRequest, errResp)
			return
		}
		if isBatch && len(msgs) == 0 {
			writeJSON(w, http.StatusBadRequest, dev.JSONRPCMessage{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &dev.JSONRPCError{Code: -32600, Message: "empty batch"},
			})
			return
		}

		sessID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
		if sessID == "" {
			if len(msgs) > 0 && isInitializeRequest(msgs[0]) {
				sessID = randomHex(16)
				w.Header().Set("Mcp-Session-Id", sessID)
			} else {
				sessID = "default"
			}
		} else {
			w.Header().Set("Mcp-Session-Id", sessID)
		}
		sess := s.getOrCreateMCPSession(sessID, true)
		sess.touch()

		responses := make([]*dev.JSONRPCMessage, 0, len(msgs))
		for _, raw := range msgs {
			resp, _ := sess.mcpSrv.HandleJSONRPC(raw)
			if resp != nil {
				responses = append(responses, resp)
			}
		}

		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var outPayload []byte
		if isBatch {
			outPayload, err = json.Marshal(responses)
		} else {
			outPayload, err = json.Marshal(responses[0])
		}
		if err != nil {
			http.Error(w, "marshal response failed", http.StatusInternalServerError)
			return
		}

		accept := strings.ToLower(r.Header.Get("Accept"))
		if strings.Contains(accept, "text/event-stream") && !strings.Contains(accept, "application/json") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", outPayload)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(outPayload)
		_, _ = w.Write([]byte{'\n'})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMCPSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkMCPAuth(r) {
		s.writeMCPUnauthorized(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sessID := randomHex(16)
	sess := s.newMCPSession(sessID, true)

	s.mcpMu.Lock()
	s.mcpSessions[sessID] = sess
	s.mcpMu.Unlock()

	defer func() {
		s.deleteMCPSession(sessID)
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessID)
	flusher.Flush()

	ticker := time.NewTicker(mcpKeepalivePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.done:
			return
		case payload := <-sess.sseCh:
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleMCPMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	var sess *mcpSession
	if sessID != "" {
		sess = s.getOrCreateMCPSession(sessID, false)
	}
	if sess == nil || sess.sseCh == nil {
		if !s.checkMCPAuth(r) {
			s.writeMCPUnauthorized(w, r)
			return
		}
		http.Error(w, "unknown or expired SSE sessionId", http.StatusNotFound)
		return
	}
	sess.touch()

	r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body failed: %v", err), http.StatusBadRequest)
		return
	}
	msgs, isBatch, err := splitJSONRPCMessages(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	responses := make([]*dev.JSONRPCMessage, 0, len(msgs))
	for _, raw := range msgs {
		resp, _ := sess.mcpSrv.HandleJSONRPC(raw)
		if resp != nil {
			responses = append(responses, resp)
		}
	}
	if len(responses) > 0 {
		var outPayload []byte
		if isBatch {
			outPayload, err = json.Marshal(responses)
		} else {
			outPayload, err = json.Marshal(responses[0])
		}
		if err != nil {
			http.Error(w, "marshal response failed", http.StatusInternalServerError)
			return
		}
		select {
		case sess.sseCh <- outPayload:
		case <-sess.done:
			http.Error(w, "SSE session closed", http.StatusGone)
			return
		case <-time.After(5 * time.Second):
			http.Error(w, "SSE client backpressure timeout", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

package dev

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"

	"mcp-remoteaccess/internal/agent"
	"mcp-remoteaccess/internal/protocol"
)

type ForwardEntry struct {
	ID         string
	Direction  string // "remote_to_local" | "local_to_remote"
	MachineID  string
	LocalAddr  string
	RemoteAddr string
	CreatedAt  time.Time
	ActiveConns int64
	closeFn    func()
}

type SessionConfig struct {
	ServerAddr     string
	DevKey         string
	DefaultMachine string
	Insecure       bool
	Logger         *log.Logger
}

type Session struct {
	cfg             SessionConfig
	mu              sync.Mutex
	yamuxSess       *yamux.Session
	activeMachineID string

	fwdMu    sync.Mutex
	fwdSeq   uint64
	forwards map[string]*ForwardEntry
}

func NewSession(cfg SessionConfig) *Session {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "[dev-client] ", log.LstdFlags)
	}
	return &Session{
		cfg:             cfg,
		activeMachineID: cfg.DefaultMachine,
		forwards:        make(map[string]*ForwardEntry),
	}
}

func (s *Session) Close() {
	s.fwdMu.Lock()
	for id, f := range s.forwards {
		if f.closeFn != nil {
			f.closeFn()
		}
		delete(s.forwards, id)
	}
	s.fwdMu.Unlock()

	s.mu.Lock()
	if s.yamuxSess != nil {
		_ = s.yamuxSess.Close()
		s.yamuxSess = nil
	}
	s.mu.Unlock()
}

func (s *Session) getOrConnect() (*yamux.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.yamuxSess != nil && !s.yamuxSess.IsClosed() {
		return s.yamuxSess, nil
	}

	wsURL, err := agent.NormalizeWSURLWithInsecure(s.cfg.ServerAddr, "/ws/dev", s.cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("invalid relay server address %q: %w", s.cfg.ServerAddr, err)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  agent.BuildClientTLSConfig(s.cfg.Insecure),
	}
	headers := http.Header{}
	headers.Set(protocol.HeaderDevKey, s.cfg.DevKey)

	ws, resp, err := dialer.Dial(wsURL, headers)
	if err != nil && s.cfg.Insecure && strings.HasPrefix(wsURL, "wss://") {
		plainURL := "ws://" + strings.TrimPrefix(wsURL, "wss://")
		if ws2, resp2, err2 := dialer.Dial(plainURL, headers); err2 == nil {
			ws = ws2
			resp = resp2
			err = nil
		}
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("relay authentication failed: invalid dev key")
		}
		return nil, fmt.Errorf("connect to relay server %s failed: %w", wsURL, err)
	}

	wsConn := protocol.NewWSConn(ws)
	sess, err := yamux.Client(wsConn, protocol.DefaultYamuxConfig())
	if err != nil {
		_ = wsConn.Close()
		return nil, fmt.Errorf("yamux client init failed: %w", err)
	}
	s.yamuxSess = sess
	return sess, nil
}

func (s *Session) openStream() (*protocol.BufferedStream, error) {
	sess, err := s.getOrConnect()
	if err != nil {
		return nil, err
	}
	st, err := sess.OpenStream()
	if err != nil {
		// Retry once on stale session
		s.mu.Lock()
		_ = sess.Close()
		s.yamuxSess = nil
		s.mu.Unlock()

		sess, err = s.getOrConnect()
		if err != nil {
			return nil, err
		}
		st, err = sess.OpenStream()
		if err != nil {
			return nil, err
		}
	}
	return protocol.NewBufferedStream(st), nil
}

func (s *Session) GetActiveMachineID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeMachineID
}

func (s *Session) SetActiveMachineID(id string) {
	s.mu.Lock()
	s.activeMachineID = id
	s.mu.Unlock()
}

func (s *Session) FetchMachines() ([]protocol.MachineInfo, error) {
	bs, err := s.openStream()
	if err != nil {
		return nil, err
	}
	defer bs.Close()

	if err := protocol.WriteLineJSON(bs, protocol.StreamHeader{
		StreamType: protocol.StreamTypeListMachines,
	}); err != nil {
		return nil, err
	}

	var ack protocol.StreamAck
	if err := bs.ReadLineJSON(&ack); err != nil {
		return nil, err
	}
	if !ack.OK {
		return nil, fmt.Errorf("%s", ack.Error)
	}

	var machines []protocol.MachineInfo
	if len(ack.Data) > 0 {
		if err := json.Unmarshal(ack.Data, &machines); err != nil {
			return nil, err
		}
	}

	if s.GetActiveMachineID() == "" && len(machines) > 0 {
		s.SetActiveMachineID(machines[0].MachineID)
	}
	return machines, nil
}

func (s *Session) ListMachinesFormatted() (string, map[string]any, error) {
	machines, err := s.FetchMachines()
	if err != nil {
		return "", nil, err
	}
	if len(machines) == 0 {
		return "No target machines are currently connected to the relay server.", map[string]any{"count": 0}, nil
	}

	activeID := s.GetActiveMachineID()
	var lines []string
	lines = append(lines, fmt.Sprintf("Connected machines (%d):", len(machines)))
	for _, m := range machines {
		marker := " "
		if m.MachineID == activeID {
			marker = "*"
		}
		adminTag := ""
		if m.IsAdmin {
			adminTag = " [admin/root]"
		}
		lines = append(lines, fmt.Sprintf(
			" %s %s  (%s/%s)  host=%s  user=%s%s  shell=%s (avail: %s)  workdir=%s  jobs=%d",
			marker,
			m.MachineID,
			m.OS,
			m.Arch,
			m.Hostname,
			m.User,
			adminTag,
			m.Shell,
			strings.Join(m.AvailableShells, ","),
			m.Workdir,
			m.RunningJobs,
		))
	}
	return strings.Join(lines, "\n"), map[string]any{
		"count":          len(machines),
		"active_machine": activeID,
		"machines":       machines,
	}, nil
}

func (s *Session) SwitchMachine(targetID string) (string, map[string]any, error) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return s.ListMachinesFormatted()
	}

	resp, err := s.CallRPCOnMachine(targetID, "machine_info", nil)
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
	s.SetActiveMachineID(m.MachineID)

	adminStr := "no"
	if m.IsAdmin {
		adminStr = "yes"
	}
	summary := strings.Join([]string{
		fmt.Sprintf("Switched active machine to: %s", m.MachineID),
		fmt.Sprintf("  Hostname:         %s", m.Hostname),
		fmt.Sprintf("  OS / Arch:        %s / %s", m.OS, m.Arch),
		fmt.Sprintf("  User:             %s (admin/root: %s)", m.User, adminStr),
		fmt.Sprintf("  Workdir:          %s", m.Workdir),
		fmt.Sprintf("  Current Shell:    %s", m.Shell),
		fmt.Sprintf("  Available Shells: %s", strings.Join(m.AvailableShells, ", ")),
		fmt.Sprintf("  Running Jobs:     %d", m.RunningJobs),
	}, "\n")

	return summary, map[string]any{
		"machine": m,
	}, nil
}

func (s *Session) CallRPC(method string, params any) (*protocol.RPCResponse, error) {
	return s.CallRPCOnMachine(s.GetActiveMachineID(), method, params)
}

func (s *Session) CallRPCOnMachine(targetMachineID, method string, params any) (*protocol.RPCResponse, error) {
	bs, err := s.openStream()
	if err != nil {
		return nil, err
	}
	defer bs.Close()

	if err := protocol.WriteLineJSON(bs, protocol.StreamHeader{
		StreamType:      protocol.StreamTypeRPC,
		TargetMachineID: targetMachineID,
	}); err != nil {
		return nil, err
	}

	resp, err := protocol.SendRPC(bs, method, params)
	if err != nil {
		return nil, err
	}
	if resp.Machine != nil && s.GetActiveMachineID() == "" {
		s.SetActiveMachineID(resp.Machine.MachineID)
	}
	return resp, nil
}

func (s *Session) Upload(localPath, remotePath string) (string, map[string]any, error) {
	start := time.Now()
	absLocal := agent.ExpandHome(localPath)
	if !filepath.IsAbs(absLocal) {
		wd, _ := os.Getwd()
		absLocal = filepath.Join(wd, absLocal)
	}
	absLocal = filepath.Clean(absLocal)

	st, err := os.Stat(absLocal)
	if err != nil {
		return "", nil, fmt.Errorf("Local path error (%s): %v", absLocal, err)
	}

	bs, err := s.openStream()
	if err != nil {
		return "", nil, err
	}
	defer bs.Close()

	hdr := protocol.StreamHeader{
		StreamType:      protocol.StreamTypeFileUpload,
		TargetMachineID: s.GetActiveMachineID(),
		RemotePath:      remotePath,
		IsDir:           st.IsDir(),
		Size:            st.Size(),
		ConnID:          filepath.Base(absLocal),
	}
	if err := protocol.WriteLineJSON(bs, hdr); err != nil {
		return "", nil, err
	}

	var ack protocol.StreamAck
	if err := bs.ReadLineJSON(&ack); err != nil {
		return "", nil, fmt.Errorf("upload handshake failed: %w", err)
	}
	if !ack.OK {
		return "", nil, fmt.Errorf("upload rejected: %s", ack.Error)
	}

	var localSHA string
	if st.IsDir() {
		_, _, localSHA, err = protocol.PackDirToStream(absLocal, bs)
	} else {
		_, localSHA, err = protocol.StreamFileFromDisk(absLocal, bs)
	}
	if err != nil {
		return "", nil, fmt.Errorf("upload stream error: %w", err)
	}

	var receipt protocol.TransferReceipt
	if err := bs.ReadLineJSON(&receipt); err != nil {
		return "", nil, fmt.Errorf("read upload receipt failed: %w", err)
	}
	if !receipt.OK {
		return "", nil, fmt.Errorf("remote write failed: %s", receipt.Error)
	}
	if localSHA != "" && receipt.SHA256 != "" && !strings.EqualFold(localSHA, receipt.SHA256) {
		return "", nil, fmt.Errorf("upload sha256 mismatch: local=%s remote=%s", localSHA, receipt.SHA256)
	}

	elapsed := time.Since(start)
	kind := "file"
	if st.IsDir() {
		kind = fmt.Sprintf("directory (%d files)", receipt.Files)
	}
	msg := fmt.Sprintf("Uploaded %s %s -> %s (%d bytes, sha256=%s, %s)", kind, absLocal, remotePath, receipt.Bytes, receipt.SHA256, elapsed.Round(time.Millisecond))
	return msg, map[string]any{
		"bytes":   receipt.Bytes,
		"files":   receipt.Files,
		"sha256":  receipt.SHA256,
		"is_dir":  st.IsDir(),
		"elapsed": elapsed.Milliseconds(),
	}, nil
}

func (s *Session) Download(remotePath, localPath string) (string, map[string]any, error) {
	start := time.Now()
	absLocal := agent.ExpandHome(localPath)
	if !filepath.IsAbs(absLocal) {
		wd, _ := os.Getwd()
		absLocal = filepath.Join(wd, absLocal)
	}
	absLocal = filepath.Clean(absLocal)

	bs, err := s.openStream()
	if err != nil {
		return "", nil, err
	}
	defer bs.Close()

	hdr := protocol.StreamHeader{
		StreamType:      protocol.StreamTypeFileDownload,
		TargetMachineID: s.GetActiveMachineID(),
		RemotePath:      remotePath,
	}
	if err := protocol.WriteLineJSON(bs, hdr); err != nil {
		return "", nil, err
	}

	var ack protocol.StreamAck
	if err := bs.ReadLineJSON(&ack); err != nil {
		return "", nil, fmt.Errorf("download handshake failed: %w", err)
	}
	if !ack.OK {
		return "", nil, fmt.Errorf("%s", ack.Error)
	}

	var bytesRead int64
	var filesCount int
	var shaHex string

	if ack.IsDir {
		bytesRead, filesCount, shaHex, err = protocol.UnpackDirFromStream(absLocal, bs)
	} else {
		if st, statErr := os.Stat(absLocal); statErr == nil && st.IsDir() && ack.Name != "" {
			absLocal = filepath.Join(absLocal, filepath.Base(ack.Name))
		}
		bytesRead, shaHex, err = protocol.ReceiveFileToDisk(absLocal, bs, ack.Size)
		filesCount = 1
	}
	if err != nil {
		return "", nil, fmt.Errorf("download receive failed: %w", err)
	}

	elapsed := time.Since(start)
	kind := "file"
	if ack.IsDir {
		kind = fmt.Sprintf("directory (%d files)", filesCount)
	}
	msg := fmt.Sprintf("Downloaded %s %s -> %s (%d bytes, sha256=%s, %s)", kind, remotePath, absLocal, bytesRead, shaHex, elapsed.Round(time.Millisecond))
	return msg, map[string]any{
		"bytes":      bytesRead,
		"files":      filesCount,
		"sha256":     shaHex,
		"is_dir":     ack.IsDir,
		"local_path": absLocal,
		"elapsed":    elapsed.Milliseconds(),
	}, nil
}

func (s *Session) Forwarding(action, id, direction, localAddr, remoteAddr string) (string, map[string]any, error) {
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list", "":
		return s.listForwards()
	case "remove", "delete", "stop", "close":
		return s.removeForward(id)
	case "add", "create", "start":
		return s.addForward(direction, localAddr, remoteAddr)
	default:
		return "", nil, fmt.Errorf("invalid forwarding action %q: must be \"add\", \"remove\", or \"list\"", action)
	}
}

func (s *Session) listForwards() (string, map[string]any, error) {
	s.fwdMu.Lock()
	entries := make([]*ForwardEntry, 0, len(s.forwards))
	for _, f := range s.forwards {
		entries = append(entries, f)
	}
	s.fwdMu.Unlock()

	if len(entries) == 0 {
		return "No active port forwarding rules.", map[string]any{"count": 0}, nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].CreatedAt.Before(entries[j].CreatedAt)
	})

	lines := []string{fmt.Sprintf("Active port forwarding rules (%d):", len(entries))}
	for _, f := range entries {
		arrow := "->"
		if f.Direction == "local_to_remote" {
			arrow = "<-"
		}
		lines = append(lines, fmt.Sprintf(
			"  #%s  [%s]  local %s %s remote %s  (machine: %s, active_conns: %d)",
			f.ID,
			f.Direction,
			f.LocalAddr,
			arrow,
			f.RemoteAddr,
			f.MachineID,
			atomic.LoadInt64(&f.ActiveConns),
		))
	}
	return strings.Join(lines, "\n"), map[string]any{"count": len(entries)}, nil
}

func (s *Session) removeForward(id string) (string, map[string]any, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "#")
	if id == "" {
		return "", nil, fmt.Errorf("forwarding remove requires `id`")
	}

	s.fwdMu.Lock()
	entry, ok := s.forwards[id]
	if ok {
		delete(s.forwards, id)
	}
	s.fwdMu.Unlock()

	if !ok {
		return "", nil, fmt.Errorf("forwarding rule #%s not found; run forwarding with action=\"list\"", id)
	}
	if entry.closeFn != nil {
		entry.closeFn()
	}
	return fmt.Sprintf("Removed port forwarding rule #%s (%s: local %s <-> remote %s)", entry.ID, entry.Direction, entry.LocalAddr, entry.RemoteAddr), map[string]any{
		"id": entry.ID,
	}, nil
}

func (s *Session) addForward(direction, localAddr, remoteAddr string) (string, map[string]any, error) {
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		direction = "remote_to_local"
	}
	if direction != "remote_to_local" && direction != "local_to_remote" {
		return "", nil, fmt.Errorf("invalid direction %q: must be \"remote_to_local\" or \"local_to_remote\"", direction)
	}
	localAddr = normalizeTCPAddr(localAddr)
	remoteAddr = normalizeTCPAddr(remoteAddr)
	if localAddr == "" || remoteAddr == "" {
		return "", nil, fmt.Errorf("forwarding add requires both `local_addr` and `remote_addr` (e.g. \"127.0.0.1:8080\")")
	}

	// Ensure we know which machine we're targeting
	targetMachine := s.GetActiveMachineID()
	if targetMachine == "" {
		machines, err := s.FetchMachines()
		if err != nil {
			return "", nil, err
		}
		if len(machines) == 0 {
			return "", nil, fmt.Errorf("no target machines are connected")
		}
		targetMachine = machines[0].MachineID
		s.SetActiveMachineID(targetMachine)
	}

	if direction == "remote_to_local" {
		return s.addRemoteToLocal(targetMachine, localAddr, remoteAddr)
	}
	return s.addLocalToRemote(targetMachine, localAddr, remoteAddr)
}

func (s *Session) addRemoteToLocal(machineID, localAddr, remoteAddr string) (string, map[string]any, error) {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen on local_addr %s failed: %w", localAddr, err)
	}
	boundLocal := ln.Addr().String()
	id := fmt.Sprintf("%d", atomic.AddUint64(&s.fwdSeq, 1))

	entry := &ForwardEntry{
		ID:         id,
		Direction:  "remote_to_local",
		MachineID:  machineID,
		LocalAddr:  boundLocal,
		RemoteAddr: remoteAddr,
		CreatedAt:  time.Now(),
		closeFn: func() {
			_ = ln.Close()
		},
	}

	s.fwdMu.Lock()
	s.forwards[id] = entry
	s.fwdMu.Unlock()

	go func() {
		for {
			localConn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&entry.ActiveConns, 1)
			go func(c net.Conn) {
				defer atomic.AddInt64(&entry.ActiveConns, -1)
				defer c.Close()

				bs, err := s.openStream()
				if err != nil {
					return
				}
				defer bs.Close()

				if err := protocol.WriteLineJSON(bs, protocol.StreamHeader{
					StreamType:      protocol.StreamTypeTCPConnect,
					TargetMachineID: machineID,
					RemoteAddr:      remoteAddr,
				}); err != nil {
					return
				}

				var ack protocol.StreamAck
				if err := bs.ReadLineJSON(&ack); err != nil || !ack.OK {
					return
				}

				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, _ = io.Copy(bs, c)
					_ = bs.Close()
				}()
				go func() {
					defer wg.Done()
					_, _ = io.Copy(c, bs)
					_ = c.Close()
				}()
				wg.Wait()
			}(localConn)
		}
	}()

	return fmt.Sprintf("Added remote_to_local forwarding #%s: local %s -> remote %s (machine %s)", id, boundLocal, remoteAddr, machineID), map[string]any{
		"id":          id,
		"direction":   "remote_to_local",
		"local_addr":  boundLocal,
		"remote_addr": remoteAddr,
		"machine_id":  machineID,
	}, nil
}

func (s *Session) addLocalToRemote(machineID, localAddr, remoteAddr string) (string, map[string]any, error) {
	ctrlStream, err := s.openStream()
	if err != nil {
		return "", nil, err
	}

	if err := protocol.WriteLineJSON(ctrlStream, protocol.StreamHeader{
		StreamType:      protocol.StreamTypeReverseListen,
		TargetMachineID: machineID,
		RemoteAddr:      remoteAddr,
	}); err != nil {
		_ = ctrlStream.Close()
		return "", nil, err
	}

	var ack protocol.StreamAck
	if err := ctrlStream.ReadLineJSON(&ack); err != nil {
		_ = ctrlStream.Close()
		return "", nil, fmt.Errorf("reverse_listen handshake failed: %w", err)
	}
	if !ack.OK {
		_ = ctrlStream.Close()
		return "", nil, fmt.Errorf("%s", ack.Error)
	}

	boundRemote := ack.BoundAddr
	if boundRemote == "" {
		boundRemote = remoteAddr
	}
	id := fmt.Sprintf("%d", atomic.AddUint64(&s.fwdSeq, 1))

	entry := &ForwardEntry{
		ID:         id,
		Direction:  "local_to_remote",
		MachineID:  machineID,
		LocalAddr:  localAddr,
		RemoteAddr: boundRemote,
		CreatedAt:  time.Now(),
		closeFn: func() {
			_ = ctrlStream.Close()
		},
	}

	s.fwdMu.Lock()
	s.forwards[id] = entry
	s.fwdMu.Unlock()

	go func() {
		defer func() {
			_ = ctrlStream.Close()
			s.fwdMu.Lock()
			if cur, ok := s.forwards[id]; ok && cur == entry {
				delete(s.forwards, id)
			}
			s.fwdMu.Unlock()
		}()

		for {
			var ev protocol.ReverseConnEvent
			if err := ctrlStream.ReadLineJSON(&ev); err != nil {
				return
			}
			if ev.ConnID == "" {
				continue
			}
			atomic.AddInt64(&entry.ActiveConns, 1)
			go func(connID string) {
				defer atomic.AddInt64(&entry.ActiveConns, -1)

				localConn, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
				if err != nil {
					return
				}
				defer localConn.Close()

				acceptStream, err := s.openStream()
				if err != nil {
					return
				}
				defer acceptStream.Close()

				if err := protocol.WriteLineJSON(acceptStream, protocol.StreamHeader{
					StreamType:      protocol.StreamTypeReverseAccept,
					TargetMachineID: machineID,
					ConnID:          connID,
				}); err != nil {
					return
				}

				var aAck protocol.StreamAck
				if err := acceptStream.ReadLineJSON(&aAck); err != nil || !aAck.OK {
					return
				}

				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					_, _ = io.Copy(acceptStream, localConn)
					_ = acceptStream.Close()
				}()
				go func() {
					defer wg.Done()
					_, _ = io.Copy(localConn, acceptStream)
					_ = localConn.Close()
				}()
				wg.Wait()
			}(ev.ConnID)
		}
	}()

	return fmt.Sprintf("Added local_to_remote forwarding #%s: remote %s -> local %s (machine %s)", id, boundRemote, localAddr, machineID), map[string]any{
		"id":          id,
		"direction":   "local_to_remote",
		"local_addr":  localAddr,
		"remote_addr": boundRemote,
		"machine_id":  machineID,
	}, nil
}

func normalizeTCPAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if !strings.Contains(addr, ":") {
		return "127.0.0.1:" + addr
	}
	return addr
}

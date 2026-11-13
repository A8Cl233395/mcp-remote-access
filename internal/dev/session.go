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
	ID          string
	Protocol    string // "tcp" | "udp"
	Direction   string // "remote_to_local" | "local_to_remote"
	MachineID   string
	LocalAddr   string
	RemoteAddr  string
	CreatedAt   time.Time
	ActiveConns int64
	closeFn     func()
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
		encTag := ""
		if m.Encoding != "" {
			encTag = fmt.Sprintf("  enc=%s", m.Encoding)
			if m.ConsoleEncoding != "" && m.ConsoleEncoding != m.Encoding {
				encTag += fmt.Sprintf("/%s", m.ConsoleEncoding)
			}
		}
		lines = append(lines, fmt.Sprintf(
			" %s %s  (%s/%s)  host=%s  user=%s%s  shell=%s (avail: %s)%s  workdir=%s  jobs=%d",
			marker,
			m.MachineID,
			m.OS,
			m.Arch,
			m.Hostname,
			m.User,
			adminTag,
			m.Shell,
			strings.Join(m.AvailableShells, ","),
			encTag,
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
	encStr := m.Encoding
	if encStr == "" {
		encStr = "utf-8"
	}
	if m.ConsoleEncoding != "" && m.ConsoleEncoding != encStr {
		encStr = fmt.Sprintf("%s (console: %s)", encStr, m.ConsoleEncoding)
	}
	summary := strings.Join([]string{
		fmt.Sprintf("Switched active machine to: %s", m.MachineID),
		fmt.Sprintf("  Hostname:         %s", m.Hostname),
		fmt.Sprintf("  OS / Arch:        %s / %s", m.OS, m.Arch),
		fmt.Sprintf("  User:             %s (admin/root: %s)", m.User, adminStr),
		fmt.Sprintf("  Workdir:          %s", m.Workdir),
		fmt.Sprintf("  Current Shell:    %s", m.Shell),
		fmt.Sprintf("  Available Shells: %s", strings.Join(m.AvailableShells, ", ")),
		fmt.Sprintf("  System Encoding:  %s", encStr),
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

func (s *Session) Forwarding(action, id, proto, direction, localAddr, remoteAddr string) (string, map[string]any, error) {
	action = strings.ToLower(strings.TrimSpace(action))
	switch action {
	case "list", "":
		return s.listForwards()
	case "remove", "delete", "stop", "close":
		return s.removeForward(id)
	case "add", "create", "start":
		return s.addForward(proto, direction, localAddr, remoteAddr)
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
		protoLabel := f.Protocol
		if protoLabel == "" {
			protoLabel = "tcp"
		}
		lines = append(lines, fmt.Sprintf(
			"  #%s  [%s/%s]  local %s %s remote %s  (machine: %s, active_conns: %d)",
			f.ID,
			protoLabel,
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
	protoLabel := entry.Protocol
	if protoLabel == "" {
		protoLabel = "tcp"
	}
	return fmt.Sprintf("Removed port forwarding rule #%s (%s: local %s <-> remote %s)", entry.ID, entry.Direction, entry.LocalAddr, entry.RemoteAddr), map[string]any{
		"id":       entry.ID,
		"protocol": protoLabel,
	}, nil
}

func (s *Session) addForward(proto, direction, localAddr, remoteAddr string) (string, map[string]any, error) {
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		proto = "tcp"
	}
	if proto != "tcp" && proto != "udp" {
		return "", nil, fmt.Errorf("invalid protocol %q: must be \"tcp\" or \"udp\"", proto)
	}

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

	if proto == "udp" {
		if direction == "remote_to_local" {
			return s.addRemoteToLocalUDP(targetMachine, localAddr, remoteAddr)
		}
		return s.addLocalToRemoteUDP(targetMachine, localAddr, remoteAddr)
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
		Protocol:   "tcp",
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
	openReverseListen := func(addr string) (*protocol.BufferedStream, string, error) {
		cs, err := s.openStream()
		if err != nil {
			return nil, "", err
		}
		if err := protocol.WriteLineJSON(cs, protocol.StreamHeader{
			StreamType:      protocol.StreamTypeReverseListen,
			TargetMachineID: machineID,
			RemoteAddr:      addr,
		}); err != nil {
			_ = cs.Close()
			return nil, "", err
		}
		var ack protocol.StreamAck
		if err := cs.ReadLineJSON(&ack); err != nil {
			_ = cs.Close()
			return nil, "", fmt.Errorf("reverse_listen handshake failed: %w", err)
		}
		if !ack.OK {
			_ = cs.Close()
			return nil, "", fmt.Errorf("%s", ack.Error)
		}
		bound := ack.BoundAddr
		if bound == "" {
			bound = addr
		}
		return cs, bound, nil
	}

	ctrlStream, boundRemote, err := openReverseListen(remoteAddr)
	if err != nil {
		return "", nil, err
	}

	id := fmt.Sprintf("%d", atomic.AddUint64(&s.fwdSeq, 1))
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var streamMu sync.Mutex
	curCtrl := ctrlStream

	entry := &ForwardEntry{
		ID:         id,
		Protocol:   "tcp",
		Direction:  "local_to_remote",
		MachineID:  machineID,
		LocalAddr:  localAddr,
		RemoteAddr: boundRemote,
		CreatedAt:  time.Now(),
		closeFn: func() {
			stopOnce.Do(func() {
				close(stopCh)
			})
			streamMu.Lock()
			if curCtrl != nil {
				_ = curCtrl.Close()
			}
			streamMu.Unlock()
		},
	}

	s.fwdMu.Lock()
	s.forwards[id] = entry
	s.fwdMu.Unlock()

	go func() {
		defer func() {
			streamMu.Lock()
			if curCtrl != nil {
				_ = curCtrl.Close()
			}
			streamMu.Unlock()
			s.fwdMu.Lock()
			if cur, ok := s.forwards[id]; ok && cur == entry {
				delete(s.forwards, id)
			}
			s.fwdMu.Unlock()
		}()

		for {
			streamMu.Lock()
			activeStream := curCtrl
			streamMu.Unlock()

			if activeStream != nil {
				for {
					var ev protocol.ReverseConnEvent
					if err := activeStream.ReadLineJSON(&ev); err != nil {
						_ = activeStream.Close()
						break
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
			}

			// If the rule was explicitly removed, exit immediately
			select {
			case <-stopCh:
				return
			case <-time.After(time.Second):
			}

			// Re-establish reverse_listen on boundRemote (or fallback to remoteAddr)
			nextStream, newBound, reErr := openReverseListen(boundRemote)
			if reErr != nil && boundRemote != remoteAddr {
				nextStream, newBound, reErr = openReverseListen(remoteAddr)
			}
			streamMu.Lock()
			curCtrl = nextStream
			if reErr == nil && newBound != "" {
				boundRemote = newBound
				entry.RemoteAddr = newBound
			}
			streamMu.Unlock()
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

type devUDPSession struct {
	bs           *protocol.BufferedStream
	peerAddr     net.Addr
	toDev        chan []byte
	done         chan struct{}
	closeOnce    sync.Once
	lastActivity int64 // unix timestamp in seconds
}

func (ds *devUDPSession) Close() {
	ds.closeOnce.Do(func() {
		close(ds.done)
		if ds.bs != nil {
			_ = ds.bs.Close()
		}
	})
}

func (s *Session) addRemoteToLocalUDP(machineID, localAddr, remoteAddr string) (string, map[string]any, error) {
	ln, err := net.ListenPacket("udp", localAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen udp on local_addr %s failed: %w", localAddr, err)
	}
	boundLocal := ln.LocalAddr().String()
	id := fmt.Sprintf("%d", atomic.AddUint64(&s.fwdSeq, 1))

	var sessMu sync.Mutex
	sessions := make(map[string]*devUDPSession)
	stopCh := make(chan struct{})
	var stopOnce sync.Once

	entry := &ForwardEntry{
		ID:         id,
		Protocol:   "udp",
		Direction:  "remote_to_local",
		MachineID:  machineID,
		LocalAddr:  boundLocal,
		RemoteAddr: remoteAddr,
		CreatedAt:  time.Now(),
		closeFn: func() {
			stopOnce.Do(func() {
				close(stopCh)
			})
			_ = ln.Close()
			sessMu.Lock()
			for _, ds := range sessions {
				ds.Close()
			}
			sessions = nil
			sessMu.Unlock()
		},
	}

	s.fwdMu.Lock()
	s.forwards[id] = entry
	s.fwdMu.Unlock()

	// Periodic cleanup of idle sessions (> 60s)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				now := time.Now().Unix()
				sessMu.Lock()
				for k, ds := range sessions {
					if now-atomic.LoadInt64(&ds.lastActivity) > 60 {
						ds.Close()
						delete(sessions, k)
					}
				}
				sessMu.Unlock()
			}
		}
	}()

	go func() {
		buf := make([]byte, protocol.MaxUDPPacketSize)
		for {
			n, peerAddr, err := ln.ReadFrom(buf)
			if err != nil {
				return
			}
			peerKey := peerAddr.String()

			sessMu.Lock()
			ds, ok := sessions[peerKey]
			if !ok {
				bs, sErr := s.openStream()
				if sErr != nil {
					sessMu.Unlock()
					continue
				}
				if err := protocol.WriteLineJSON(bs, protocol.StreamHeader{
					StreamType:      protocol.StreamTypeUDPConnect,
					TargetMachineID: machineID,
					RemoteAddr:      remoteAddr,
				}); err != nil {
					_ = bs.Close()
					sessMu.Unlock()
					continue
				}
				var ack protocol.StreamAck
				if err := bs.ReadLineJSON(&ack); err != nil || !ack.OK {
					_ = bs.Close()
					sessMu.Unlock()
					continue
				}

				ds = &devUDPSession{
					bs:           bs,
					peerAddr:     peerAddr,
					toDev:        make(chan []byte, 128),
					done:         make(chan struct{}),
					lastActivity: time.Now().Unix(),
				}
				sessions[peerKey] = ds
				atomic.AddInt64(&entry.ActiveConns, 1)

				// Stream -> Local UDP packet
				go func(sess *devUDPSession, key string) {
					defer atomic.AddInt64(&entry.ActiveConns, -1)
					defer func() {
						sessMu.Lock()
						delete(sessions, key)
						sessMu.Unlock()
						sess.Close()
					}()
					rBuf := make([]byte, protocol.MaxUDPPacketSize)
					for {
						rn, rErr := protocol.ReadPacket(sess.bs, rBuf)
						if rErr != nil {
							break
						}
						atomic.StoreInt64(&sess.lastActivity, time.Now().Unix())
						if _, err := ln.WriteTo(rBuf[:rn], sess.peerAddr); err != nil {
							break
						}
					}
				}(ds, peerKey)

				// Local UDP packet -> Stream
				go func(sess *devUDPSession) {
					for {
						select {
						case <-sess.done:
							return
						case p, open := <-sess.toDev:
							if !open {
								return
							}
							atomic.StoreInt64(&sess.lastActivity, time.Now().Unix())
							if err := protocol.WritePacket(sess.bs, p); err != nil {
								sess.Close()
								return
							}
						}
					}
				}(ds)
			}

			atomic.StoreInt64(&ds.lastActivity, time.Now().Unix())
			pktCopy := make([]byte, n)
			copy(pktCopy, buf[:n])
			select {
			case ds.toDev <- pktCopy:
			default:
			}
			sessMu.Unlock()
		}
	}()

	return fmt.Sprintf("Added remote_to_local forwarding #%s: local %s -> remote %s (machine %s, proto udp)", id, boundLocal, remoteAddr, machineID), map[string]any{
		"id":          id,
		"protocol":    "udp",
		"direction":   "remote_to_local",
		"local_addr":  boundLocal,
		"remote_addr": remoteAddr,
		"machine_id":  machineID,
	}, nil
}

func (s *Session) addLocalToRemoteUDP(machineID, localAddr, remoteAddr string) (string, map[string]any, error) {
	openUDPReverseListen := func(addr string) (*protocol.BufferedStream, string, error) {
		cs, err := s.openStream()
		if err != nil {
			return nil, "", err
		}
		if err := protocol.WriteLineJSON(cs, protocol.StreamHeader{
			StreamType:      protocol.StreamTypeUDPReverseListen,
			TargetMachineID: machineID,
			RemoteAddr:      addr,
		}); err != nil {
			_ = cs.Close()
			return nil, "", err
		}
		var ack protocol.StreamAck
		if err := cs.ReadLineJSON(&ack); err != nil {
			_ = cs.Close()
			return nil, "", fmt.Errorf("udp reverse_listen handshake failed: %w", err)
		}
		if !ack.OK {
			_ = cs.Close()
			return nil, "", fmt.Errorf("%s", ack.Error)
		}
		bound := ack.BoundAddr
		if bound == "" {
			bound = addr
		}
		return cs, bound, nil
	}

	ctrlStream, boundRemote, err := openUDPReverseListen(remoteAddr)
	if err != nil {
		return "", nil, err
	}

	id := fmt.Sprintf("%d", atomic.AddUint64(&s.fwdSeq, 1))
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	var streamMu sync.Mutex
	curCtrl := ctrlStream

	entry := &ForwardEntry{
		ID:         id,
		Protocol:   "udp",
		Direction:  "local_to_remote",
		MachineID:  machineID,
		LocalAddr:  localAddr,
		RemoteAddr: boundRemote,
		CreatedAt:  time.Now(),
		closeFn: func() {
			stopOnce.Do(func() {
				close(stopCh)
			})
			streamMu.Lock()
			if curCtrl != nil {
				_ = curCtrl.Close()
			}
			streamMu.Unlock()
		},
	}

	s.fwdMu.Lock()
	s.forwards[id] = entry
	s.fwdMu.Unlock()

	go func() {
		defer func() {
			streamMu.Lock()
			if curCtrl != nil {
				_ = curCtrl.Close()
			}
			streamMu.Unlock()
			s.fwdMu.Lock()
			if cur, ok := s.forwards[id]; ok && cur == entry {
				delete(s.forwards, id)
			}
			s.fwdMu.Unlock()
		}()

		for {
			streamMu.Lock()
			activeStream := curCtrl
			streamMu.Unlock()

			if activeStream != nil {
				for {
					var ev protocol.ReverseConnEvent
					if err := activeStream.ReadLineJSON(&ev); err != nil {
						_ = activeStream.Close()
						break
					}
					if ev.ConnID == "" {
						continue
					}
					atomic.AddInt64(&entry.ActiveConns, 1)
					go func(connID string) {
						defer atomic.AddInt64(&entry.ActiveConns, -1)

						lconn, err := net.Dial("udp", localAddr)
						if err != nil {
							return
						}
						defer lconn.Close()

						acceptStream, err := s.openStream()
						if err != nil {
							return
						}
						defer acceptStream.Close()

						if err := protocol.WriteLineJSON(acceptStream, protocol.StreamHeader{
							StreamType:      protocol.StreamTypeUDPReverseAccept,
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
						// Stream -> Local UDP
						go func() {
							defer wg.Done()
							buf := make([]byte, protocol.MaxUDPPacketSize)
							for {
								n, err := protocol.ReadPacket(acceptStream, buf)
								if err != nil {
									break
								}
								if _, err := lconn.Write(buf[:n]); err != nil {
									break
								}
							}
							_ = lconn.Close()
						}()
						// Local UDP -> Stream
						go func() {
							defer wg.Done()
							buf := make([]byte, protocol.MaxUDPPacketSize)
							for {
								n, err := lconn.Read(buf)
								if err != nil {
									break
								}
								if err := protocol.WritePacket(acceptStream, buf[:n]); err != nil {
									break
								}
							}
							_ = acceptStream.Close()
						}()
						wg.Wait()
					}(ev.ConnID)
				}
			}

			select {
			case <-stopCh:
				return
			case <-time.After(time.Second):
			}

			nextStream, newBound, reErr := openUDPReverseListen(boundRemote)
			if reErr != nil && boundRemote != remoteAddr {
				nextStream, newBound, reErr = openUDPReverseListen(remoteAddr)
			}
			streamMu.Lock()
			curCtrl = nextStream
			if reErr == nil && newBound != "" {
				boundRemote = newBound
				entry.RemoteAddr = newBound
			}
			streamMu.Unlock()
		}
	}()

	return fmt.Sprintf("Added local_to_remote forwarding #%s: remote %s -> local %s (machine %s, proto udp)", id, boundRemote, localAddr, machineID), map[string]any{
		"id":          id,
		"protocol":    "udp",
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

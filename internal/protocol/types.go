package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	HeaderUserKey = "X-User-Key"
	HeaderDevKey  = "X-Dev-Key"

	DefaultPort       = "20055"
	DefaultListenAddr = ":20055"

	StreamTypeListMachines  = "relay_list_machines"
	StreamTypeAgentRegister = "agent_register"
	StreamTypeRPC           = "agent_rpc"
	StreamTypeFileUpload    = "file_upload"
	StreamTypeFileDownload  = "file_download"
	StreamTypeTCPConnect       = "tcp_connect"
	StreamTypeReverseListen    = "reverse_listen"
	StreamTypeReverseAccept    = "reverse_accept"
	StreamTypeUDPConnect       = "udp_connect"
	StreamTypeUDPReverseListen = "udp_reverse_listen"
	StreamTypeUDPReverseAccept = "udp_reverse_accept"
)

// DefaultServerAddr is the built-in relay server address used when -server is not specified via CLI.
// Can also be set at compile time via -ldflags "-X mcp-remoteaccess/internal/protocol.DefaultServerAddr=..."
var DefaultServerAddr = "127.0.0.1:20055"

func DefaultYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = 256
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = 1024 * 1024 // 1MB window for fast file transfer
	cfg.LogOutput = io.Discard
	return cfg
}

type UpdateManifest struct {
	Available bool   `json:"available"`
	Filename  string `json:"filename,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

type MachineInfo struct {
	MachineID       string   `json:"machine_id"`
	Hostname        string   `json:"hostname"`
	OS              string   `json:"os"`
	Arch            string   `json:"arch"`
	User            string   `json:"user"`
	IsAdmin         bool     `json:"is_admin"`
	Workdir         string   `json:"workdir"`
	Shell           string   `json:"shell"`
	AvailableShells []string `json:"available_shells"`
	Encoding        string   `json:"encoding,omitempty"`
	ConsoleEncoding string   `json:"console_encoding,omitempty"`
	Version         string   `json:"version"`
	ConnectedAt     int64    `json:"connected_at"`
	RunningJobs     int      `json:"running_jobs,omitempty"`
}

type StreamHeader struct {
	StreamType      string          `json:"stream_type"`
	TargetMachineID string          `json:"target_machine_id,omitempty"`
	RemotePath      string          `json:"remote_path,omitempty"`
	RemoteAddr      string          `json:"remote_addr,omitempty"`
	ConnID          string          `json:"conn_id,omitempty"`
	IsDir           bool            `json:"is_dir,omitempty"`
	Size            int64           `json:"size,omitempty"`
	Machine         *MachineInfo    `json:"machine,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

type StreamAck struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	BoundAddr string          `json:"bound_addr,omitempty"`
	IsDir     bool            `json:"is_dir,omitempty"`
	Size      int64           `json:"size,omitempty"`
	Name      string          `json:"name,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type ReverseConnEvent struct {
	ConnID     string `json:"conn_id"`
	RemotePeer string `json:"remote_peer,omitempty"`
}

type TransferReceipt struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Bytes  int64  `json:"bytes"`
	Files  int    `json:"files"`
	SHA256 string `json:"sha256"`
}

type RPCRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	OK       bool           `json:"ok"`
	Content  string         `json:"content,omitempty"`
	Error    string         `json:"error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Machine  *MachineInfo   `json:"machine,omitempty"`
}

// BufferedStream wraps an io.ReadWriteCloser with a bufio.Reader so bytes
// buffered during line-delimited JSON header reading are never lost when
// transitioning to raw binary streaming.
type BufferedStream struct {
	io.ReadWriteCloser
	r *bufio.Reader
}

func NewBufferedStream(rwc io.ReadWriteCloser) *BufferedStream {
	if bs, ok := rwc.(*BufferedStream); ok {
		return bs
	}
	return &BufferedStream{
		ReadWriteCloser: rwc,
		r:               bufio.NewReaderSize(rwc, 32*1024),
	}
}

func (b *BufferedStream) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

func (b *BufferedStream) ReadLineJSON(v any) error {
	line, err := b.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func WriteLineJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func SendRPC(stream io.ReadWriteCloser, method string, params any) (*RPCResponse, error) {
	bs := NewBufferedStream(stream)
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}
	req := RPCRequest{
		Method: method,
		Params: rawParams,
	}
	if err := WriteLineJSON(bs, req); err != nil {
		return nil, fmt.Errorf("write rpc request: %w", err)
	}
	var resp RPCResponse
	if err := bs.ReadLineJSON(&resp); err != nil {
		return nil, fmt.Errorf("read rpc response: %w", err)
	}
	return &resp, nil
}

// RelayBidirectional copies bytes bidirectionally between a and b, closing each side when its copy ends.
func RelayBidirectional(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		_ = a.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		_ = b.Close()
	}()
	wg.Wait()
}

package dev

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type MCPToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type MCPContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type MCPCallToolResult struct {
	Content []MCPContentBlock `json:"content"`
	IsError bool              `json:"isError,omitempty"`
	Meta    map[string]any    `json:"_meta,omitempty"`
}

type MCPServer struct {
	session *Session
	writeMu sync.Mutex
}

func NewMCPServer(session *Session) *MCPServer {
	return &MCPServer{session: session}
}

func (m *MCPServer) ServeStdio(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	// Support up to 32MB JSON-RPC messages
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 32*1024*1024)

	var wg sync.WaitGroup
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		rawCopy := append([]byte(nil), line...)
		wg.Add(1)
		go func(payload []byte) {
			defer wg.Done()
			m.handleRawMessage(payload, w)
		}(rawCopy)
	}
	wg.Wait()
	return scanner.Err()
}

func (m *MCPServer) sendResponse(w io.Writer, msg JSONRPCMessage) {
	msg.JSONRPC = "2.0"
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	data = append(data, '\n')
	m.writeMu.Lock()
	_, _ = w.Write(data)
	m.writeMu.Unlock()
}

func (m *MCPServer) handleRawMessage(payload []byte, w io.Writer) {
	var req JSONRPCMessage
	if err := json.Unmarshal(payload, &req); err != nil {
		return
	}
	// Notifications have no ID
	if len(req.ID) == 0 || string(req.ID) == "null" {
		return
	}

	switch req.Method {
	case "initialize":
		var initParams struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &initParams)
		protoVer := initParams.ProtocolVersion
		if protoVer == "" {
			protoVer = "2024-11-05"
		}
		m.sendResponse(w, JSONRPCMessage{
			ID: req.ID,
			Result: map[string]any{
				"protocolVersion": protoVer,
				"capabilities": map[string]any{
					"tools": map[string]any{
						"listChanged": false,
					},
				},
				"serverInfo": map[string]any{
					"name":    "mcp-remoteaccess",
					"version": "1.0.0",
				},
			},
		})

	case "ping":
		m.sendResponse(w, JSONRPCMessage{
			ID:     req.ID,
			Result: map[string]any{},
		})

	case "tools/list":
		m.sendResponse(w, JSONRPCMessage{
			ID: req.ID,
			Result: map[string]any{
				"tools": ToolDefinitions(),
			},
		})

	case "tools/call":
		var callParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &callParams); err != nil {
			m.sendResponse(w, JSONRPCMessage{
				ID: req.ID,
				Error: &JSONRPCError{
					Code:    -32602,
					Message: fmt.Sprintf("invalid tools/call params: %v", err),
				},
			})
			return
		}
		res := m.CallTool(callParams.Name, callParams.Arguments)
		m.sendResponse(w, JSONRPCMessage{
			ID:     req.ID,
			Result: res,
		})

	case "resources/list":
		m.sendResponse(w, JSONRPCMessage{
			ID:     req.ID,
			Result: map[string]any{"resources": []any{}},
		})

	case "prompts/list":
		m.sendResponse(w, JSONRPCMessage{
			ID:     req.ID,
			Result: map[string]any{"prompts": []any{}},
		})

	default:
		m.sendResponse(w, JSONRPCMessage{
			ID: req.ID,
			Error: &JSONRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		})
	}
}

func (m *MCPServer) CallTool(name string, rawArgs json.RawMessage) MCPCallToolResult {
	if len(rawArgs) == 0 || string(rawArgs) == "null" {
		rawArgs = []byte("{}")
	}

	text, meta, err := m.dispatchTool(name, rawArgs)
	if err != nil {
		return MCPCallToolResult{
			Content: []MCPContentBlock{{Type: "text", Text: err.Error()}},
			IsError: true,
		}
	}
	return MCPCallToolResult{
		Content: []MCPContentBlock{{Type: "text", Text: text}},
		IsError: false,
		Meta:    meta,
	}
}

func (m *MCPServer) dispatchTool(name string, rawArgs json.RawMessage) (string, map[string]any, error) {
	switch name {
	case "list_machines":
		return m.session.ListMachinesFormatted()

	case "switch_machine":
		var args struct {
			MachineID string `json:"machine_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", nil, err
		}
		return m.session.SwitchMachine(args.MachineID)

	case "switch_workdir", "read", "write", "edit", "glob", "grep", "shell", "job_list", "job_read", "job_kill":
		var generic any
		_ = json.Unmarshal(rawArgs, &generic)
		resp, err := m.session.CallRPC(name, generic)
		if err != nil {
			return "", nil, err
		}
		if !resp.OK {
			return "", nil, fmt.Errorf("%s", resp.Error)
		}
		return resp.Content, resp.Metadata, nil

	case "upload":
		var args struct {
			LocalPath  string `json:"local_path"`
			RemotePath string `json:"remote_path"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", nil, err
		}
		if args.LocalPath == "" || args.RemotePath == "" {
			return "", nil, fmt.Errorf("upload requires both `local_path` and `remote_path`")
		}
		return m.session.Upload(args.LocalPath, args.RemotePath)

	case "download":
		var args struct {
			RemotePath string `json:"remote_path"`
			LocalPath  string `json:"local_path"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", nil, err
		}
		if args.RemotePath == "" || args.LocalPath == "" {
			return "", nil, fmt.Errorf("download requires both `remote_path` and `local_path`")
		}
		return m.session.Download(args.RemotePath, args.LocalPath)

	case "forwarding", "fowarding":
		var args struct {
			Action     string `json:"action"`
			ID         string `json:"id,omitempty"`
			Protocol   string `json:"protocol,omitempty"`
			Direction  string `json:"direction,omitempty"`
			LocalAddr  string `json:"local_addr,omitempty"`
			RemoteAddr string `json:"remote_addr,omitempty"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", nil, err
		}
		return m.session.Forwarding(args.Action, args.ID, args.Protocol, args.Direction, args.LocalAddr, args.RemoteAddr)

	default:
		return "", nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func ToolDefinitions() []MCPToolDefinition {
	return []MCPToolDefinition{
		{
			Name:        "list_machines",
			Description: "List all remote target machines currently connected to the relay server, showing their machine_id, OS, architecture, user, active shell, available shells, current working directory, and running background jobs.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "switch_machine",
			Description: "Switch the active target machine for all subsequent remote tool calls and return its full environment snapshot.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"machine_id": map[string]any{
						"type":        "string",
						"description": "Target machine_id or hostname (from list_machines).",
					},
				},
				"required": []string{"machine_id"},
			},
		},
		{
			Name:        "switch_workdir",
			Description: "Change the default working directory on the active remote target machine. Relative paths in read, write, edit, glob, grep, upload, download, and shell resolve against this directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Target directory path on the remote machine (absolute, relative, or ~).",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "read",
			Description: "Read the contents of a file or directory on the remote machine. Each text line is prefixed by its 1-based line number as <line>: <content>. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File or directory to read on the remote machine.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "The line or directory entry to start reading from (1-based).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000).",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "write",
			Description: "Writes a file to the remote machine's filesystem, overwriting if one exists. Missing parent directories are created automatically.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to write to on the remote machine.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Content to write to the file.",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "edit",
			Description: "Edit the contents of a file on the remote machine by finding and replacing exact text. Omit line-number prefixes from Read output. Preserves existing CRLF or LF line endings. Retries ignoring typographic quote/dash/space variants and trailing whitespace if exact match misses.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File to edit on the remote machine.",
					},
					"oldString": map[string]any{
						"type":        "string",
						"description": "Exact text to find and replace.",
					},
					"newString": map[string]any{
						"type":        "string",
						"description": "Text to replace oldString with (must differ from oldString).",
					},
					"replaceAll": map[string]any{
						"type":        "boolean",
						"description": "Whether to replace every occurrence of oldString. Defaults to false.",
					},
				},
				"required": []string{"path", "oldString", "newString"},
			},
		},
		{
			Name:        "glob",
			Description: "Search file paths on the remote machine using a glob pattern (examples: \"**/*.ts\", \"src/**/*.py\").",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Glob pattern to match files against.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to search on the remote machine. Defaults to the current working directory.",
					},
					"hidden": map[string]any{
						"type":        "boolean",
						"description": "Include hidden files and directories (default: false).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of matching files to return (default: 100).",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search file contents on the remote machine using regular expression syntax or literal text matching. Returns matching file paths, line numbers, and line previews.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Regular expression or literal text to match in file contents.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "File or directory to search on the remote machine. Defaults to the current working directory.",
					},
					"include": map[string]any{
						"type":        "string",
						"description": "Glob pattern to filter files (for example, \"*.js\" or \"*.{ts,tsx}\").",
					},
					"literal": map[string]any{
						"type":        "boolean",
						"description": "Treat `pattern` as exact text instead of a regular expression (default: false).",
					},
					"caseSensitive": map[string]any{
						"type":        "boolean",
						"description": "Use case-sensitive matching (default: true).",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of matching lines to return (default: 100).",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "shell",
			Description: "Execute a shell command on the remote machine and return its output. Commands accept an optional timeout; a command that does not finish within 30 seconds is moved to a background job. Set timeout to 0 to start in the background and wait once for its first output (up to 3000 ms). Background jobs survive network reconnects; read them with job_read and stop them with job_kill.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "Shell command string to execute.",
					},
					"workdir": map[string]any{
						"type":        "string",
						"description": "Working directory to execute the command in. Defaults to the remote machine's current working directory.",
					},
					"shell": map[string]any{
						"type":        "string",
						"description": "Optional shell override (e.g. pwsh, powershell, cmd, bash, sh, ash).",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"maximum":     900000,
						"description": "Timeout in milliseconds. Set to 0 to start in the background and wait once for its first output (up to 3000 ms). Defaults to 30000 ms.",
					},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "job_list",
			Description: "List background jobs on the active remote machine with their id, command, status, elapsed time, and exit code.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "job_read",
			Description: "Read output from a background job on the active remote machine. Reads are incremental: omitting offset returns only output produced since the previous read, while offset: 0 re-reads from the beginning; the result includes next_offset.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"description": "Job id returned by the shell tool or job_list.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"description": "Byte offset to read from. Omit to continue from the previous read; pass 0 to re-read from the beginning.",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"minimum":     0,
						"maximum":     900000,
						"description": "Timeout in milliseconds. Blocks only while there is no new output. Defaults to 5000 ms.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "job_kill",
			Description: "Stop a background job and its child processes on the active remote machine, then return its final status and any unread output.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"description": "Job id returned by the shell tool or job_list.",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "upload",
			Description: "Upload a file or directory from the developer's local machine to the active remote target machine over a binary stream with SHA256 verification.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local_path": map[string]any{
						"type":        "string",
						"description": "Path to the source file or directory on the developer's local machine.",
					},
					"remote_path": map[string]any{
						"type":        "string",
						"description": "Destination file or directory path on the remote target machine.",
					},
				},
				"required": []string{"local_path", "remote_path"},
			},
		},
		{
			Name:        "download",
			Description: "Download a file or directory from the active remote target machine to the developer's local machine over a binary stream with SHA256 verification.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote_path": map[string]any{
						"type":        "string",
						"description": "Source file or directory path on the remote target machine.",
					},
					"local_path": map[string]any{
						"type":        "string",
						"description": "Destination file or directory path on the developer's local machine.",
					},
				},
				"required": []string{"remote_path", "local_path"},
			},
		},
		{
			Name:        "forwarding",
			Description: "Manage bidirectional TCP and UDP port forwarding tunnels between the developer's local machine and the active remote target machine.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"add", "remove", "list"},
						"description": "Action to perform: \"add\" a tunnel, \"remove\" a tunnel by id, or \"list\" active tunnels.",
					},
					"id": map[string]any{
						"type":        "string",
						"description": "Tunnel ID (required when action is \"remove\").",
					},
					"protocol": map[string]any{
						"type":        "string",
						"enum":        []string{"tcp", "udp"},
						"description": "Transport protocol: \"tcp\" (default) or \"udp\".",
					},
					"direction": map[string]any{
						"type":        "string",
						"enum":        []string{"remote_to_local", "local_to_remote"},
						"description": "\"remote_to_local\" listens on local_addr and forwards to remote_addr on the target machine; \"local_to_remote\" listens on remote_addr on the target machine and forwards to local_addr on the developer machine.",
					},
					"local_addr": map[string]any{
						"type":        "string",
						"description": "Developer machine address (e.g. \"127.0.0.1:8080\" or \"127.0.0.1:0\" for dynamic port).",
					},
					"remote_addr": map[string]any{
						"type":        "string",
						"description": "Remote target machine address (e.g. \"127.0.0.1:3000\" or \"127.0.0.1:0\").",
					},
				},
				"required": []string{"action"},
			},
		},
	}
}

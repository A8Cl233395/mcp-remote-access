# AGENTS.md

This file provides architectural context, build/test commands, and engineering invariants for AI agents working in `mcp-remoteaccess`.

## Project Overview

`mcp-remoteaccess` is a zero-configuration remote machine control, file streaming, and bidirectional TCP/UDP port forwarding suite built for AI agents over the Model Context Protocol (MCP).

### Three-Component Architecture
1. **`cmd/relay-server` (`internal/relay`)**:
   - Stateless public relay server autenticating clients via two fixed keys (`-user-key` and `-dev-key`).
   - Bridges multiplexed Yamux streams (`github.com/hashicorp/yamux`) over WebSocket (`github.com/gorilla/websocket`) between `dev-client` and target `user-client` instances.
   - Serves `./releases/user-client-{os}-{arch}[.exe]` over `/update/manifest` and `/update/binary` for automatic client self-updates.
2. **`cmd/user-client` (`internal/agent`)**:
   - Runs on remote target machines (Windows & Linux) in either GUI mode (default when launched without `-cli`) or CLI mode (`-cli`).
   - Executes file operations, shell commands, decoupled background jobs, and TCP port forwarding without human approval prompts.
   - Holds OS wake locks (`SetThreadExecutionState` on Windows, `systemd-inhibit` on Linux) while connected to prevent idle sleep.
   - Auto-updates before connecting to the main relay channel using the `.old` rename swap pattern so running `.exe` binaries on Windows can self-replace.
3. **`cmd/dev-client` (`internal/dev`)**:
   - Runs on the developer's local machine and exposes a standard MCP JSON-RPC 2.0 server over `stdio`.
   - Connects to `relay-server` to route tool calls to the active remote machine, stream local files/directories for `upload`/`download`, and bind/dial local TCP sockets for `forwarding`.

---

## Build & Test Commands

- **Build all Windows & Linux binaries and populate `./releases/`**:
  ```powershell
  .\build.ps1
  ```
- **Run end-to-end integration tests**:
  ```powershell
  go test -count=1 -v ./test/...
  ```

---

## Core Engineering Invariants

1. **Pure Go (`CGO_ENABLED=0`) Cross-Compilation**:
   - Never introduce CGo dependencies. All Windows and Linux binaries must compile from a single host via `CGO_ENABLED=0`.
   - Windows GUI (`internal/agent/gui_windows.go`), process tree termination (`internal/agent/proc_windows.go`), and wake locks (`internal/agent/power_windows.go`) use pure Go `syscall` (`user32.dll`, `gdi32.dll`, `kernel32.dll`).
2. **TLS & Anti-Downgrade Security Model**:
   - **Default (Strict Mode)**: `relay-server` requires `-tls-cert` and `-tls-key`. `user-client` and `dev-client` strictly rewrite all server addresses to `https://` and `wss://` with zero fallback to `http://` or `ws://` (preventing SSL-stripping / downgrade attacks) and enforce system CA verification (`InsecureSkipVerify: false`).
   - **Manual Override (`-insecure`)**: Passing `-insecure` on `relay-server` enables an auto-generated self-signed TLS certificate and a 1-byte peek listener (`FlexTLSListener`) that accepts both TLS and plain HTTP/WS. Passing `-insecure` on `user-client` (CLI) or `dev-client` skips certificate verification (`InsecureSkipVerify: true`). Never expose `-insecure` in the `user-client` GUI.
3. **Backward-Compatible Auto-Update Protocol**:
   - Do not break the HTTP contract of `GET /update/manifest?os=<os>&arch=<arch>` or `GET /update/binary?os=<os>&arch=<arch>`. Ancient `user-client` binaries rely on this endpoint before establishing the WebSocket/Yamux session.
4. **Stream Multiplexing & Framing (`internal/protocol`)**:
   - Always wrap `yamux.Stream` with `protocol.NewBufferedStream` before calling `ReadLineJSON` so bytes buffered after the newline header are preserved when transitioning to raw binary `io.Copy` (`upload`, `download`, `forwarding`).
   - In `UnpackDirFromStream` (`internal/protocol/archive.go`), keep `gr.Multistream(false)` enabled on `gzip.Reader` so tar+gzip stream extraction does not block waiting for a subsequent gzip stream.
5. **MCP Tool Behavior Alignment**:
   - File tools (`read`, `write`, `edit`, `glob`, `grep` in `internal/agent/fs.go`) follow OpenCode's exact line-numbering, pagination, 3-tier `edit` matching (exact -> Unicode normalized -> line-trimmed), and CRLF/LF preservation semantics.
   - Shell and background job tools (`shell`, `job_list`, `job_read`, `job_kill` in `internal/agent/jobs.go`) follow the `opencode-background-jobs` plugin semantics and remain decoupled from WebSocket session reconnects.
   - Shell auto-detection fallback chains (`internal/agent/shell.go`):
     - Windows: `pwsh` -> `powershell` -> `cmd`
     - Linux: `bash` -> `sh` -> `ash`

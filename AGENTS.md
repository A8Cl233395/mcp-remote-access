# AGENTS.md

## Architecture

Three-component pure-Go (`CGO_ENABLED=0`) suite bridging local MCP `stdio` to remote machines over WebSocket + `hashicorp/yamux`:

- `cmd/relay-server` (`internal/relay`): Stateless relay authenticating via `-user-key` / `-dev-key`; routes Yamux streams between `dev-client` and `user-client`; serves `./releases/user-client-{os}-{arch}[.exe]` on `/update/manifest` and `/update/binary`; optionally exposes server-side HTTP/SSE MCP endpoints (`/mcp`, `/sse`, `/messages`) and an OAuth 2.0 shim (`mcp.go`, `oauth.go`) when `-mcp` is enabled.
- `cmd/user-client` (`internal/agent`): Remote agent running in GUI (default) or headless CLI (`-cli`) mode. Executes file/shell/background-job/port-forward (TCP & UDP) operations without approval prompts, holds OS wake locks while connected, and self-updates before connecting.
- `cmd/dev-client` (`internal/dev`): Local MCP JSON-RPC 2.0 `stdio` server; forwards tool RPCs and handles local disk (`upload`/`download`) and local sockets (`forwarding`).

## Build & Test Commands

- **Full cross-platform build (Windows `amd64`/`386`, Linux `amd64`/`arm64`, populates `./bin/` and `./releases/`)**:
  ```powershell
  .\build.ps1
  ```
  Reads `DEFAULT_SERVER` (or `DEFAULT_SERVER_ADDR` / `MCP_SERVER_ADDR`) from `.env` or `-DefaultServer` and embeds it via `-ldflags "-X mcp-remoteaccess/internal/protocol.DefaultServerAddr=..."`. Downloads `.toolchain/go-win7` (`XTLS/go-win7`, `patched-1.27.4`) on first run to build `user-client-windows-386.exe`.
- **Manual single-binary build**: Always set `CGO_ENABLED=0`; Windows `user-client.exe` also requires `-ldflags="-H=windowsgui"` (it reattaches to parent console via `EnsureCLIConsole()` when passed CLI args).
- **Run all tests (unit + E2E)**:
  ```powershell
  go test -count=1 ./...
  ```
- **Run a single test or E2E suite**:
  ```powershell
  go test -count=1 -v ./test -run TestEndToEndAllToolsAndAutoUpdate
  go test -count=1 -v ./internal/agent -run TestReadWriteEditGrepEncodings
  ```

## Core Engineering Invariants

1. **Zero CGo & Windows 7 Compatibility**:
   - Never add CGo dependencies. Windows GUI (`internal/agent/gui_windows.go`), process tree termination (`proc_windows.go`), wake locks (`power_windows.go`), and codepage lookup (`encoding_windows.go`) use pure Go `syscall` (`user32.dll`, `gdi32.dll`, `kernel32.dll`).
   - Linux GUI (`internal/agent/gui_linux.go`) serves a loopback web UI with a heartbeat watchdog (no GTK/Qt) and falls back to CLI only if `DISPLAY`/`WAYLAND_DISPLAY` are unset and `-key` is provided.
   - Keep `cmd/user-client` and `internal/agent` compatible with `.toolchain/go-win7` (`GOTOOLCHAIN=local` in `build.ps1`) so the `windows/386` Win7 build does not break.

2. **TLS & Anti-Downgrade Model**:
   - **Strict mode (default)**: `relay-server` requires `-tls-cert` and `-tls-key`. Clients rewrite all server addresses to `https://`/`wss://` (defaulting port to `:20055`), never fall back to plaintext, and verify system Root CAs (`agent.CustomRootCAs` is test-only).
   - **`-insecure` mode**: On `relay-server`, generates an ephemeral self-signed cert and enables `FlexTLSListener` (1-byte `0x16` peek accepting both TLS and plain HTTP/WS). On `user-client` (CLI only) / `dev-client`, skips cert verification and allows `http://`/`ws://`. Never expose `-insecure` in the `user-client` GUI.

3. **Auto-Update, Persistence & Credential Handoff**:
   - Preserve the HTTP contract of `GET /update/manifest?os=<os>&arch=<arch>` and `GET /update/binary?os=<os>&arch=<arch>` (`internal/relay/server.go` maps `i386`/`x86` -> `386` and `windows7`/`win7` -> `windows`). `user-client` self-replaces via `.new` -> `.old` rename swap.
   - Never write `user-client.json` to disk unless "Save Login State" / `-save` is enabled (`RemoveConfigFile` cleans up the config directory when disabled).
   - Across a self-update restart, pass transient credentials/options via `MCP_HANDOFF_STATE`; `ConsumeRestartState()` must immediately unset it so child `shell` processes never inherit secrets.

4. **Yamux Stream Framing (`internal/protocol`)**:
   - Always wrap `yamux.Stream` with `protocol.NewBufferedStream` before `ReadLineJSON` so bytes buffered past `\n` are not lost when switching to raw `io.Copy` (`upload`, `download`, `forwarding`).
   - Keep `gr.Multistream(false)` in `UnpackDirFromStream` (`internal/protocol/archive.go`) so tar.gz extraction does not block waiting for another gzip stream.

5. **MCP Tool, Encoding & Job Semantics (`internal/agent`)**:
   - File tools (`fs.go`) match OpenCode semantics: 1-based line numbers, 2000-line / 50KB pagination, 3-tier `edit` matching (exact -> Unicode normalized -> line-trimmed), and CRLF/LF preservation.
   - Multi-encoding (`encoding.go`): Check BOMs (UTF-8-BOM, UTF-16LE/BE) *before* the NUL-byte binary check; auto-detect non-UTF-8 files using `SystemFileEncoding()` first and preserve encoding/BOM on `edit`. `write` defaults to `utf-8` and must reject `encoding: "auto"`. `shellStreamWriter` stream-decodes child console output into UTF-8.
   - Background jobs (`jobs.go`) remain decoupled from WebSocket/Yamux reconnects and kill entire process trees (`taskkill /f /t` on Windows, `-pgid` on Unix).
   - Shell fallback chains (`shell.go`): Windows `pwsh -> powershell -> cmd`; Linux `bash -> sh -> ash`.

6. **Disconnect, Kick & Error Popup Semantics (`internal/protocol`, `internal/relay`, `internal/agent`)**:
   - `relay-server` kicks duplicate `MachineID` sessions via `WSConn.CloseWithCode(protocol.CloseCodeKicked, ...)` (`4001`) asynchronously before closing Yamux so the replaced client receives the close code.
   - `user-client` only auto-reconnects on unexpected/transient drops (`EOF`, `1006`, `1001`, `5xx`, timeouts, DNS errors). Non-unexpected failures (`FatalError`: `ECONNREFUSED`/`WSAECONNREFUSED`, HTTP `4xx`/invalid key, TLS errors, registration rejection, and WS close codes `1000`, `1008`, `4000-4999`) stop without reconnecting and pop up an error (`MessageBoxW` on Windows UI thread; non-blocking HTML modal on Linux so `alert()` never stalls the heartbeat watchdog).

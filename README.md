# MCP Remote Access (`mcp-remote-access`)

专为 AI Agent（OpenCode / Claude Desktop 等）设计的免配置远程机器控制、文件流传输与双向端口穿透套件。

## 架构概览

```text
[AI Agent (OpenCode / Claude 等)]
       │  (1) MCP Stdio JSON-RPC 2.0 ──► [开发者客户端 (dev-client)]  ◄── 本地磁盘流式上传/下载 (upload / download)
       │                                           │                     ◄── 本地 TCP/UDP 端口双向转发 (forwarding)
       │                                           │  (WebSocket + Yamux, X-Dev-Key)
       │                                           ▼
       └─ (2) 可选直连 HTTP/SSE (-mcp) ──► [公网服务器 (relay-server)]  ── 托管 ./releases/ 供客户端自动更新
                                                   ▲
                                                   │  (WebSocket + Yamux, X-User-Key)
                                           [用户客户端 (user-client)]   ── 目标机执行 (Shell/File/Job/PortForward/多编码/防休眠/GUI+CLI)
```

---

## 编译选项与构建说明

项目全程使用纯 Go（`CGO_ENABLED=0`）构建，无任何外部动态库依赖，支持在单机上一键交叉编译 Windows（含 Windows 7 32 位）与 Linux（`amd64` / `arm64`）单文件可执行程序。

### 1. 使用 `build.ps1` 一键编译

- **自动从 `.env` 读取公网服务器地址（推荐）**：
  复制 `.env.example` 为 `.env`，填入你的中继服务器地址：
  ```env
  DEFAULT_SERVER=example.com:20055
  ```
  然后直接执行脚本，脚本会自动读取并注入（支持 `DEFAULT_SERVER`、`DEFAULT_SERVER_ADDR` 或 `MCP_SERVER_ADDR`）：
  ```powershell
  .\build.ps1
  ```
- **命令行传参注入自定义服务器地址**：
  ```powershell
  .\build.ps1 -DefaultServer "example.com:20055"
  ```
- **使用本地缺省地址（`127.0.0.1:20055`）**：
  若未配置 `.env` 且未传参，默认使用本地回环地址。
  > 若省略端口（如 `-DefaultServer "example.com"`），客户端运行时也会自动补齐默认端口 `:20055`。
- **Windows 7 32 位（`windows/386`）工具链**：
  首次运行 `build.ps1` 时，会自动下载并校验 [`XTLS/go-win7`](https://github.com/XTLS/go-win7)（`patched-1.27.4`）至 `.toolchain/go-win7`，专门用于编译原生兼容 Windows 7 32 位的 `user-client-windows-386.exe`。

### 2. 手动 `go build` 与 `-ldflags` 编译选项详解

如果你需要手动单独编译某一个组件或架构，可使用以下编译参数：

| 编译参数 / 环境变量 | 作用说明 |
| :--- | :--- |
| `CGO_ENABLED=0` | 禁用 CGo，产出零依赖纯静态单文件二进制（兼容各 Linux 发行版与 Windows） |
| `GOOS=windows` / `GOOS=linux` | 目标操作系统 |
| `GOARCH=amd64` / `arm64` / `386` | 目标 CPU 架构（构建 Win7 32 位 `386` 需使用 `.toolchain/go-win7` 并配合 `GOTOOLCHAIN=local`） |
| `-trimpath` | 移除编译机本地源码绝对路径信息 |
| `-ldflags="-s -w"` | 剥离符号表与 DWARF 调试信息，减小可执行文件体积 |
| `-ldflags="-H=windowsgui"` | **仅 Windows `user-client.exe` 使用**：编译为 Windows GUI 子系统，双击运行时彻底不弹黑色 CMD 控制台窗口（传入 CLI 参数时会自动挂载父终端输出） |
| `-ldflags="-X mcp-remoteaccess/internal/protocol.DefaultServerAddr=..."` | 编译期覆写客户端内置的默认中继服务器地址 |

手动编译示例（PowerShell）：
```powershell
# 编译 Windows 用户客户端（无黑框 + 注入默认公网地址）
$env:CGO_ENABLED="0"; $env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w -H=windowsgui -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=example.com:20055" -o bin/user-client.exe ./cmd/user-client

# 编译 Linux 用户客户端（amd64 / arm64）
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=example.com:20055" -o bin/user-client-linux-amd64 ./cmd/user-client
$env:GOARCH="arm64"
go build -trimpath -ldflags="-s -w -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=example.com:20055" -o bin/user-client-linux-arm64 ./cmd/user-client
```

### 3. 编译产物说明
- `bin/relay-server.exe` & `bin/relay-server-linux-amd64`：公网中继服务器
- `bin/dev-client.exe` & `bin/dev-client-linux-amd64`：本地开发者 MCP Stdio 客户端
- `bin/user-client.exe`、`bin/user-client-windows-386.exe`（Win7+）、`bin/user-client-linux-amd64`、`bin/user-client-linux-arm64`：目标机受控客户端（支持 GUI 与 CLI）
- `releases/user-client-windows-amd64.exe`、`releases/user-client-windows-386.exe`、`releases/user-client-linux-amd64`、`releases/user-client-linux-arm64`：供 `relay-server` 分发的自动更新包（将新版二进制放入服务端 `./releases/` 目录即可让老客户端在启动与重连前自动热替换升级，无需重启服务端）

---

## 全部 CLI Flags 与环境变量参考

### 1. 公网服务器 (`relay-server`)

| CLI Flag | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-addr <string>` | `RELAY_ADDR` | `:20055` | 服务端监听地址与端口（如 `:20055` 或 `0.0.0.0:20055`） |
| `-user-key <string>` | `RELAY_USER_KEY` | `default-user-key` | 供目标机 `user-client` 连接鉴权的固定密钥 |
| `-dev-key <string>` | `RELAY_DEV_KEY` | `default-dev-key` | 供开发者 `dev-client` 与服务端 MCP 接口鉴权的固定密钥 |
| `-releases-dir <path>` | `RELAY_RELEASES_DIR` | `./releases` | 存放最新用户客户端二进制包（供客户端自动更新）的目录；自动将 `i386`/`x86` 映射为 `386`、`windows7`/`win7` 映射为 `windows` |
| `-tls-cert <path>` | `RELAY_TLS_CERT` | `""` | SSL/TLS 证书文件路径（如 `fullchain.pem`）；默认安全模式下必填 |
| `-tls-key <path>` | `RELAY_TLS_KEY` | `""` | SSL/TLS 私钥文件路径（如 `privkey.pem`）；默认安全模式下必填 |
| `-insecure` | `RELAY_INSECURE=1` | `false` | 允许无正规 SSL 证书运行（自动生成内存自签名 TLS 证书，并通过首字节嗅探同时兼容加密 `wss://`/`https://` 与明文 `ws://`/`http://`） |
| `-mcp` | `RELAY_ENABLE_MCP=1` | `false` | 开启服务端 HTTP/SSE MCP 端点（`/mcp`、`/sse`、`/messages`）与 OAuth 2.0 兼容层 |

### 2. 用户客户端 (`user-client`)

> 无参数直接运行时默认进入 **GUI 模式**：Windows 为原生 Win32 窗口，Linux 为零依赖本地回环 Web UI（配合心跳看门狗与非阻塞错误弹窗；若 `DISPLAY`/`WAYLAND_DISPLAY` 均未设置且传入 `-key` 则自动回退无界面运行）。传入 `-cli` 时直接进入无界面命令行模式。

| CLI Flag | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-cli` | — | `false` | 以无界面 CLI 模式运行（不弹窗，适合后台服务或脚本启动） |
| `-gui` | — | `false` | 强制以 GUI 窗口模式运行 |
| `-server <string>` | `MCP_SERVER_ADDR` | 内置 `DefaultServerAddr` (`127.0.0.1:20055`) | 公网中继服务器地址（未写端口时自动补全 `:20055`；仅支持通过 CLI 或环境变量覆盖，GUI 界面不可改） |
| `-key <string>` | `MCP_USER_KEY` | 本地已保存密钥（CLI 兜底 `default-user-key`） | 用户客户端连接密钥（与服务端的 `-user-key` 对应；仅在启用 `-save` 或勾选 GUI **Save Login State** 时持久化） |
| `-id <string>` | `MCP_MACHINE_ID` | `<hostname>-<随机后缀>`（启用保存时持久化） | 自定义当前目标机的 `machine_id` 标识 |
| `-shell <string>` | `MCP_SHELL` | 自动按 Fallback 链探测 | 手动指定默认 Shell（如 `pwsh`, `powershell`, `cmd`, `bash`, `sh`, `ash` 或绝对路径）。未指定时 Windows 按 `pwsh -> powershell -> cmd`、Linux 按 `bash -> sh -> ash` 自动探测 |
| `-workdir <path>` | `MCP_WORKDIR` | 当前启动目录 | 初始默认工作目录 |
| `-save` | — | 本地已有配置时为 `true`，否则 `false` | CLI 模式下是否将配置写入本地配置文件（`user-client.json`） |
| `-no-update` | — | `false` | 禁用启动与重连时的自动更新检查 |
| `-insecure` | `MCP_INSECURE=1` | `false` | 强制跳过 SSL 证书链校验并允许非加密连接（仅 CLI 可配，GUI 不暴露此选项；自更新重启时通过 `MCP_HANDOFF_STATE` 临时传递并在子进程启动前销毁） |

### 3. 开发者客户端 (`dev-client`)

> 作为本地 MCP Stdio Server 运行，由 OpenCode 等 AI 工具自动拉起并通过标准输入输出（`stdio`）通信。

| CLI Flag | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-server <string>` | `MCP_SERVER_ADDR` | 内置 `DefaultServerAddr` (`127.0.0.1:20055`) | 公网中继服务器地址（未写端口时自动补全 `:20055`） |
| `-key <string>` | `MCP_DEV_KEY` | `default-dev-key` | 开发者客户端连接密钥（与服务端的 `-dev-key` 对应） |
| `-machine <string>` | `MCP_DEFAULT_MACHINE` | `""`（自动选中唯一在线机器） | 默认操作的目标机 `machine_id` 或主机名 |
| `-insecure` | `MCP_INSECURE=1` | `false` | 强制跳过 SSL 证书链校验并允许非加密连接 |

---

## 快速使用指南

### 1. 启动公网服务器 (`relay-server`)

- **默认安全模式（强制 HTTPS/WSS，需提供正规 SSL 证书）**：
  ```bash
  ./relay-server -addr :20055 -user-key "my-user-secret" -dev-key "my-dev-secret" -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem
  ```
  > 默认情况下，客户端（`user-client` 与 `dev-client`）强制将所有连接重写为 `https://` 与 `wss://`，绝不回退明文（防降级攻击），并严格校验系统根证书链（拒绝自签名或中间人伪造证书）。

- **手动跳过 SSL 校验模式（服务端与客户端均加上 `-insecure` flag）**：
  当你手动配置、在纯 IP 或内网环境下不想配证书时，可以在服务端和客户端同时加上 `-insecure`：
  ```bash
  # 服务端（自动生成内存自签名 TLS 证书并同时兼容明文 WS/HTTP）
  ./relay-server -addr :20055 -user-key "my-user-secret" -dev-key "my-dev-secret" -insecure

  # 用户客户端（CLI 手动加 -insecure 跳过证书校验）
  ./user-client -cli -insecure -server "1.2.3.4:20055" -key "my-user-secret"

  # 开发者客户端（加 -insecure 跳过证书校验）
  ./dev-client -insecure -server "1.2.3.4:20055" -key "my-dev-secret"
  ```

- **可选：启用服务端 HTTP/SSE MCP 端点 (`-mcp`)**：
  如果需要让支持远程 MCP（Streamable HTTP / Legacy SSE / OAuth 2.0）的客户端直接连接 `relay-server`（无需在本地运行 `dev-client`），可附加 `-mcp`：
  ```bash
  ./relay-server -addr :20055 -user-key "my-user-secret" -dev-key "my-dev-secret" -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem -mcp
  ```
  - **Streamable HTTP**：`https://example.com:20055/mcp`
  - **Legacy HTTP+SSE**：`https://example.com:20055/sse`（消息投递至 `/messages`）
  - **鉴权方式**：支持 `X-Dev-Key: <dev-key>`、`Authorization: Bearer <dev-key>`、`?key=<dev-key>`，或使用内置 OAuth 2.0 端点（`/.well-known/oauth-protected-resource`、`/.well-known/oauth-authorization-server`、`/oauth/authorize`、`/oauth/token`，`client_secret` 校验 `-dev-key`）。
  - **工具范围差异**：服务端 MCP 模式仅暴露 12 个纯远程控制工具；涉及本地磁盘与本地端口的 `upload`、`download`、`forwarding` 需通过本地 `dev-client` 使用。

### 2. 在目标机器运行用户客户端 (`user-client`)

- **GUI 模式（默认，适合直接发给不懂技术的用户双击运行）**：
  直接双击 `user-client.exe`（Windows 下无黑框），在窗口填入连接密钥，点击 **Connect**。
  - 若勾选 **Save Login State**，配置将保存在本地用户目录（`%APPDATA%\mcp-remoteaccess\user-client.json` 或 `~/.config/mcp-remoteaccess/user-client.json`），且下次打开会自动勾选；取消勾选时会自动删除配置文件并清理空配置目录，不留磁盘痕迹。
  - **自动重连与错误弹窗语义**：仅网络波动、超时、DNS 暂时失败或服务端 `5xx` / `1006` / `1001` 等异常断线会自动重连；遇到连接拒绝（`ECONNREFUSED`）、HTTP `4xx` / 密钥错误、TLS 校验失败或同 `machine_id` 被新会话顶下线（`4001`）等非预期错误时，会立即停止重连并弹出错误提示框。
  - 自更新重启期间会通过临时环境变量 `MCP_HANDOFF_STATE` 交接当前密钥与运行时选项，启动后立即销毁，不会写入磁盘，也不会泄露给 `shell` 子进程。
- **CLI 模式（高度自定义）**：
  ```bash
  ./user-client -cli -server "example.com:20055" -key "my-user-secret" -shell pwsh -id "office-pc" -workdir "C:\Projects"
  ```

### 3. 在本地 AI 工具（如 OpenCode）配置开发者客户端 (`dev-client`)

在 OpenCode 的 MCP 配置中添加：

```json
{
  "mcp": {
    "remote-access": {
      "type": "local",
      "command": [
        "C:\\Files\\Projects\\mcp-remoteacess\\bin\\dev-client.exe",
        "-server",
        "example.com:20055",
        "-key",
        "my-dev-secret"
      ]
    }
  }
}
```

---

## 提供的 15 个 MCP 工具

> 注：通过本地 `dev-client` 接入时提供全部 15 个工具；通过 `relay-server -mcp` 远程直连时提供前 12 个远程工具（不含依赖开发者本地机器的 `upload`、`download`、`forwarding`）。

1. `list_machines`：列出所有在线目标机及其 OS、架构、当前 Shell、可用 Shell、工作目录、后台任务数。
2. `switch_machine`：切换当前操作的目标机并返回环境快照。
3. `switch_workdir`：切换目标机默认工作目录。
4. `read`：读取远程文件（带 1-based 行号分页）或目录列表；支持自动探测 UTF-8、UTF-8-BOM、UTF-16LE/BE 及系统本地编码（如 GBK/GB18030、Big5、Shift-JIS、EUC-KR、Windows-1252），亦可通过 `encoding` 参数手动指定。
5. `write`：创建或覆盖远程文件（自动创建父目录）；默认输出 UTF-8，支持通过 `encoding` 指定目标编码（不允许设为 `"auto"`）。
6. `edit`：精准修改远程文件（支持精确匹配、Unicode 标点归一化、忽略行尾空白三层匹配，自动保留原文件编码、BOM 与 CRLF/LF 换行符）。
7. `glob`：远程通配符搜索文件路径（支持 `**` 与 `{a,b}`）。
8. `grep`：远程正则或字面量搜索文件内容（支持 `include` 过滤，自动适配多编码文件或指定 `encoding`）。
9. `shell`：执行远程命令（默认 30s 超时自动转后台 Job；`timeout: 0` 立即转后台并等待最多 3s 首包输出；支持单次指定 `shell`；自动将目标机非 UTF-8 控制台输出如 Windows GBK/CP936 流式解码为 UTF-8，也支持 `encoding` 覆盖）。
10. `job_list`：列出远程后台任务。
11. `job_read`：增量字节游标（`next_offset`）读取后台任务输出，支持 `stalled` 检测。
12. `job_kill`：强制终止远程后台任务及其子进程树。

> **注**：命令与后台任务相关工具（9~12）参考了 [**`opencode-background-jobs`**](https://github.com/A8Cl233395/opencode-background-jobs)，在远程机器上同样支持超时自动转后台、断线不丢任务、增量拉取输出和进程树清理。

13. `upload`：从开发者本机流式上传文件或整个目录到远程目标机（含 SHA256 校验，仅 `dev-client` 可用）。
14. `download`：从远程目标机流式下载文件或整个目录到开发者本机（含 SHA256 校验，仅 `dev-client` 可用）。
15. `forwarding`：统一管理双向 TCP/UDP 端口转发（`action: "add" | "remove" | "list"`，`protocol: "tcp" | "udp"`，支持 `remote_to_local` 与 `local_to_remote`，仅 `dev-client` 可用）。

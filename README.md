# MCP Remote Access (`mcp-remote-access`)

专为 AI Agent（OpenCode / Claude Desktop 等）设计的免配置远程机器控制、文件流传输与双向端口穿透套件。

## 架构概览

```text
[AI Agent (OpenCode)]
       │  (MCP Stdio JSON-RPC 2.0)
       ▼
[开发者客户端 (dev-client)]  ◄── 本地磁盘流式上传/下载 (upload / download)
       │                     ◄── 本地 TCP/UDP 端口双向转发 (forwarding)
       │  (WebSocket + Yamux, X-Dev-Key)
       ▼
[公网服务器 (relay-server)]  ── 托管 ./releases/ 供远古客户端自动更新
       ▲
       │  (WebSocket + Yamux, X-User-Key)
[用户客户端 (user-client)]   ── 目标机执行 (Shell/File/Job/PortForward/防休眠/GUI+CLI)
```

---

## 编译选项与构建说明

项目全程使用纯 Go（`CGO_ENABLED=0`）构建，无任何外部动态库依赖，支持在单机上一键交叉编译 Windows 与 Linux 双平台单文件可执行程序。

### 1. 使用 `build.ps1` 一键编译

- **自动从 `.env` 读取公网服务器地址（推荐）**：
  复制 `.env.example` 为 `.env`，填入你的中继服务器地址：
  ```env
  DEFAULT_SERVER=example.com:20055
  ```
  然后直接执行脚本，脚本会自动读取并注入：
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

### 2. 手动 `go build` 与 `-ldflags` 编译选项详解

如果你需要手动单独编译某一个组件或架构，可使用以下编译参数：

| 编译参数 / 环境变量 | 作用说明 |
| :--- | :--- |
| `CGO_ENABLED=0` | 禁用 CGo，产出零依赖纯静态单文件二进制（兼容各 Linux 发行版与 Windows） |
| `GOOS=windows` / `GOOS=linux` | 目标操作系统 |
| `GOARCH=amd64` / `GOARCH=arm64` | 目标 CPU 架构 |
| `-trimpath` | 移除编译机本地源码绝对路径信息 |
| `-ldflags="-s -w"` | 剥离符号表与 DWARF 调试信息，减小可执行文件体积 |
| `-ldflags="-H=windowsgui"` | **仅 Windows `user-client.exe` 使用**：编译为 Windows GUI 子系统，双击运行时彻底不弹黑色 CMD 控制台窗口（CLI 模式下仍会自动挂载父终端输出） |
| `-ldflags="-X mcp-remoteaccess/internal/protocol.DefaultServerAddr=..."` | 编译期覆写客户端内置的默认中继服务器地址 |

手动编译示例（PowerShell）：
```powershell
# 编译 Windows 用户客户端（无黑框 + 注入默认公网地址）
$env:CGO_ENABLED="0"; $env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w -H=windowsgui -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=example.com:20055" -o bin/user-client.exe ./cmd/user-client

# 编译 Linux 用户客户端
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w -X mcp-remoteaccess/internal/protocol.DefaultServerAddr=example.com:20055" -o bin/user-client-linux-amd64 ./cmd/user-client
```

### 3. 编译产物说明
- `bin/relay-server.exe` & `bin/relay-server-linux-amd64`：公网中继服务器
- `bin/dev-client.exe` & `bin/dev-client-linux-amd64`：本地开发者 MCP 客户端
- `bin/user-client.exe` & `bin/user-client-linux-amd64`：目标机受控客户端（支持 GUI 与 CLI）
- `releases/user-client-windows-amd64.exe` & `releases/user-client-linux-amd64`：供 `relay-server` 分发的自动更新包（将新版二进制放入服务端 `./releases/` 目录即可让所有老客户端自动热替换升级，无需重启服务端）

---

## 全部 CLI Flags 与环境变量参考

### 1. 公网服务器 (`relay-server`)

| CLI Flag | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-addr <string>` | `RELAY_ADDR` | `:20055` | 服务端监听地址与端口（如 `:20055` 或 `0.0.0.0:20055`） |
| `-user-key <string>` | `RELAY_USER_KEY` | `default-user-key` | 供目标机 `user-client` 连接鉴权的固定密钥 |
| `-dev-key <string>` | `RELAY_DEV_KEY` | `default-dev-key` | 供开发者 `dev-client` 连接鉴权的固定密钥 |
| `-releases-dir <path>` | `RELAY_RELEASES_DIR` | `./releases` | 存放最新用户客户端二进制包（供远古客户端自动更新）的目录 |
| `-tls-cert <path>` | `RELAY_TLS_CERT` | `""` | SSL/TLS 证书文件路径（如 `fullchain.pem`）；默认安全模式下必填 |
| `-tls-key <path>` | `RELAY_TLS_KEY` | `""` | SSL/TLS 私钥文件路径（如 `privkey.pem`）；默认安全模式下必填 |
| `-insecure` | `RELAY_INSECURE=1` | `false` | 允许无正规 SSL 证书运行（自动生成内存自签名 TLS 证书，并通过首字节嗅探同时兼容加密 `wss://`/`https://` 与明文 `ws://`/`http://`） |

### 2. 用户客户端 (`user-client`)

> 无参数直接双击时默认进入 **GUI 模式**（仅显示连接密钥输入框与运行状态，锁定使用内置默认服务器地址）；传入 `-cli` 时进入无界面命令行模式。

| CLI Flag | 对应环境变量 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-cli` | — | `false` | 以无界面 CLI 模式运行（不弹窗，适合后台服务或脚本启动） |
| `-gui` | — | `false` | 强制以 GUI 窗口模式运行 |
| `-server <string>` | `MCP_SERVER_ADDR` | 内置 `DefaultServerAddr` (`127.0.0.1:20055`) | 公网中继服务器地址（未写端口时自动补全 `:20055`；仅支持通过 CLI 或环境变量覆盖，GUI 界面不可改） |
| `-key <string>` | `MCP_USER_KEY` | 本地已保存密钥（CLI 兜底 `default-user-key`） | 用户客户端连接密钥（与服务端的 `-user-key` 对应，连接成功后自动持久化到本地配置） |
| `-id <string>` | `MCP_MACHINE_ID` | `<hostname>-<随机后缀>`（持久化） | 自定义当前目标机的 `machine_id` 标识 |
| `-shell <string>` | `MCP_SHELL` | 自动按 Fallback 链探测 | 手动指定默认 Shell（如 `pwsh`, `powershell`, `cmd`, `bash`, `sh`, `ash` 或绝对路径）。未指定时 Windows 按 `pwsh -> powershell -> cmd`、Linux 按 `bash -> sh -> ash` 自动探测 |
| `-workdir <path>` | `MCP_WORKDIR` | 当前启动目录 | 初始默认工作目录 |
| `-no-update` | — | `false` | 禁用启动与重连时的自动更新检查 |
| `-insecure` | `MCP_INSECURE=1` | `false` | 强制跳过 SSL 证书链校验并允许非加密连接（仅 CLI 可配，自更新重启时会自动保留此参数） |

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

### 2. 在目标机器运行用户客户端 (`user-client`)

- **GUI 模式（默认，适合直接发给不懂技术的用户双击运行）**：
  直接双击 `user-client.exe`（Windows 下无黑框），在窗口填入连接密钥，点击 **Connect**。若勾选 **Save Login State**，配置将保存在本地用户目录（`%APPDATA%\mcp-remoteaccess\user-client.json` 或 `~/.config/mcp-remoteaccess/user-client.json`）以便后续自动读取；默认取消勾选，取消勾选状态下不会在磁盘保存任何文件。
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

1. `list_machines`：列出所有在线目标机及其 OS、架构、当前 Shell、可用 Shell、工作目录、后台任务数。
2. `switch_machine`：切换当前操作的目标机并返回环境快照。
3. `switch_workdir`：切换目标机默认工作目录。
4. `read`：读取远程文件（带 1-based 行号分页）或目录列表。
5. `write`：创建或覆盖远程文件（自动创建父目录）。
6. `edit`：精准修改远程文件（支持精确匹配、Unicode 标点归一化、忽略行尾空白三层匹配，自动保持 CRLF/LF）。
7. `glob`：远程通配符搜索文件路径（支持 `**` 与 `{a,b}`）。
8. `grep`：远程正则或字面量搜索文件内容（支持 `include` 过滤）。
9. `shell`：执行远程命令（默认 30s 超时自动转后台 Job；`timeout: 0` 立即转后台并等待最多 3s 首包输出；支持单次指定 `shell`）。
10. `job_list`：列出远程后台任务。
11. `job_read`：增量字节游标（`next_offset`）读取后台任务输出，支持 `stalled` 检测。
12. `job_kill`：强制终止远程后台任务及其子进程树。

> **注**：命令与后台任务相关工具（9~12）参考了 [**`opencode-background-jobs`**](https://github.com/A8Cl233395/opencode-background-jobs)，在远程机器上同样支持超时自动转后台、断线不丢任务、增量拉取输出和进程树清理。

13. `upload`：从开发者本机流式上传文件或整个目录到远程目标机（含 SHA256 校验）。
14. `download`：从远程目标机流式下载文件或整个目录到开发者本机（含 SHA256 校验）。
15. `forwarding`：统一管理双向 TCP/UDP 端口转发（`action: "add" | "remove" | "list"`，`protocol: "tcp" | "udp"`，支持 `remote_to_local` 与 `local_to_remote`）。

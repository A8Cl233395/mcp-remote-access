# MCP 远程控制与穿透套件 (`mcp-remoteaccess`) 任务书与技术规范

> 本文件为项目核心设计、通信协议、MCP 工具契约与开发里程碑的唯一权威文档（Single Source of Truth），用于指导全量开发与后续接力维护。

---

## 1. 项目背景与核心目标

解决在家通过本地 AI Agent（如 OpenCode）操作外部远程机器时，手动配置目标机 `sshd`、反向隧道、SSH 密钥过于繁琐的痛点。

### 核心架构
采用 **三端分离 + 公网无状态中继 + 多路流复用** 架构：
1. **用户客户端 (`user-client`)**：运行在远程目标机器上（Windows / Linux），扮演“远程木马/受控端”角色。
   - **傻瓜式体验**：无任何审批弹窗；提供极简 GUI（填密钥即连，配置自动持久化）与高可配 CLI 模式。
   - **远古客户端自更新**：连接前通过独立于主协议的固定 HTTP 接口校验 SHA256 并自动热替换重启。
   - **防休眠（Wake Lock）**：连接期间阻止目标操作系统进入空闲睡眠。
   - **长任务解耦**：后台任务（Background Jobs）生命周期与网络连接解耦，闪断重连后任务不丢、输出不断。
2. **公网服务器 (`relay-server`)**：运行在公网 VPS 上。
   - **纯转发中继**：通过固定双密钥（`dev_key` / `user_key`）鉴权，负责桥接 `dev-client` 与目标 `user-client` 的多路复用流（Yamux Stream）。
   - **更新分发源**：静态托管 `./releases/` 目录下的最新客户端二进制，提供版本清单与流式下载。
3. **开发者客户端 (`dev-client`)**：运行在开发者本机。
   - 对内通过标准 **MCP (Model Context Protocol, Stdio JSON-RPC 2.0)** 向本地 AI 暴露 15 个工具。
   - 对外连接 `relay-server`，直接操作开发者本机磁盘（用于 `upload`/`download`）与本机网卡端口（用于 `forwarding` 双向端口透传）。

---

## 2. 参考基准实现

1. **基础文件工具 (`read`, `write`, `edit`, `glob`, `grep`)**
   - 对齐 OpenCode 核心实现：`packages/core/src/tool/plugin/{read,write,edit,glob,grep}.ts` 及 `read-filesystem.ts`。
   - 保持一致的参数定义、行号格式（`<line>: <content>`）、分页截断提示、`edit` 的三层容错匹配（精确匹配 -> Unicode 标点/空格归一化 -> 行尾空白忽略）与换行符（CRLF/LF）保持逻辑。
2. **命令与后台任务工具 (`shell`, `job_list`, `job_read`, `job_kill`)**
   - 对齐 OpenCode 后台任务插件：`plugin/background-jobs/lib/{tools,jobs,describe,shell,spawn,truncate,config}.ts`。
   - 保持一致的前台等待转后台机制（默认 30s 超时自动转后台，`timeout: 0` 立即转后台并等待最多 3s 首包输出）、增量字节游标（`next_offset`）、静默卡死检测（`stalled`）及进程树清理（Windows `taskkill /f /t` / Linux `-pgid`）。

---

## 3. 通信协议规范

### 3.1 独立自更新协议（HTTP 1.1，永不破坏兼容）
- **检查更新**：`GET /update/manifest?os={windows|linux}&arch={amd64|arm64}`
  - 鉴权：Header `X-User-Key: <user_key>` 或 Query `key=<user_key>`
  - 服务端查找 `./releases/user-client-{os}-{arch}{.exe}`
  - 响应 `200 OK` JSON：
    ```json
    {
      "available": true,
      "filename": "user-client-windows-amd64.exe",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      "size": 5242880
    }
    ```
    （若服务端 `releases/` 下未放置对应平台包，返回 `{"available": false}`，客户端跳过更新直接连接）。
- **下载更新**：`GET /update/binary?os={windows|linux}&arch={amd64|arm64}`
  - 响应：二进制流（`application/octet-stream`）。
- **客户端自替换流程**：
  1. 启动时尝试删除残留的 `<exe>.old`。
  2. 计算当前自身可执行文件 SHA256，请求 `/update/manifest`。
  3. 若 `available == true` 且 `sha256` 不同：
     - 下载至 `<exe>.new` 并校验大小与 SHA256。
     - `chmod 0755 <exe>.new`
     - 将自身 `<exe>` 重命名为 `<exe>.old`（Windows 允许重命名运行中的 `.exe`）。
     - 将 `<exe>.new` 重命名为 `<exe>`。
     - 使用原启动参数与环境变量启动新的 `<exe>` 进程，当前进程立即退出。

### 3.2 主通信与多路复用协议（WebSocket + Yamux）
1. **用户客户端接入**：
   - `GET /ws/user`（Header `X-User-Key: <user_key>`）
   - WebSocket 握手成功后，底层包装为 `yamux.Server`（`user-client` 作为 Yamux Server 接受来自 `dev-client` 的流，同时也可通过控制流向 `dev-client` 发起反向端口转发请求）。
   - 连接建立后，`user-client` 在首个控制握手包中上报 `MachineInfo`（`machine_id`, `hostname`, `os`, `arch`, `user`, `is_admin`, `workdir`, `shell`, `available_shells`, `version`）。
2. **开发者客户端接入**：
   - `GET /ws/dev`（Header `X-Dev-Key: <dev_key>`）
   - WebSocket 握手成功后，底层包装为 `yamux.Session`。
3. **Yamux Stream 帧头协议**：
   每个新打开的 Yamux Stream 先发送 1 行以 `\n` 结尾的 JSON Header：
   ```json
   {
     "stream_type": "rpc" | "file_upload" | "file_download" | "tcp_connect" | "reverse_listen" | "reverse_conn",
     "target_machine_id": "machine-xxx",
     "meta": { ... }
   }
   ```
   - `rpc`：携带单次或长连接 JSON-RPC 调用，转发至目标 `user-client` 执行工具。
   - `file_upload`：Header 校验通过后，后续字节流为 tar.gz 或原始文件流，直接写入目标机磁盘。
   - `file_download`：Header 指定远程路径，目标机直接将文件/目录流式写回 Stream。
   - `tcp_connect`：用于 `remote_to_local`（正向穿透）或 `local_to_remote` 的单条 TCP 连接桥接，Header 含 `"remote_addr": "127.0.0.1:3000"`，握手成功后双方直接 `io.Copy` 裸字节流。

---

## 4. MCP 15 个工具完整契约

`dev-client` 通过 MCP Stdio 暴露以下 15 个工具：

| 序号 | 工具名 | 参数 Schema | 核心行为与返回格式 |
| :--- | :--- | :--- | :--- |
| 1 | `list_machines` | `{}` | 列出所有在线目标机器，标记当前选中的 `active` 机器，包含 OS、架构、用户名、当前工作目录、当前 Shell、可用 Shell 列表。 |
| 2 | `switch_machine` | `{ machine_id: string }` | 切换当前操作的目标机（支持精确 ID 或主机名匹配），返回该机器完整环境快照与运行中任务数。 |
| 3 | `switch_workdir` | `{ path: string }` | 修改当前目标机的默认工作目录（支持相对路径、`~` 展开），校验目录存在并返回绝对路径。 |
| 4 | `read` | `{ path: string, offset?: int, limit?: int }` | 读取远程文件或目录。目录按字典序（目录优先）分页返回；文本文件带 1-based 行号 `<line>: <content>` 返回，单页上限 2000 行 / 50KB。 |
| 5 | `write` | `{ path: string, content: string }` | 创建或覆盖远程文件，自动创建父目录。返回 `Created/Wrote file successfully: <path>`。 |
| 6 | `edit` | `{ path: string, oldString: string, newString: string, replaceAll?: bool }` | 精准替换远程文件内容，支持三层匹配（精确/Unicode归一化/忽略行尾空格）与 CRLF/LF 自动保持，未命中时提供 `missHint` 邻近行提示。 |
| 7 | `glob` | `{ pattern: string, path?: string, hidden?: bool, limit?: int }` | 远程通配符搜文件（支持 `**`、`{a,b}`），默认忽略 `.git`/`node_modules` 及隐藏文件（除非 `hidden: true`），默认上限 100 条。 |
| 8 | `grep` | `{ pattern: string, path?: string, include?: string, literal?: bool, caseSensitive?: bool, limit?: int }` | 远程正则或字面量搜索文件内容，跳过二进制文件，返回匹配文件路径、行号与行内容预览。 |
| 9 | `shell` | `{ command: string, workdir?: string, shell?: string, timeout?: int }` | 在目标机执行命令。默认等待 30000ms；超时未结束或 `timeout: 0`（等待 3000ms 首包）自动转为后台 Job 并返回 `job #id` 与已产生输出。 |
| 10 | `job_list` | `{}` | 列出当前目标机所有后台任务（`#id status elapsed command (code X)`）。 |
| 11 | `job_read` | `{ id: string\|int, offset?: int, timeout?: int }` | 增量读取后台任务输出，维护 `next_offset` 游标；无新输出时最多阻塞等待 `timeout`（默认 5000ms），支持 `stalled` 提示。 |
| 12 | `job_kill` | `{ id: string\|int }` | 强制终止目标机后台任务及其整个进程树，返回最终状态与未读输出。 |
| 13 | `upload` | `{ local_path: string, remote_path: string }` | 将开发者本机文件或整个目录流式上传至目标机指定路径，返回传输字节数、文件数、SHA256 与耗时。 |
| 14 | `download` | `{ remote_path: string, local_path: string }` | 将目标机文件或整个目录流式下载至开发者本机指定路径，返回传输字节数、文件数、SHA256 与耗时。 |
| 15 | `forwarding` | `{ action: "add"\|"remove"\|"list", id?: string, direction?: "remote_to_local"\|"local_to_remote", local_addr?: string, remote_addr?: string }` | 管理双向 TCP 端口转发隧道。支持本地监听映射远端端口（`remote_to_local`）及远端监听映射本地端口（`local_to_remote`）。 |

---

## 5. Shell 自动探测与 Fallback 规范

`user-client` 启动时探测系统 PATH 及常用路径：
- **Windows Fallback 路线**：`pwsh` -> `powershell` -> `cmd`（同时额外探测 Git `bash` 供可选切换）
  - 执行参数：
    - `pwsh` / `powershell`：`["-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command]`
    - `cmd`：`["/d", "/s", "/c", command]`
    - `bash` / `sh` / `ash`：`["-c", command]`
- **Linux Fallback 路线**：`bash` -> `sh` -> `ash`
  - 执行参数：`["-c", command]`
- **手动覆盖**：
  - 用户客户端 CLI 启动参数：`--shell pwsh`（或任意可执行文件路径）。
  - MCP `shell` 工具参数：单次传入 `shell` 字段可临时使用指定 Shell。

---

## 6. 目录结构与模块划分

```text
C:\Files\Projects\mcp-remoteacess\
├── TASK_SPEC.md                  # 本任务书
├── README.md                     # 使用说明与配置文档
├── build.ps1                     # 一键跨平台编译脚本 (Windows + Linux)
├── go.mod
├── go.sum
├── cmd/
│   ├── relay-server/main.go      # 公网中继服务器入口
│   ├── user-client/main.go       # 目标机客户端入口 (GUI + CLI)
│   └── dev-client/main.go        # 开发者客户端入口 (MCP Stdio Server)
├── internal/
│   ├── protocol/                 # 协议结构、Yamux 帧头、常量定义
│   ├── relay/                    # 中继服务器实现、连接注册表、/update 更新分发
│   ├── agent/                    # 用户客户端实现：
│   │   ├── client.go             # WebSocket + Yamux 接入与重连循环、RPC 分发
│   │   ├── shell.go              # Shell 探测链 (pwsh->powershell->cmd / bash->sh->ash)
│   │   ├── jobs.go               # 解耦式后台任务管理器 (对齐 background-jobs)
│   │   ├── fs.go                 # read, write, edit, glob, grep 与流式文件传输
│   │   ├── power_windows.go      # Windows SetThreadExecutionState 防休眠
│   │   ├── power_linux.go        # Linux 防休眠
│   │   ├── updater.go            # 独立 HTTP 自更新与 .old 替换重启
│   │   ├── gui_windows.go        # Windows 原生 Win32 GUI (零 CGo 依赖)
│   │   └── gui_linux.go          # Linux GUI (原生弹窗/本地 Web 视图 + CLI 自动回退)
│   └── dev/                      # 开发者客户端实现：
│       ├── session.go            # 连接中继、多机选择、RPC 调用、upload/download/forwarding
│       └── mcp.go                # MCP JSON-RPC 2.0 Stdio 服务端与 15 个工具处理器
└── test/
    └── e2e_test.go               # 全链路自动化集成测试（覆盖 15 个工具、双向转发、自更新）
```

---

## 7. 开发里程碑 Checklist

- [x] **M0：任务书制定与源码调研**（对齐 OpenCode 核心工具与 `background-jobs` 插件逻辑）
- [x] **M1：基础工程初始化与协议层 (`internal/protocol`)**
- [x] **M2：公网中继服务器 (`internal/relay`, `cmd/relay-server`)**
- [x] **M3：用户客户端核心执行器 (`internal/agent`: Shell、Jobs、FS、防休眠、自更新、GUI/CLI)**
- [x] **M4：开发者客户端与 MCP 服务端 (`internal/dev`, `cmd/dev-client`)**
- [x] **M5：跨平台编译脚本 (`build.ps1`) 与全链路 E2E 自动化测试 (`test/e2e_test.go`)**

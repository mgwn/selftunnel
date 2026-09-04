# selftunnel —— 软件规格说明书 v0.1.0·原生客户端版（供 AI 编码使用）

> 本 Spec 自包含：读完即可生成完整工程。所有"必须（MUST）"为验收项，"可选（OPTIONAL）"可后置。
> 目标读者：代码生成 AI 或全栈工程师。禁止引入本 Spec 未列出的第三方依赖。

## 0. 项目概述

实现一个多租户反向隧道系统：

- **服务器**（`selftunnel-server`，Go 单二进制）：无 Web UI，只提供两个端点——隧道接入端点（`/ws/tunnel`，WSS）与 REST 中转接口（`ANY /t/{tunnelID}/*`）。支持任意多个原生客户端同时接入，按 tunnelID 分发请求。
- **客户端**（原生 Go 二进制，**无需管理员权限**）：
  - `selftunnel-client`：命令行客户端，适合服务器/容器/脚本场景；
  - `selftunnel-client-gui`：Fyne 跨平台桌面客户端，带状态、配置、日志窗口；
  - 两者主动出站建立 WSS 长连接；连接成功后服务器分配（或确认续用/自定义）一个 **8 位字母数字 tunnelID**；
  - 收到中转请求后转发到用户配置的**真实内网目标地址**，响应分块回传。
- 对 API 调用方而言，`https://server/t/{tunnelID}/{path}` 就是一个普通 REST API，支持**全部 HTTP 方法、任意 Header、任意大小 Body、流式响应（含 SSE）**。
- 防休眠：隧道在线期间阻止系统因空闲进入睡眠，断开/退出后恢复默认电源策略。
- 调用侧不做鉴权（明确非目标）；tunnelID + 接入密钥仅用于防 ID 抢注（见 §3.4）。

## 1. 技术栈（固定，不得更换）

### 服务端

| 项 | 选型 | 说明 |
|---|---|---|
| 语言 | Go ≥ 1.25 | 单二进制 |
| WebSocket | `github.com/gorilla/websocket` | `/ws/tunnel` 端点 |
| HTTP 路由 | Go 1.22+ 标准库 `http.ServeMux` 模式路由 | 不引第三方 router |
| 配置持久化 | 单个 JSON 文件（`data/tunnels.json`），写时整文件覆盖 | 不引入数据库 |
| 日志 | 标准库 `log/slog` | |
| 其他 | 仅 Go 标准库 + gorilla/websocket | |

### 客户端

| 项 | 选型 | 说明 |
|---|---|---|
| 语言 | Go ≥ 1.25 | 单二进制，跨平台 |
| 命令行入口 | `cmd/selftunnel-client` | 标准库 `flag` 解析参数 |
| GUI 入口 | `cmd/selftunnel-client-gui` | Fyne v2 (`fyne.io/fyne/v2`) |
| 隧道 | `github.com/gorilla/websocket` | wss\/\/server/ws/tunnel |
| 转发 | 标准库 `net/http` | |
| 配置持久化 | 单个 JSON 文件（`config.json`），写时整文件覆盖 | 与二进制同目录 |
| 日志 | 标准库 `log/slog`（CLI）；回调写入 GUI 文本框 | |
| 防休眠 | 平台原生 API（见 §3.7） | 无第三方依赖 |

### 依赖白名单

- `github.com/gorilla/websocket`
- `fyne.io/fyne/v2`（仅 GUI 客户端）
- Go 标准库

## 2. 工程结构

Go 模块位于仓库根目录（标准开源布局）；规格说明文档（本文档，中英双语）位于 `specs/`。

```
（仓库根目录）
├── go.mod
├── build.sh                         # 一键构建：服务端 + 跨平台 CLI + 本机 GUI
├── Makefile                         # fmt / vet / test / build 便捷目标
├── specs/                           # 规格说明——事实来源（英文 + 中文）
├── test/e2e/                        # 本地回环端到端测试（真实 server + client）
├── tests/                           # 测试策略：pipeline 门槛脚本（冒烟/覆盖率/交叉编译）+ AI 测试计划
├── scripts/hooks/                   # git 钩子：commit-msg 校验 + pre-commit 检查
├── cmd/
│   ├── selftunnel-server/main.go         # 服务端入口
│   ├── selftunnel-client/main.go         # 命令行客户端入口
│   └── selftunnel-client-gui/main.go     # Fyne GUI 客户端入口
├── internal/
│   ├── proto/
│   │   └── frame.go                 # 分帧协议定义（§5）
│   ├── client/
│   │   ├── client.go                # 客户端核心：连接、心跳、重连、请求转发
│   │   ├── power.go                 # 防休眠通用接口
│   │   ├── power_windows.go         # Windows SetThreadExecutionState
│   │   ├── power_darwin.go          # macOS caffeinate
│   │   ├── power_linux.go           # Linux systemd-inhibit
│   │   └── power_unsupported.go     # 其他平台兜底
│   └── server/
│       ├── server.go                # HTTP 路由装配、启动
│       ├── registry.go              # 隧道注册表（内存态 + JSON 持久化）
│       ├── relay.go                 # 中转处理器：HTTP 请求 → 帧下发 → 响应帧流式写回
│       ├── tunnel.go                # WS 接入、tunnelID 分配/续用、会话读写循环、reqId 分发
│       └── debuglog.go              # 调试请求日志中间件（-debug）
└── config.json                      # 客户端本地配置（运行时生成）
```

## 3. 功能需求

### 3.1 服务器：隧道接入与 tunnelID 分配（`GET /ws/tunnel`）

`GET /ws/tunnel`（WebSocket Upgrade）。**接入参数不放在 URL query 里，改由 Upgrade 后客户端发送的首帧 `hello` 携带**（避免密钥进入访问日志）。

处理流程：

1. Upgrade 成功（`gorilla/websocket`，读写缓冲 ≥ 64KB，读限 4MB，关闭压缩协商）。校验 `Origin` 头：仅允许 `native-client://` 前缀（或 `-allowed-origins` 显式配置），其余拒绝 `403`。
2. 10s 内必须收到 `hello` 帧：`{"type":"hello","version":1,"desiredId":"<8位或空>","secret":"<此前持有的密钥或空>","clientType":"native-client","extVersion":"1.0.0"}`。超时未收到 → 关闭。
3. **ID 分配逻辑**（按优先级）：
   - `desiredId` 非空且格式合法（`^[a-z0-9]{8}$`，服务器对输入统一小写化后校验）且**未被他人占用**（不存在，或存在且 `secret` 与该 ID 注册时存的密钥哈希匹配）→ 确认使用该 ID；
   - `desiredId` 非空但已被占用且 secret 不匹配 → 不回错误，**自动降级**为分配一个新的随机 ID（保证客户端永远能连上，客户端 UI/日志负责提示"自定义 ID 不可用，已分配新 ID"）；
   - `desiredId` 为空 → 生成随机 8 位 ID（`[a-z0-9]`，`crypto/rand`，冲突重试）。
4. 回 `hello_ack`：`{"type":"hello_ack","ok":true,"tunnelID":"<最终ID>","secret":"<新生成的密钥，仅首次分配时下发>","idReused":true|false,"desiredDenied":true|false}`。
   - **secret 规则**：首次分配某 ID 时由服务器生成（32 字节随机，base64url），随 ack 下发一次；服务器只存 SHA-256 哈希。客户端必须持久化保存到 `config.json`，此后每次连接都在 `hello` 中带上 `desiredId` + `secret` 以证明所有权，实现"未被分配走就优先续用原码"。
   - 续用成功（`idReused:true`）时**不重新下发 secret**（沿用旧值）。
   - `desiredDenied:true` 表示自定义 ID 被占用、已降级为随机 ID。
5. 同 ID 重复接入：若该 tunnelID 已有在线会话且本次 hello 通过所有权校验 → 踢掉旧会话（后连优先），其全部 pending 请求按 502 失败。此机制同时保证"同一客户端重启后秒级抢回自己的 ID"。
6. 会话建立后进入读写循环（§6.1）；会话关闭（任意原因）→ 标记离线，全部 pending 请求 502 快速失败；未决的 `set_target` 等待返回 502。**注册表条目（ID→secret 哈希→目标地址）永久保留**，离线仅是会话状态，不释放 ID——这是"续用优先"的基础。

### 3.2 服务器：中转接口（核心）

`ANY /t/{tunnelID}/*path`（`ANY` = 所有 HTTP 方法，含 OPTIONS/HEAD/PATCH/自定义动词；tunnelID 不合法格式直接 `404`）：

1. 按 tunnelID 查注册表；不存在 → `404 {"error":"tunnel not found"}`；离线 → `502 {"error":"tunnel offline"}`（**快速失败，绝不排队等待**）。
2. 生成 reqId（会话内单调递增 uint32），构造 `request_start` 帧（方法、path、query、headers、hasBody），经会话发送队列下发：
   - **hop-by-hop 头必须剥离**（Connection、Keep-Alive、Proxy-Authenticate、Proxy-Authorization、TE、Trailer、Transfer-Encoding、Upgrade），其余头（含 Cookie、Authorization、自定义头）原样透传；
   - 请求体流式读取，按 **256KB**（base64 编码前）切块，逐块发 `request_chunk`，结束后发 `request_end`；无 body 则 `hasBody=false`。
3. 等待客户端回帧：
   - `response_start`（status、headers）→ 剥离 hop-by-hop 头与 `Content-Length`，写状态码与头，**立即 Flush**；
   - 每个 `response_chunk` → base64 解码写入 ResponseWriter 并 **Flush**（SSE/流式的关键）；
   - `response_end` → 结束；其 `error` 非空时：未写状态码则返回 `502 {"error":...}`，已写则直接截断连接；
   - 会话断开 → 未写状态码时返回 `502`。
4. 两段式超时（取代旧版全程 120s 硬超时）：
   - **等待 `response_start`**：120s 未收到首帧 → 向客户端发 `cancel` 帧，向调用方返回 `504`；
   - **流式阶段**：不设总时长上限（SSE/LLM 长回复可达数分钟乃至更久），仅设 **300s 空闲超时**且每收到一帧即重置——只要流持续有数据就永不超时，彻底停滞才断开（向客户端发 `cancel`；已写状态码则截断连接并记 warn 日志）。
5. 每完成一次请求，该隧道 `requestCount++`（内存计数，供 `stats` 上报用，不落盘）。

### 3.3 服务器：健康检查

`GET /healthz` → `200 {"ok":true,"tunnels":<注册数>,"online":<在线数>}`（不含 ID 明细，避免信息泄露）。

### 3.4 服务器：安全边界（按需求保持最小化）

- 中转接口 `/t/*` 无鉴权（用户明确不需要）；安全性依赖 tunnelID 的不可猜测性（8 位字母数字 ≈ 41 bit 熵，属"能力 URL"模型，README 中声明适用边界为可信网络/演示环境）。
- ID 所有权由 secret 保护：仅防止 ID 被他人抢注/劫持，不构成对调用方的访问控制。

### 3.5 客户端：命令行客户端（`selftunnel-client`）

`selftunnel-client -config config.json [-server ...] [-target ...] [-custom-id ...]`

1. 启动时加载 `config.json`（不存在则创建空配置）。
2. 命令行参数优先级高于配置文件：`-server`、`-target`、`-custom-id`、`-insecure`、`-server-insecure`、`-client-cert`、`-client-key`、`-debug`（调试模式：打印每一个转发到目标的网络请求）。
3. 校验服务器地址与目标地址格式；服务器地址统一归一化为 `wss://host/ws/tunnel`（输入 `https://` 自动转 `wss://`，裸 host 补 `wss://`）。
4. 连接成功后终端打印框线提示，包含公网入口 URL：`https://<server>/t/{tunnelID}/`。
5. 状态变化通过 `log/slog` 输出；在线期间每 25s 心跳；断线后指数退避重连（1s→2s→4s…上限 60s，±20% 抖动），无限重试。
6. 收到 `SIGINT`/`SIGTERM` 后优雅关闭连接。

### 3.6 客户端：GUI 客户端（`selftunnel-client-gui`）

Fyne 跨平台桌面窗口，标题"selftunnel client"，初始尺寸 720×540：

1. **连接设置表单**：服务器地址、目标地址、自定义 tunnelID、"跳过中继服务器 TLS 校验"复选框、"跳过目标 HTTPS 自签名校验"复选框、"调试模式"复选框（打印每个网络请求）、客户端证书/私钥路径（可选，用于目标 mTLS）。
2. **状态栏**：连接状态（未连接/连接中/在线/重连中）、当前 tunnelID、"复制 ID"按钮。
3. **日志区**：多行只读文本框，保留最近约 500 行日志。
4. **连接按钮**：在线时显示"断开"，离线时显示"连接"。
5. 点击连接时保存配置到 `config.json`，启动后台 goroutine 运行客户端核心；点击断开或关闭窗口时优雅停止。
6. 配置目标地址为空时，后续中转请求返回 502（客户端侧行为）。

### 3.7 客户端：防休眠（跨平台）

隧道在线期间阻止系统因空闲进入睡眠；断开或退出后恢复默认电源策略。

| 平台 | 机制 | 是否需要管理员 |
|---|---|---|
| Windows | `SetThreadExecutionState(ES_CONTINUOUS \| ES_SYSTEM_REQUIRED \| ES_DISPLAY_REQUIRED)` | 否 |
| macOS | `caffeinate -i -w <pid>` 子进程 | 否 |
| Linux | `systemd-inhibit --what=sleep:idle --mode=block sleep infinity` 子进程 | 否（普通用户默认允许） |
| 其他 | 空实现，不阻止睡眠 | — |

- 连接成功后调用 `PreventSleep()`；连接关闭/退出时调用 `AllowSleep()`。
- 失败只记 warn，不影响隧道主逻辑。
- 只能阻止**空闲超时导致的睡眠**；用户手动睡眠、合盖（若电源计划设为合盖休眠）、电量耗尽关机仍会触发。

## 4. URL 路由总表（服务端，全部路由）

| 路由 | 用途 |
|---|---|
| `GET /ws/tunnel` | 隧道接入（WebSocket Upgrade） |
| `ANY /t/{tunnelID}/*` | 中转接口（§3.2） |
| `GET /healthz` | 健康检查（§3.3） |
| 其他 | `404` |

服务器启动 flag：`-addr`（默认 `:8080`）、`-data`（数据目录，默认 `./data`）、`-allowed-origins`（默认 `native-client://`，逗号分隔前缀，`*` 关闭校验）、`-cert`/`-key`（TLS）、`-debug`（调试模式：打印每一个收到的网络请求，并将日志级别降为 Debug）。无其他端点、无静态资源、无 UI。

## 5. 隧道分帧协议（单条 WSS 连接，全部为 WS 文本帧，JSON 编码）

所有帧都是一条独立 WS 文本消息，体为 UTF-8 JSON。二进制负载一律 base64 放在字符串字段中。

```jsonc
// ===== 客户端 → 服务端 =====
{"type":"hello","version":1,"desiredId":"ab12cd34","secret":"<base64url 或空>","clientType":"native-client","extVersion":"1.0.0"}
{"type":"pong","ts":1735689600}
{"type":"set_target_ack","ok":true}                          // 或 {"ok":false,"error":"invalid url"}
{"type":"stats","forwarded":128,"target":"http://192.168.1.10:8080"}   // 目标变更时主动上报，或应答 get_stats
{"type":"response_start","reqId":42,"status":200,"headers":{"Content-Type":["application/json"],"X-Any":["v"]}}
{"type":"response_chunk","reqId":42,"seq":0,"data":"<base64, 原始≤256KB>"}
{"type":"response_end","reqId":42}                           // 异常时 {"reqId":42,"error":"connection refused"}

// ===== 服务端 → 客户端 =====
{"type":"hello_ack","ok":true,"tunnelID":"ab12cd34","secret":"<仅首次分配时下发>","idReused":true,"desiredDenied":false}
{"type":"ping","ts":1735689600}
{"type":"set_target","target":"http://192.168.1.10:8080"}    // 保留通道：外部覆盖目标地址
{"type":"get_stats"}
{"type":"request_start","reqId":42,"method":"POST","path":"/api/users","query":"a=1&b=2","headers":{"Content-Type":["application/json"],"Authorization":["Bearer ..."]},"hasBody":true}
{"type":"request_chunk","reqId":42,"seq":0,"data":"<base64, 原始≤256KB>"}
{"type":"request_end","reqId":42}
{"type":"cancel","reqId":42}
```

规则：

- 未知 `type` 必须忽略（前向兼容）；`version` 不匹配时服务端回 `hello_ack{ok:false,"error":"version mismatch"}` 并关闭。
- **reqId 由服务端分配**（uint32 递增，回绕后跳过在用值）；同一 reqId 的 chunk 必须按 seq 递增、按序处理；不同 reqId 的帧允许任意交错。
- `request_start` 后，若 `hasBody=true` 则随后为若干 `request_chunk` + 一个 `request_end`；`hasBody=false` 则无 chunk 也无 end。
- 响应侧同理：`response_start` → 若干 `response_chunk` → 一个 `response_end`；每个 reqId 恰好一个 start 与一个 end。
- `set_target` 同一时间至多一个未决；服务端等待 ack 超时 10s 视为失败。
- `cancel` 语义：收到方立即停止该 reqId 的一切读写并回收资源，不再发送该 reqId 后续帧；迟到的该 reqId 帧静默丢弃。
- 单帧 WS 消息大小上限 4MB。

## 6. 关键实现要点

### 6.1 服务端会话读写循环（`internal/server/tunnel.go`）

- 读循环：`ReadMessage` → `json.Unmarshal` → 按 `type` switch。`response_*` 帧经 `map[uint32]*pendingResp`（RWMutex 保护，内含帧通道与 `done` 通道）投递；不存在该 reqId 则丢弃。**严禁静默丢帧**：帧通道满时阻塞发送（背压传导至隧道客户端），`UnregisterPending`/会话关闭时 close `done` 中止阻塞。流式响应（SSE）中途隧道断开、空闲超时、收到错误帧均记 warn 日志。
- 写循环：单一 goroutine 从 `sendQ chan []byte` 取帧 `WriteMessage`；任何组件发帧只能入队。队列容量 256，满时最老的中转请求按失败处理并记录告警（背压保护）。
- 心跳：每 25s 向 sendQ 发 `ping`；`SetReadDeadline(now+90s)`，每收到任何帧即续期；超时关闭会话。
- **注册表并发**：`registry` 为 `map[string]*Tunnel` + RWMutex；`Tunnel` 内含 secret 哈希、目标地址（`atomic.Value`）、当前会话指针（`atomic.Pointer`）、请求计数（`atomic.Uint64`）。踢旧会话与新会话接管必须原子完成，防止中转请求被路由到已死会话。

### 6.2 服务端中转处理器（`internal/server/relay.go`）

不使用 `httputil.ReverseProxy`（协议是消息级，字节流抽象不适用），手工实现：

```go
// 伪代码骨架
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
    id, rest := splitTunnelPath(r.URL.Path)               // /t/{id}/* → id, path
    tun := s.registry.Get(id)                             // 不存在 → 404
    sess := tun.Session()                                 // 离线 → 502
    reqId := sess.nextReqID()
    ch := sess.registerPending(reqId)
    defer sess.unregisterPending(reqId)

    sess.send(Frame{Type:"request_start", ReqID:reqId, Method:r.Method,
                    Path:rest, Query:r.URL.RawQuery, Headers:filterHeaders(r.Header),
                    HasBody:r.Body!=nil})
    if r.Body != nil { streamChunks(sess, reqId, r.Body); sess.send(Frame{Type:"request_end",ReqID:reqId}) }

    start := waitFrame(ch, ctx)                           // 等待首帧 120s，超时 504+cancel
    // 流式阶段：300s 空闲超时（逐帧重置），无总时长上限
    writeHeaders(w, filterHeaders(start.Headers))         // 剥 hop-by-hop + Content-Length
    w.WriteHeader(start.Status); flush(w)
    for f := range ch {
        if f.Type=="response_chunk" { w.Write(decode(f.Data)); flush(w) }
        if f.Type=="response_end"   { return }
    }
}
```

### 6.3 客户端核心（`internal/client/client.go`）

- 连接状态机：`disconnected` → `connecting` → `online` → `backoff`（重连退避）。
- `Run(ctx)` 循环：连接 → hello → readLoop；失败/断线 → 指数退避重试。
- 心跳：每 25s 发 `ping`；读超时 2 倍心跳间隔未收到任何帧则判定死连接。
- 请求转发：`request_start` → 目标 `http.Client.Do` → `response_start` → 流式读取响应体 → `response_chunk` → `response_end`。
- 并发上限 8（信号量）。
- WebSocket 写超时 60s（256KB 帧 base64 后约 350KB，低带宽隧道上 10s 可能写不完导致响应被误杀）。
- 收到 `cancel` 时取消对应 reqId 的 `http.Client` 请求上下文。
- 配置保存：连接成功后若服务器下发/确认 tunnelID 与 secret，写回 `config.json`。

### 6.4 部署

服务器单端口同时服务 WS 与中转接口。生产**必须**以 HTTPS/WSS 经反向代理暴露（443）——混合内容规则下浏览器/客户端无法从安全上下文连 `ws://`；README 给出 nginx 示例（`proxy_http_version 1.1` + `Upgrade`/`Connection` 头 + `proxy_read_timeout 86400s`）。

客户端分发：

- `build.sh` 产出：
  - `selftunnel-server`（本机）
  - `selftunnel-server-linux`（Linux amd64）
  - `dist/selftunnel-client-{windows,darwin,linux}-{amd64,arm64}[.exe]`（无 cgo 静态二进制）
  - `dist/selftunnel-client-gui-{GOOS}-{GOARCH}[.exe]`（本机 GUI）
- Windows 用户直接运行 `.exe`，macOS/Linux 用户运行对应二进制；均无需安装、无需管理员权限。

### 6.5 持久化

服务端 `data/tunnels.json`：`[{id, secretHash, target, createdAt}]`。变更即写盘（临时文件 + rename）。启动时加载。**在线状态、请求计数不落盘。**

客户端 `config.json`：`{server, target, customId, tunnelId, secret}`。

## 7. 非功能需求

- 并发隧道数 ≥ 100；单隧道并发中转请求 ≥ 8。
- 中转额外延迟：局域网/低延迟链路下小请求（<64KB）< 50ms（不含目标本身耗时）。
- Body 大小无上限（分块流式；响应侧严禁整体缓冲——请求侧 v1 允许内存汇集，见 §6.2 已知限制）。
- 吞吐：内网链路下 ≥ 10 MB/s（base64+JSON 开销已计入，为消息级协议的既定代价）。
- 内存：服务端空闲每隧道 < 5MB；CLI/GUI 客户端常态 < 100MB。
- 流式可靠性：SSE/流式响应在背压下不得静默丢帧或截断；>120s 的长流完整到达，中转层不得成为时长瓶颈。

## 8. 明确非目标（不要做）

- 服务器端任何 Web UI、任何 HTML 页面。
- 调用方鉴权、用户系统、配额、HTTPS 证书自动签发。
- UDP、TCP 通用端口转发、P2P/WebRTC 任何元素。
- 浏览器扩展、Firefox/Safari 适配、纯网页转发。
- 目标 HTTPS 自签证书的程序化处理（由用户在系统/浏览器中自行信任，CLI 提供 `-insecure` 调试开关）。

## 9. 验收标准（全部通过才算完成）

1. `selftunnel-server` 启动后仅暴露 §4 三个路由；`GET /healthz` 正常返回。
2. CLI 客户端启动并配置服务器地址后，连接成功终端打印 tunnelID 与入口 URL；GUI 客户端启动后图标/状态显示在线，展示 8 位 tunnelID。
3. 重启客户端 → 自动重连且**拿回同一个 tunnelID**（`idReused:true`）。
4. 设置自定义 tunnelID 并重连 → 获得该 ID；换一台机器（无 secret）设置同一自定义 ID → 被降级分配随机 ID（`desiredDenied:true`），原持有者的隧道不受影响。
5. 多客户端：两个原生客户端同时在线，各有独立 tunnelID，`/t/{idA}/x` 与 `/t/{idB}/x` 请求分别到达各自的目标，互不串流。
6. 配置目标 `http://<内网HTTP服务>` 后，`curl -X GET/POST/PUT/DELETE/PATCH/OPTIONS/HEAD https://server/t/{id}/任意路径?q=1` 全部正确转发：方法、路径、query、自定义 header、请求体原样到达目标；响应状态码、header、body 原样返回。
7. 大文件：经隧道下载 500MB 文件完成且服务端与客户端内存占用平稳；吞吐 ≥ 10 MB/s（内网链路）。
8. 流式：目标是 SSE 端点时，事件逐条实时到达调用方（逐 chunk Flush 生效）；持续 >120s（如 160s）的 SSE 流完整到达、无截断、无丢帧、终止事件完好。
9. 断网 30s 后恢复：自动重连并恢复原 tunnelID；断连期间中转请求收到 502 快速失败（<100ms）。
10. 隧道在线期间系统空闲不自动睡眠；手动断开后恢复默认电源策略（Windows/macOS/Linux 至少验证当前开发平台）。
11. 目标不可达 → 502；等待 `response_start` 超过 120s → 504 且客户端收到 cancel；流式阶段 300s 无数据 → 断开且客户端收到 cancel；不存在的 tunnelID → 404。
12. 并发：同时发起 8 个中转请求全部正确返回，reqId 交错不串包。
13. GUI 客户端请求日志区展示最近请求/状态/重连日志，清空配置后重新连接可生成新 tunnelID。
14. 质量门槛全部干净通过：`go vet ./...`；golangci-lint 零问题（depguard 强制依赖白名单）；`go test -race` 跑全量测试且 `internal/` 语句覆盖率 ≥ 70%；服务端与 CLI 客户端在 windows/linux/darwin（amd64 与 arm64）上免 cgo 交叉编译通过；`build.sh` 一键产出服务端二进制、跨平台 CLI 二进制与本机 GUI 二进制。

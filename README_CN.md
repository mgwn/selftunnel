# selftunnel

自托管的多租户**反向隧道 / 内网穿透**系统——[ngrok](https://ngrok.com)、[frp](https://github.com/fatedier/frp) 的轻量替代。把运行在内网或本机的任意 HTTP 服务暴露到公网服务器上的一个固定 URL，无需公网 IP、无需改路由器、无需域名备案。

支持 **Windows / macOS / Linux**，服务端与客户端均为单二进制文件，**不需要管理员权限**即可运行。

> 文档：[English](README.md) · 简体中文（本文件）

---

## 目录

- [功能特性](#功能特性)
- [架构概述](#架构概述)
- [项目结构](#项目结构)
- [快速开始](#快速开始)
- [编译](#编译)
- [配置说明](#配置说明)
- [部署到服务器](#部署到服务器)
- [安全与边界](#安全与边界)
- [平台支持](#平台支持)
- [开发与调试](#开发与调试)
- [规格说明](#规格说明)

---

## 功能特性

- **多租户**：任意多个客户端可同时接入同一台服务器，每个客户端拥有独立的 8 位 `tunnelID`。
- **自动续用**：客户端持久化 `tunnelID` 与密钥，重启后优先抢回原 ID。
- **自定义 ID**：可提前占用自己的 8 位字母数字 ID，他人无法抢注。
- **全 HTTP 转发**：支持任意方法、Header、Query、Body；响应流式回传，支持 SSE / 大文件下载。
- **跨平台 GUI**：基于 Fyne 的桌面客户端，填地址点连接即可。
- **命令行客户端**：适合服务器、Docker、CI 等无头场景。
- **防休眠**：Windows / macOS / Linux 在隧道在线期间自动阻止系统空闲睡眠。
- **mTLS 支持**：客户端可向目标 HTTPS 服务提供客户端证书。
- **单二进制**：服务端 `selftunnel-server`、客户端 `selftunnel-client`、GUI `selftunnel-client-gui`，无需运行时依赖。

---

## 架构概述

```
┌─────────────────┐         WSS /ws/tunnel         ┌─────────────────┐
│   selftunnel-client  │  ◄──────────────────────────►   │  selftunnel-server   │
│  (你的笔记本/内网) │                                 │  (公网服务器)    │
└─────────────────┘                                 └────────┬────────┘
                                                             │
                                                             ▼ HTTP
                                                    ┌─────────────────┐
                                                    │   API 调用方     │
                                                    │  /t/{tunnelID}/* │
                                                    └─────────────────┘
```

1. 客户端向外主动连接服务器的 `/ws/tunnel`，建立 WebSocket 长连接。
2. 服务器分配（或续用）8 位 `tunnelID`，并给出公网入口 `https://server/t/{tunnelID}/...`。
3. 当有人访问该入口时，服务器把 HTTP 请求切片成帧，经 WebSocket 下发给客户端。
4. 客户端把请求转发到本地配置的真实目标地址，再把响应切片回传。

---

## 项目结构

仓库采用标准开源 Go 项目布局，规格说明文档与代码同库保管：

```
├── README.md / README_CN.md   # 文档（英文为正式版）
├── specs/                     # 规格说明——事实来源（英文 + 中文）
├── cmd/
│   ├── selftunnel-server/          # 服务端入口
│   ├── selftunnel-client/          # 命令行客户端入口
│   └── selftunnel-client-gui/      # Fyne GUI 客户端入口
├── internal/
│   ├── proto/                 # 分帧协议
│   ├── client/                # 客户端核心、防休眠
│   └── server/                # 服务端：注册表、中转处理器、会话
├── build.sh                   # 一键构建全部二进制
├── Makefile                   # fmt / vet / test / build 目标
├── Dockerfile*                # 服务端镜像（多阶段 / 仅构建 / 仅运行时）
└── docker-compose*.yml        # 部署示例
```

---

## 快速开始

### 1. 启动服务端

```bash
./selftunnel-server -addr :8080 -data ./data
```

调试时可加 `-debug`，服务器会打印每一个收到的网络请求（方法、路径、来源地址、状态码、字节数、耗时）：

```bash
./selftunnel-server -addr :8080 -data ./data -debug
```

服务端暴露三个端点：

- `GET /ws/tunnel` —— 隧道接入
- `ANY /t/{tunnelID}/*` —— 中转接口
- `GET /healthz` —— 健康检查

生产环境应加反向代理以 HTTPS/WSS 暴露（见下文）。

### 2. 启动 GUI 客户端

```bash
./selftunnel-client-gui
```

在窗口中填写：

- **服务器**：`wss://your-server.com` 或 `https://your-server.com`
- **目标地址**：你要暴露的内网服务，例如 `http://127.0.0.1:8080`
- **自定义 ID**（可选）：8 位小写字母或数字，留空则随机分配
- **调试模式**（可选）：勾选后在日志窗口打印每一个转发到目标的网络请求

点击「连接」，成功后状态栏显示 `tunnelID`，例如 `e51pvnaw`。此时即可通过：

```bash
curl https://your-server.com/t/e51pvnaw/api/hello
```

访问你的内网服务。

### 3. 或使用命令行客户端

```bash
./selftunnel-client -server wss://your-server.com -target http://127.0.0.1:8080
```

首次连接成功后会自动创建 `config.json` 并保存 `tunnelID` 与密钥，下次直接运行：

```bash
./selftunnel-client
```

即可自动续用原 ID。

---

## 编译

### 环境要求

- Go ≥ 1.25（构建自动使用 `go.mod` 固定的工具链 go1.26.8）
- GUI 客户端需要 cgo（Windows 需 mingw-w64，macOS/Linux 默认有）

### 一键构建

```bash
./build.sh
```

产物：

```
selftunnel-server                # 本机服务端
selftunnel-server-linux          # Linux amd64 服务端

dist/
├── selftunnel-client-windows-amd64.exe
├── selftunnel-client-darwin-amd64
├── selftunnel-client-darwin-arm64
├── selftunnel-client-linux-amd64
├── selftunnel-client-linux-arm64
├── selftunnel-client-gui-<os>-<arch>[.exe]   # 本机 GUI
├── selftunnel-client-gui-darwin-amd64        # macOS Intel（Apple Silicon 上可交叉编译）
└── selftunnel-client-gui-windows-amd64.exe   # Windows GUI（不弹控制台）
```

> Linux GUI 客户端无法从 macOS / Windows 交叉编译（依赖 glibc 与 X11/Wayland），请在 Linux 本机或 Docker 中构建。

### 手动构建

```bash
# 服务端
go build -o selftunnel-server ./cmd/selftunnel-server

# CLI 客户端（当前平台）
go build -o selftunnel-client ./cmd/selftunnel-client

# GUI 客户端（当前平台）
go build -o selftunnel-client-gui ./cmd/selftunnel-client-gui

# 交叉编译 CLI（以 Windows 为例）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o selftunnel-client.exe ./cmd/selftunnel-client

# 交叉编译 Windows GUI（macOS / Linux 上需先安装 mingw-w64）
CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
  go build -ldflags='-w -s -H=windowsgui' -o selftunnel-client-gui-windows-amd64.exe ./cmd/selftunnel-client-gui

# 交叉编译 macOS Intel GUI（仅在 macOS 上可行）
CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags='-w -s' -o selftunnel-client-gui-darwin-amd64 ./cmd/selftunnel-client-gui
```

> `-H=windowsgui` 让 Windows 启动 GUI 客户端时不显示黑色控制台窗口。
>
> 各平台 GUI 编译要点：
> - **Windows**：从 macOS / Linux 交叉编译需安装 mingw-w64。
> - **macOS amd64**：只能在 macOS 上交叉编译（用 Xcode / Command Line Tools 的 SDK）。
> - **Linux**：无法从其他平台交叉编译，请在 Linux 本机或 Docker 中构建。
> - 若只编译本机平台，直接 `go build ./cmd/selftunnel-client-gui` 即可。

---

## 配置说明

客户端配置文件为 `config.json`，首次连接成功后自动生成：

```json
{
  "server": "wss://your-server.com/ws/tunnel",
  "target": "http://127.0.0.1:8080",
  "customId": "",
  "tunnelId": "e51pvnaw",
  "secret": "4hQ2g6zDj0cbJCGl7pv0iBemStePSf1UdxRGvvZ2SCw"
}
```

| 字段 | 说明 |
|---|---|
| `server` | 中继服务器地址，支持 `wss://`、`https://` 或裸 host，会自动归一化为 `wss://host/ws/tunnel` |
| `target` | 真实内网目标基址，必须 `http://` 或 `https://` 开头 |
| `customId` | 8 位小写字母数字，留空表示沿用 `tunnelId` 或随机分配 |
| `tunnelId` | 服务器最终分配的 ID，由客户端自动维护 |
| `secret` | ID 所有权密钥，由客户端自动维护，**不要手动修改或泄露** |

### CLI 参数

```bash
./selftunnel-client \
  -config config.json \
  -server wss://your-server.com \
  -target http://127.0.0.1:8080 \
  -custom-id ab12cd34 \
  -insecure \
  -server-insecure \
  -client-cert client.pem \
  -client-key client.key
```

| 参数 | 说明 |
|---|---|
| `-config` | 配置文件路径 |
| `-server` | 中继服务器地址 |
| `-target` | 目标地址 |
| `-custom-id` | 自定义 tunnelID |
| `-insecure` | 跳过目标 HTTPS 的 TLS 校验（调试用） |
| `-server-insecure` | 跳过中继 WSS 的 TLS 校验（调试用） |
| `-client-cert` / `-client-key` | 目标 mTLS 客户端证书 |
| `-debug` | 调试模式：打印每一个转发到目标的网络请求（方法、URL、状态码、字节数、耗时） |

---

## 部署到服务器

### 使用 Docker Compose

```bash
docker compose up -d
```

默认监听 `:8080`，数据挂载到 `./data`。

### 使用 Systemd

创建 `/etc/systemd/system/selftunnel.service`：

```ini
[Unit]
Description=selftunnel server
After=network.target

[Service]
Type=simple
ExecStart=/opt/selftunnel/selftunnel-server -addr :8080 -data /opt/selftunnel/data
Restart=always
WorkingDirectory=/opt/selftunnel

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now selftunnel
```

### nginx 反向代理示例（HTTPS / WSS）

```nginx
server {
    listen 443 ssl http2;
    server_name relay.example.com;

    ssl_certificate     /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;

    location / {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade $http_upgrade;
        proxy_set_header   Connection "upgrade";
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_read_timeout 86400s;
    }
}
```

> 生产环境必须走 HTTPS/WSS，否则浏览器或 HTTP 客户端会因混合内容策略拒绝连接。

---

## 安全与边界

- **调用方无鉴权**：`/t/{tunnelID}/*` 是能力 URL，安全性依赖于 8 位 `tunnelID` 的不可猜测性（约 41 bit 熵）。
- **ID 所有权保护**：自定义 ID 需要持有对应 `secret` 才能续用/抢注，防止被他人占用。
- **无管理员权限**：客户端不需要 root / UAC 提权。
- **TLS 建议**：公网服务端必须使用 TLS；客户端与目标服务之间也建议使用 TLS。

本项目适用于可信网络、个人/团队演示、开发联调等场景，**不建议直接暴露高敏感内网服务到公开互联网**。

---

## 平台支持

| 平台 | 服务端 | CLI 客户端 | GUI 客户端 | 防休眠 |
|---|---|---|---|---|
| Windows | ✓ | ✓ | ✓ | `SetThreadExecutionState` |
| macOS | ✓ | ✓ | ✓ | `caffeinate` |
| Linux | ✓ | ✓ | ✓ | `systemd-inhibit` |
| 其他 | ✓ | ✓ | 视 Fyne 支持 | 无 |

---

## 开发与调试

```bash
# 格式化 + 检查
go fmt ./...
go vet ./...

# 运行测试
go test ./...

# 本地启动服务端 + CLI 客户端（-debug 打印每一个网络请求）
./selftunnel-server -addr :18080 -data ./data -debug &
./selftunnel-client -server ws://127.0.0.1:18080 -target http://127.0.0.1:18081 -debug

# 测试中转
curl http://127.0.0.1:18080/t/{tunnelID}/hello
```

规格说明的验收标准由双轨测试体系保障：自动化 pipeline 测试（`make test` 单元 + 本地回环端到端，`make smoke` 真实二进制冒烟），以及面向 AI 执行的测试计划（GUI、跨机器、长时间运行场景），覆盖对照见 [tests/README.md](tests/README.md)。克隆后执行一次 `make hooks` 可安装 git 钩子（Conventional Commits 提交消息校验 + 上述提交前检查）。

---

## 规格说明

本项目采用 **规格驱动（spec-first）** 的开发方式：规格说明是事实来源，行为变更先改规格。英文版为正式版，中文翻译保持同步。

- [specs/selftunnel-Spec.md](specs/selftunnel-Spec.md) —— 英文（正式版）
- [specs/selftunnel-Spec_CN.md](specs/selftunnel-Spec_CN.md) —— 简体中文

贡献流程遵循同样的工作方式，见 [CONTRIBUTING.md](CONTRIBUTING.md)；重要变更记录在 [CHANGELOG.md](CHANGELOG.md)。

---

## 许可证

[MIT](LICENSE)

# selftunnel

[![CI](https://github.com/mgwn/selftunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/mgwn/selftunnel/actions/workflows/ci.yml)
[![Release pipeline](https://github.com/mgwn/selftunnel/actions/workflows/release.yml/badge.svg)](https://github.com/mgwn/selftunnel/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/mgwn/selftunnel.svg)](https://pkg.go.dev/github.com/mgwn/selftunnel)
[![Release](https://img.shields.io/github/v/release/mgwn/selftunnel)](https://github.com/mgwn/selftunnel/releases)
[![Downloads](https://img.shields.io/github/downloads/mgwn/selftunnel/total)](https://github.com/mgwn/selftunnel/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-informational.svg)](LICENSE)
[![Go ≥ 1.25](https://img.shields.io/badge/go-%E2%89%A5%201.25-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)

A self-hosted, multi-tenant **reverse tunnel** — a lightweight [ngrok](https://ngrok.com) / [frp](https://github.com/fatedier/frp) alternative that exposes any intranet or localhost HTTP service at a public URL. No public IP, no router changes, no domain registration required.

Supports **Windows / macOS / Linux**. The server and clients are all single binaries and **do not require administrator privileges**.

> Documentation: English (this file) · [简体中文](README_CN.md)

---

## Contents

- [Features](#features)
- [Architecture](#architecture)
- [Project Layout](#project-layout)
- [Quick Start](#quick-start)
- [Building](#building)
- [Configuration](#configuration)
- [Server Deployment](#server-deployment)
- [Security & Boundaries](#security--boundaries)
- [Platform Support](#platform-support)
- [Development & Debugging](#development--debugging)
- [Specifications](#specifications)

---

## Features

- **Multi-tenant**: any number of clients can connect to the same server at the same time, each with its own 8-character `tunnelID`.
- **Automatic ID reuse**: the client persists its `tunnelID` and secret, and reclaims the same ID after a restart.
- **Custom IDs**: reserve your own 8-character alphanumeric ID in advance; nobody else can claim it.
- **Full HTTP forwarding**: any method, header, query or body; responses are streamed back, including SSE and large file downloads.
- **Cross-platform GUI**: a Fyne-based desktop client — fill in the address, click Connect.
- **Command-line client**: for headless servers, Docker, CI and similar scenarios.
- **Sleep prevention**: on Windows / macOS / Linux the system is kept from idle-sleeping while the tunnel is online.
- **mTLS support**: the client can present a client certificate to the target HTTPS service.
- **Single binaries**: `selftunnel-server`, `selftunnel-client` and `selftunnel-client-gui` have no runtime dependencies.

---

## Architecture

```
┌─────────────────┐         WSS /ws/tunnel         ┌─────────────────┐
│   selftunnel-client  │  ◄──────────────────────────►   │  selftunnel-server   │
│ (your laptop /  │                                 │ (public server) │
│  intranet host) │                                 └────────┬────────┘
└─────────────────┘                                          │
                                                             ▼ HTTP
                                                    ┌─────────────────┐
                                                    │   API callers   │
                                                    │ /t/{tunnelID}/* │
                                                    └─────────────────┘
```

1. The client dials out to the server's `/ws/tunnel` endpoint and keeps a long-lived WebSocket connection.
2. The server allocates (or reuses) an 8-character `tunnelID` and publishes the entry point `https://server/t/{tunnelID}/...`.
3. When someone hits that entry point, the server slices the HTTP request into frames and delivers them to the client over the WebSocket.
4. The client forwards the request to the real target address configured locally, then streams the response back in chunks.

---

## Project Layout

The repository follows a standard open-source Go layout, with the specifications kept alongside the code:

```
├── README.md / README_CN.md   # documentation (English is official)
├── specs/                     # specifications — the source of truth (EN + CN)
├── cmd/
│   ├── selftunnel-server/          # server entry point
│   ├── selftunnel-client/          # CLI client entry point
│   └── selftunnel-client-gui/      # Fyne GUI client entry point
├── internal/
│   ├── proto/                 # wire framing protocol
│   ├── client/                # client core, sleep prevention
│   └── server/                # server: registry, relay handler, sessions
├── build.sh                   # one-command build for all binaries
├── Makefile                   # fmt / vet / test / build targets
├── Dockerfile*                # server images (multi-stage / build-only / runtime-only)
└── docker-compose*.yml        # deployment examples
```

---

## Quick Start

### 1. Start the server

```bash
./selftunnel-server -addr :8080 -data ./data
```

For debugging, add `-debug`: the server then prints every incoming network request (method, path, peer address, status code, byte count, duration):

```bash
./selftunnel-server -addr :8080 -data ./data -debug
```

The server exposes three endpoints:

- `GET /ws/tunnel` — tunnel intake
- `ANY /t/{tunnelID}/*` — relay endpoint
- `GET /healthz` — health check

In production the server should be exposed via HTTPS/WSS behind a reverse proxy (see below).

### 2. Start the GUI client

```bash
./selftunnel-client-gui
```

Fill in the window:

- **Server**: `wss://your-server.com` or `https://your-server.com`
- **Target**: the intranet service you want to expose, e.g. `http://127.0.0.1:8080`
- **Custom ID** (optional): 8 lowercase letters or digits; leave empty for a random ID

Click **Connect**. Once connected, the status bar shows your `tunnelID`, e.g. `e51pvnaw`. You can now reach your intranet service via:

```bash
curl https://your-server.com/t/e51pvnaw/api/hello
```

### 3. Or use the command-line client

```bash
./selftunnel-client -server wss://your-server.com -target http://127.0.0.1:8080
```

After the first successful connection, `config.json` is created automatically with your `tunnelID` and secret. From then on, simply running:

```bash
./selftunnel-client
```

reuses the same ID.

---

## Building

### Requirements

- Go ≥ 1.25 (builds automatically use the toolchain pinned in `go.mod`: go1.26.8)
- The GUI client needs cgo (mingw-w64 on Windows; present by default on macOS/Linux)

### One-command build

```bash
./build.sh
```

Outputs:

```
selftunnel-server                # server for the current platform
selftunnel-server-linux          # Linux amd64 server

dist/
├── selftunnel-client-windows-amd64.exe
├── selftunnel-client-darwin-amd64
├── selftunnel-client-darwin-arm64
├── selftunnel-client-linux-amd64
├── selftunnel-client-linux-arm64
├── selftunnel-client-gui-<os>-<arch>[.exe]   # GUI for the current platform
├── selftunnel-client-gui-darwin-amd64        # macOS Intel (cross-buildable on Apple Silicon)
└── selftunnel-client-gui-windows-amd64.exe   # Windows GUI (no console window)
```

> The Linux GUI client cannot be cross-compiled from macOS / Windows (it depends on glibc and X11/Wayland). Build it on Linux, either natively or in Docker.

### Manual builds

```bash
# Server
go build -o selftunnel-server ./cmd/selftunnel-server

# CLI client (current platform)
go build -o selftunnel-client ./cmd/selftunnel-client

# GUI client (current platform)
go build -o selftunnel-client-gui ./cmd/selftunnel-client-gui

# Cross-compile the CLI (Windows example)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o selftunnel-client.exe ./cmd/selftunnel-client

# Cross-compile the Windows GUI (needs mingw-w64 on macOS / Linux)
CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
  go build -ldflags='-w -s -H=windowsgui' -o selftunnel-client-gui-windows-amd64.exe ./cmd/selftunnel-client-gui

# Cross-compile the macOS Intel GUI (only possible on macOS)
CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags='-w -s' -o selftunnel-client-gui-darwin-amd64 ./cmd/selftunnel-client-gui
```

> `-H=windowsgui` prevents the black console window from appearing when the Windows GUI client starts.
>
> Per-platform GUI build notes:
> - **Windows**: cross-compiling from macOS / Linux requires mingw-w64.
> - **macOS amd64**: can only be cross-compiled on macOS (using the Xcode / Command Line Tools SDK).
> - **Linux**: cannot be cross-compiled from other platforms; build natively on Linux or in Docker.
> - For the current platform only, `go build ./cmd/selftunnel-client-gui` is enough.

### Releases

Publishing a release on GitHub — which also creates the tag if it does not
exist yet — triggers the release workflow:

```bash
gh release create v0.1.0 --title "selftunnel 0.1.0" --generate-notes
```

The workflow runs the full quality gate on the tagged commit, builds the
server, the CLI client and the GUI client for every supported platform
(linux/darwin/windows, amd64/arm64 — observing the GUI platform rules
above), attaches them with SHA-256 checksums to the release, and pushes
the multi-arch server image to Docker Hub.

The server and CLI client can also be installed directly with Go:

```bash
go install github.com/mgwn/selftunnel/cmd/selftunnel-server@latest
go install github.com/mgwn/selftunnel/cmd/selftunnel-client@latest
```

---

## Configuration

The client configuration file is `config.json`, created automatically after the first successful connection:

```json
{
  "server": "wss://your-server.com/ws/tunnel",
  "target": "http://127.0.0.1:8080",
  "customId": "",
  "tunnelId": "e51pvnaw",
  "secret": "4hQ2g6zDj0cbJCGl7pv0iBemStePSf1UdxRGvvZ2SCw"
}
```

| Field | Description |
|---|---|
| `server` | Relay server address; accepts `wss://`, `https://` or a bare host, normalized to `wss://host/ws/tunnel` |
| `target` | Real intranet target base URL; must start with `http://` or `https://` |
| `customId` | 8 lowercase alphanumerics; empty means "keep `tunnelId` or allocate randomly" |
| `tunnelId` | The ID finally assigned by the server; maintained by the client |
| `secret` | Ownership secret for the ID, maintained by the client — **do not edit or share it** |

### CLI flags

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

| Flag | Description |
|---|---|
| `-config` | Configuration file path |
| `-server` | Relay server address |
| `-target` | Target base URL |
| `-custom-id` | Desired tunnelID |
| `-insecure` | Skip TLS verification for the target HTTPS server (debugging) |
| `-server-insecure` | Skip TLS verification for the relay WSS server (debugging) |
| `-client-cert` / `-client-key` | Client certificate for target mTLS |
| `-debug` | Debug mode: print every request forwarded to the target (method, URL, status code, byte count, duration) |

---

## Server Deployment

### Docker

Prebuilt multi-arch server images (linux/amd64, linux/arm64) are published
to [Docker Hub](https://hub.docker.com/r/uoks/selftunnel) for every
release, tagged with the version and `latest`:

```bash
docker run -d -p 8080:8080 -v $(pwd)/data:/data --name selftunnel uoks/selftunnel:latest
```

### Docker Compose

```bash
docker compose up -d
```

Listens on `:8080` by default; data is mounted at `./data`.

### systemd

Create `/etc/systemd/system/selftunnel.service`:

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

### nginx reverse proxy example (HTTPS / WSS)

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

> Production must use HTTPS/WSS; otherwise browsers and HTTP clients reject the connection due to mixed-content rules.

---

## Security & Boundaries

- **No caller authentication**: `/t/{tunnelID}/*` is a capability URL. Security relies on the 8-character `tunnelID` being unguessable (~41 bits of entropy).
- **ID ownership protection**: reusing or claiming a custom ID requires the matching `secret`, so others cannot take your ID.
- **No administrator privileges**: the client needs no root / UAC elevation.
- **TLS recommendation**: the public server must use TLS; TLS between the client and its target is recommended as well.

This project suits trusted networks, personal or team demos, and development/debugging scenarios. **Do not expose highly sensitive intranet services to the public internet with it.**

---

## Platform Support

| Platform | Server | CLI client | GUI client | Sleep prevention |
|---|---|---|---|---|
| Windows | ✓ | ✓ | ✓ | `SetThreadExecutionState` |
| macOS | ✓ | ✓ | ✓ | `caffeinate` |
| Linux | ✓ | ✓ | ✓ | `systemd-inhibit` |
| Others | ✓ | ✓ | depends on Fyne | none |

---

## Development & Debugging

```bash
# Format + checks
go fmt ./...
go vet ./...

# Tests
go test ./...

# Start a local server + CLI client (-debug prints every network request)
./selftunnel-server -addr :18080 -data ./data -debug &
./selftunnel-client -server ws://127.0.0.1:18080 -target http://127.0.0.1:18081 -debug

# Test the relay
curl http://127.0.0.1:18080/t/{tunnelID}/hello
```

The spec's acceptance criteria are verified by a two-track test suite:
automated pipeline tests (`make test` for unit + loopback end-to-end,
`make smoke` for a real-binary smoke run) and an AI-executable plan for
GUI, cross-machine and long-running scenarios — see
[tests/README.md](tests/README.md) for the coverage map. Run `make hooks`
once after cloning to install the git hooks (Conventional Commits message
lint + the pre-commit checks above).

---

## Specifications

This project is developed **spec-first**: the specification is the source of truth, and behavioural changes start there. The English version is official; the Chinese translation is kept in sync.

- [specs/selftunnel-Spec.md](specs/selftunnel-Spec.md) — English (official)
- [specs/selftunnel-Spec_CN.md](specs/selftunnel-Spec_CN.md) — 简体中文

Contributions follow the same workflow — see [CONTRIBUTING.md](CONTRIBUTING.md). Notable changes are recorded in [CHANGELOG.md](CHANGELOG.md).

---

## License

[MIT](LICENSE)

# selftunnel — Software Specification v0.2.0 · Native Client Edition (for AI coding)

> This spec is self-contained: the complete project can be generated from it alone. All "MUST" items are acceptance criteria; "OPTIONAL" items may be deferred.
> Intended audience: code-generating AI or full-stack engineers. Do not introduce third-party dependencies not listed in this spec.
> v0.2.0 revision (GUI server for non-technical operators): adds `selftunnel-server-gui` — the full CLI-server feature set behind a desktop UI, with optional embedded ngrok exposure (managed as an external binary subprocess, not a Go dependency), one-click client-config generation, live client-session management and a filterable log view (§3.8). No wire-protocol changes.

## 0. Project Overview

Implement a multi-tenant reverse tunnel system:

- **Server** (single Go binary, served identically by two front ends):
  - `selftunnel-server`: headless CLI server for public hosts / containers;
  - `selftunnel-server-gui`: Fyne desktop application for non-technical operators, embedding the same server plus operator features (§3.8) — ngrok exposure, client-config generation, session management, log view;
  - Both expose only two endpoints — the tunnel intake endpoint (`/ws/tunnel`, WSS) and the REST relay endpoint (`ANY /t/{tunnelID}/*`) — and support any number of native clients connected simultaneously, dispatching requests by tunnelID. Neither has a web UI (§8).
- **Client** (native Go binary, **no administrator privileges required**):
  - `selftunnel-client`: command-line client for servers / containers / scripts;
  - `selftunnel-client-gui`: Fyne cross-platform desktop client with status, configuration and log panes;
  - Both dial out to establish a long-lived WSS connection; once connected, the server allocates (or confirms reuse of / a custom) **8-character alphanumeric tunnelID**;
  - Upon receiving a relayed request, the client forwards it to the **real intranet target address** configured by the user, and returns the response in chunks.
- To API callers, `https://server/t/{tunnelID}/{path}` is a plain REST API supporting **all HTTP methods, arbitrary headers, arbitrary body sizes and streaming responses (including SSE)**.
- Sleep prevention: while the tunnel is online, prevent the system from idle-sleeping; restore the default power policy on disconnect/exit.
- No caller-side authentication (explicit non-goal); tunnelID + intake secret are only used to prevent ID squatting (see §3.4).

## 1. Technology Stack (fixed, no substitutions)

### Server

| Item | Choice | Notes |
|---|---|---|
| Language | Go ≥ 1.25 | single binary |
| WebSocket | `github.com/gorilla/websocket` | `/ws/tunnel` endpoint |
| HTTP routing | Go 1.22+ standard library `http.ServeMux` pattern routing | no third-party router |
| Config persistence | single JSON file (`data/tunnels.json`), whole-file overwrite on write | no database |
| Logging | standard library `log/slog` | |
| GUI (optional) | `cmd/selftunnel-server-gui` | Fyne v2, operator features (§3.8) |
| Everything else | Go standard library + gorilla/websocket only | |

### Client

| Item | Choice | Notes |
|---|---|---|
| Language | Go ≥ 1.25 | single binary, cross-platform |
| CLI entry point | `cmd/selftunnel-client` | standard library `flag` for arguments |
| GUI entry point | `cmd/selftunnel-client-gui` | Fyne v2 (`fyne.io/fyne/v2`) |
| Tunnel | `github.com/gorilla/websocket` | wss:\/\/server/ws/tunnel |
| Forwarding | standard library `net/http` | |
| Config persistence | single JSON file (`config.json`), whole-file overwrite on write | same directory as the binary |
| Logging | standard library `log/slog` (CLI); callback writes into the GUI text box | |
| Sleep prevention | platform-native APIs (see §3.7) | no third-party dependencies |

### Dependency whitelist

- `github.com/gorilla/websocket`
- `fyne.io/fyne/v2` (GUI applications: client and server GUIs)
- Go standard library

ngrok is invoked as an **external binary subprocess** (§3.8.2) and is not
a Go module dependency — the whitelist above is unchanged by it.

## 2. Project Layout

The Go module lives at the repository root (standard open-source layout); specifications (this document, EN + CN) live under `specs/`.

```
(repository root)
├── go.mod
├── build.sh                         # one-command build: server + cross-platform CLI + local GUI
├── Makefile                         # fmt / vet / test / build convenience targets
├── specs/                           # specifications — the source of truth (EN + CN)
├── test/e2e/                        # loopback end-to-end tests (real server + client)
├── tests/                           # test strategy: pipeline gate scripts (smoke/coverage/crosscompile) + AI test plan
├── scripts/hooks/                   # git hooks: commit-msg lint + pre-commit checks
├── cmd/
│   ├── selftunnel-server/main.go         # server entry point
│   ├── selftunnel-server-gui/main.go     # Fyne GUI server entry point (§3.8)
│   ├── selftunnel-client/main.go         # CLI client entry point
│   └── selftunnel-client-gui/main.go     # Fyne GUI client entry point
├── internal/
│   ├── proto/
│   │   └── frame.go                 # framing protocol definition (§5)
│   ├── ngrok/
│   │   └── ngrok.go                # ngrok subprocess manager (§3.8.2): download, run, public-URL polling
│   ├── client/
│   │   ├── client.go                # client core: connect, heartbeat, reconnect, request forwarding
│   │   ├── power.go                 # sleep-prevention common interface
│   │   ├── power_windows.go         # Windows SetThreadExecutionState
│   │   ├── power_darwin.go          # macOS caffeinate
│   │   ├── power_linux.go           # Linux systemd-inhibit
│   │   └── power_unsupported.go     # fallback for other platforms
│   └── server/
│       ├── server.go                # HTTP route assembly, startup
│       ├── registry.go              # tunnel registry (in-memory + JSON persistence)
│       ├── relay.go                 # relay handler: HTTP request → frames down → response frames streamed back
│       ├── tunnel.go                # WS intake, tunnelID allocation/reuse, session read/write loops, reqId dispatch
│       └── debuglog.go              # debug request-logging middleware (-debug)
└── config.json                      # client-local config (generated at runtime)
```

## 3. Functional Requirements

### 3.1 Server: tunnel intake and tunnelID allocation (`GET /ws/tunnel`)

`GET /ws/tunnel` (WebSocket Upgrade). **Intake parameters are not passed in the URL query; instead they are carried by the first `hello` frame sent by the client after Upgrade** (keeps the secret out of access logs).

Processing flow:

1. Upgrade succeeds (`gorilla/websocket`, read/write buffers ≥ 64KB, read limit 4MB, compression negotiation disabled). Validate the `Origin` header: only the `native-client://` prefix is allowed (or whatever `-allowed-origins` explicitly configures); everything else gets `403`.
2. The `hello` frame must arrive within 10s: `{"type":"hello","version":1,"desiredId":"<8 chars or empty>","secret":"<previously held secret or empty>","clientType":"native-client","extVersion":"1.0.0"}`. On timeout → close.
3. **ID allocation logic** (in priority order):
   - `desiredId` non-empty and valid format (`^[a-z0-9]{8}$`, the server lowercases input before validating) and **not taken by someone else** (absent, or present with a secret-hash match against the registering secret) → confirm that ID;
   - `desiredId` non-empty but taken with a mismatching secret → do not return an error; **automatically fall back** to allocating a new random ID (the client can always connect; the client UI/log is responsible for the notice "custom ID unavailable, a new ID was allocated");
   - `desiredId` empty → generate a random 8-character ID (`[a-z0-9]`, `crypto/rand`, retry on collision).
4. Reply `hello_ack`: `{"type":"hello_ack","ok":true,"tunnelID":"<final ID>","secret":"<newly generated secret, delivered only on first allocation>","idReused":true|false,"desiredDenied":true|false}`.
   - **secret rules**: when an ID is first allocated, the server generates it (32 random bytes, base64url) and delivers it once with the ack; the server stores only the SHA-256 hash. The client must persist it to `config.json` and include `desiredId` + `secret` in every subsequent `hello` to prove ownership, achieving "reuse the original ID whenever it has not been given away".
   - On successful reuse (`idReused:true`) the secret is **not re-delivered** (the old value stays in effect).
   - `desiredDenied:true` means the custom ID was taken and a random ID was allocated instead.
5. Duplicate intake with the same ID: if the tunnelID already has an online session and this hello passes ownership verification → kick the old session (last-connect-wins); all of its pending requests fail with 502. This mechanism also guarantees "the same client reclaims its own ID within seconds after a restart".
6. After the session is established, enter the read/write loops (§6.1); when the session closes (for any reason) → mark offline, all pending requests fail fast with 502; outstanding `set_target` waits return 502. **Registry entries (ID → secret hash → target address) are kept forever**; offline is only a session state and never releases the ID — this is the foundation of "reuse-first".

### 3.2 Server: relay endpoint (core)

`ANY /t/{tunnelID}/*path` (`ANY` = all HTTP methods, including OPTIONS/HEAD/PATCH/custom verbs; a malformed tunnelID format gets an immediate `404`):

1. Look up the registry by tunnelID; not found → `404 {"error":"tunnel not found"}`; offline → `502 {"error":"tunnel offline"}` (**fail fast, never queue and wait**).
2. Generate a reqId (monotonically increasing uint32 within the session), build a `request_start` frame (method, path, query, headers, hasBody) and deliver it through the session's send queue:
   - **hop-by-hop headers must be stripped** (Connection, Keep-Alive, Proxy-Authenticate, Proxy-Authorization, TE, Trailer, Transfer-Encoding, Upgrade); all other headers (including Cookie, Authorization, custom headers) pass through unchanged;
   - the request body is read as a stream, chunked at **256KB** (before base64 encoding), sending `request_chunk` per chunk and `request_end` at the end; with no body, `hasBody=false`.
3. Wait for the client's response frames:
   - `response_start` (status, headers) → strip hop-by-hop headers and `Content-Length`, write the status code and headers, **Flush immediately**;
   - every `response_chunk` → base64-decode, write to the ResponseWriter and **Flush** (the key to SSE/streaming);
   - `response_end` → done; if its `error` is non-empty: if the status code has not been written, return `502 {"error":...}`; if it has, truncate the connection;
   - session dropped → return `502` if the status code has not been written.
4. Two-stage timeout (replaces the old single hard 120s):
   - **waiting for `response_start`**: no first frame within 120s → send a `cancel` frame to the client and return `504` to the caller;
   - **streaming stage**: no total-duration cap (SSE/LLM long replies can run for minutes or more); only a **300s idle timeout** reset by every received frame — as long as the stream keeps producing data it never times out; only a fully stalled stream is cut (send `cancel` to the client; if the status code was already written, truncate the connection and log a warning).
5. Each completed request increments the tunnel's `requestCount` (in-memory counter for `stats` reporting, not persisted).

### 3.3 Server: health check

`GET /healthz` → `200 {"ok":true,"tunnels":<registered>,"online":<online>}` (no ID details, to avoid information leaks).

### 3.4 Server: security boundary (kept minimal by requirement)

- The relay endpoint `/t/*` has no authentication (explicitly not wanted); security relies on the tunnelID being unguessable (8 alphanumeric chars ≈ 41 bits of entropy — a "capability URL" model; the README states that the applicable scope is trusted networks / demo environments).
- ID ownership is protected by the secret: it only prevents ID squatting/hijacking and is not an access control for callers.

### 3.5 Client: command-line client (`selftunnel-client`)

`selftunnel-client -config config.json [-server ...] [-target ...] [-custom-id ...]`

1. On startup, load `config.json` (create an empty config if it does not exist).
2. Command-line flags take precedence over the config file: `-server`, `-target`, `-custom-id`, `-insecure`, `-server-insecure`, `-client-cert`, `-client-key`, `-debug` (debug mode: print every forwarded network request).
3. Validate the server and target address formats; normalize the server address to `wss://host/ws/tunnel` (`https://` input is converted to `wss://`; a bare host gets `wss://` prefixed).
4. After connecting, print a boxed notice to the terminal containing the public entry URL: `https://<server>/t/{tunnelID}/`.
5. Status changes are printed via `log/slog`; heartbeat every 25s while online; after a disconnect, reconnect with exponential backoff (1s→2s→4s… capped at 60s, ±20% jitter), retrying forever.
6. On `SIGINT`/`SIGTERM`, shut the connection down gracefully.

### 3.6 Client: GUI client (`selftunnel-client-gui`)

A Fyne cross-platform desktop window, title "selftunnel client", initial size 720×540:

1. **Connection settings form**: server address, target address, custom tunnelID, a "skip relay server TLS verification" checkbox, a "skip target HTTPS self-signed certificate verification" checkbox, a "debug mode" checkbox (per-request logging), client certificate/key paths (optional, for target mTLS).
2. **Status bar**: connection status (Disconnected / Connecting / Online / Reconnecting), current tunnelID, "Copy ID" button.
3. **Log area**: read-only multi-line text box keeping roughly the last 500 log lines.
4. **Connect button**: shows "Disconnect" while online, "Connect" while offline.
5. Clicking Connect saves the configuration to `config.json` and starts a background goroutine running the client core; clicking Disconnect or closing the window stops it gracefully.
6. If the target address is empty, subsequent relayed requests return 502 (client-side behavior).

### 3.7 Client: sleep prevention (cross-platform)

While the tunnel is online, prevent the system from idle-sleeping; on disconnect or exit, restore the default power policy.

| Platform | Mechanism | Admin required |
|---|---|---|
| Windows | `SetThreadExecutionState(ES_CONTINUOUS \| ES_SYSTEM_REQUIRED \| ES_DISPLAY_REQUIRED)` | no |
| macOS | `caffeinate -i -w <pid>` subprocess | no |
| Linux | `systemd-inhibit --what=sleep:idle --mode=block sleep infinity` subprocess | no (allowed for regular users by default) |
| Others | no-op; does not prevent sleep | — |

- Call `PreventSleep()` after a successful connection; call `AllowSleep()` when the connection closes or the process exits.
- Failures are only logged as warnings and never affect the tunnel's main logic.
- Only **idle-timeout sleep** can be prevented; manual sleep, lid-close (if the power plan sleeps on lid close) and battery depletion still take effect.

### 3.8 Server: GUI server (`selftunnel-server-gui`)

A Fyne desktop application (title "selftunnel server", initial size
900×600) embedding the full relay server for non-technical operators:
everything the CLI server does, behind a Start/Stop button, plus the
operator features below. It is a native desktop application, not a web UI
(§8). The HTTP surface it serves is byte-identical to §4.

1. **Server lifecycle.** A settings form — listen address (default
   `127.0.0.1:8080`; selectable per network interface or `0.0.0.0` for
   direct LAN exposure), data directory (default `./data`), optional TLS
   certificate/key paths, and a debug checkbox (the `-debug` equivalent).
   Start runs the embedded `http.Server` on the chosen address; Stop
   shuts it down gracefully — in-flight relayed requests get up to 10s to
   complete, then the listener closes and all tunnel sessions are closed
   as in §3.1 step 6. Start/Stop are idempotent and reflected in a status
   bar showing the local entry-point URL.
2. **Optional embedded ngrok exposure** (for operators without a public
   server). Inputs: an ngrok authtoken (persisted in a local settings
   file, never written to logs) and the binary source — auto-download
   from the official per-platform stable URL, or a manually selected
   path. One click starts ngrok as an HTTP tunnel to the local relay
   port, polls the ngrok local agent API
   (`http://127.0.0.1:4040/api/tunnels`) for the assigned public URL,
   and displays the public entry point (`https://<ngrok-host>/t/{tunnelID}`,
   copyable). ngrok runs as a subprocess; it is stopped when the server
   stops. Failures — missing/invalid token, download failure, agent API
   unreachable — surface as a dialog plus a log entry. ngrok terminates
   TLS at its edge, so the local listener may stay plain HTTP on
   loopback in this mode.
3. **One-click client configuration.** Generates a client `config.json`
   with `server` set to the current public address (`wss://` derived
   from the ngrok or direct HTTPS address; `ws://` for plain local
   exposure); all other fields are left empty for the operator to fill
   in on the client machine. Saved through a file dialog; the public
   entry-URL template is copyable from the status bar at any time.
4. **Client session management.** A live table of all registered tunnels
   — tunnelID, remote address, connected-since, target address
   (refreshed via the existing `get_stats`/`stats` frames on connect and
   at most every 10s), forwarded request count, online/offline. Per-row
   action: **Disconnect** — closes that session (pending requests fail
   with 502, the registration and secret are kept, §3.1). Registered but
   offline tunnels remain listed.
5. **Log view.** All server logs — including the per-request debug log
   when enabled — in a read-only pane with a level filter
   (info/warn/debug) and a text-contains filter; roughly the last 1000
   lines are kept.

Explicit non-goals of the GUI: editing tunnel registrations, resetting
secrets, or any administration of the data directory beyond the settings
above (the data directory stays operator-managed).

## 4. Complete URL Route Table (server, all routes)

| Route | Purpose |
|---|---|
| `GET /ws/tunnel` | tunnel intake (WebSocket Upgrade) |
| `ANY /t/{tunnelID}/*` | relay endpoint (§3.2) |
| `GET /healthz` | health check (§3.3) |
| anything else | `404` |

Server startup flags: `-addr` (default `:8080`), `-data` (data directory, default `./data`), `-allowed-origins` (default `native-client://`, comma-separated prefixes, `*` disables the check), `-cert`/`-key` (TLS), `-debug` (debug mode: log every incoming network request, also lowers the log level to Debug). No other endpoints, no static assets, no UI.

## 5. Tunnel Framing Protocol (single WSS connection, all WS text frames, JSON encoded)

Every frame is one standalone WS text message whose body is UTF-8 JSON. Binary payloads are always base64-encoded inside string fields.

```jsonc
// ===== client → server =====
{"type":"hello","version":1,"desiredId":"ab12cd34","secret":"<base64url or empty>","clientType":"native-client","extVersion":"1.0.0"}
{"type":"pong","ts":1735689600}
{"type":"set_target_ack","ok":true}                          // or {"ok":false,"error":"invalid url"}
{"type":"stats","forwarded":128,"target":"http://192.168.1.10:8080"}   // reported on target change, or in reply to get_stats
{"type":"response_start","reqId":42,"status":200,"headers":{"Content-Type":["application/json"],"X-Any":["v"]}}
{"type":"response_chunk","reqId":42,"seq":0,"data":"<base64, raw ≤256KB>"}
{"type":"response_end","reqId":42}                           // on failure {"reqId":42,"error":"connection refused"}

// ===== server → client =====
{"type":"hello_ack","ok":true,"tunnelID":"ab12cd34","secret":"<delivered only on first allocation>","idReused":true,"desiredDenied":false}
{"type":"ping","ts":1735689600}
{"type":"set_target","target":"http://192.168.1.10:8080"}    // reserved channel: externally override the target address
{"type":"get_stats"}
{"type":"request_start","reqId":42,"method":"POST","path":"/api/users","query":"a=1&b=2","headers":{"Content-Type":["application/json"],"Authorization":["Bearer ..."]},"hasBody":true}
{"type":"request_chunk","reqId":42,"seq":0,"data":"<base64, raw ≤256KB>"}
{"type":"request_end","reqId":42}
{"type":"cancel","reqId":42}
```

Rules:

- Unknown `type` must be ignored (forward compatibility); on `version` mismatch the server replies `hello_ack{ok:false,"error":"version mismatch"}` and closes.
- **reqId is assigned by the server** (increasing uint32, skipping in-use values after wraparound); chunks of the same reqId must be processed in increasing seq order; frames of different reqIds may interleave freely.
- After `request_start`, if `hasBody=true` it is followed by some `request_chunk`s plus one `request_end`; if `hasBody=false` there are neither chunks nor an end.
- The response side is symmetric: `response_start` → some `response_chunk`s → one `response_end`; exactly one start and one end per reqId.
- At most one outstanding `set_target` at a time; the server treats an ack timeout of 10s as failure.
- `cancel` semantics: the receiver immediately stops all reads/writes for that reqId and reclaims resources, sending no further frames for it; late frames for that reqId are silently dropped.
- Maximum size of a single WS message: 4MB.

## 6. Key Implementation Notes

### 6.1 Server session read/write loops (`internal/server/tunnel.go`)

- Read loop: `ReadMessage` → `json.Unmarshal` → switch on `type`. `response_*` frames are delivered via `map[uint32]*pendingResp` (guarded by RWMutex, containing a frame channel and a `done` channel); unknown reqIds are dropped. **Never silently drop frames**: when the frame channel is full, block the sender (backpressure propagates to the tunnel client); `UnregisterPending`/session close closes `done` to abort the blocked send. A tunnel drop, idle timeout or error frame during a streaming (SSE) response is logged as a warning.
- Write loop: a single goroutine takes frames from `sendQ chan []byte` and calls `WriteMessage`; every component may only enqueue. Queue capacity is 256; when full, the oldest relayed request is failed and a warning logged (backpressure protection).
- Heartbeat: send `ping` to sendQ every 25s; `SetReadDeadline(now+90s)`, renewed on every received frame; on timeout, close the session.
- **Registry concurrency**: `registry` is a `map[string]*Tunnel` + RWMutex; `Tunnel` holds the secret hash, target address (`atomic.Value`), current session pointer (`atomic.Pointer`) and request count (`atomic.Uint64`). Kicking the old session and handing over to the new one must be atomic, so relayed requests can never be routed to a dead session.

### 6.2 Server relay handler (`internal/server/relay.go`)

Do not use `httputil.ReverseProxy` (the protocol is message-level; a byte-stream abstraction does not apply) — implement it by hand:

```go
// pseudocode skeleton
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
    id, rest := splitTunnelPath(r.URL.Path)               // /t/{id}/* → id, path
    tun := s.registry.Get(id)                             // not found → 404
    sess := tun.Session()                                 // offline → 502
    reqId := sess.nextReqID()
    ch := sess.registerPending(reqId)
    defer sess.unregisterPending(reqId)

    sess.send(Frame{Type:"request_start", ReqID:reqId, Method:r.Method,
                    Path:rest, Query:r.URL.RawQuery, Headers:filterHeaders(r.Header),
                    HasBody:r.Body!=nil})
    if r.Body != nil { streamChunks(sess, reqId, r.Body); sess.send(Frame{Type:"request_end",ReqID:reqId}) }

    start := waitFrame(ch, ctx)                           // wait for the first frame 120s; on timeout 504+cancel
    // streaming stage: 300s idle timeout (reset per frame), no total-duration cap
    writeHeaders(w, filterHeaders(start.Headers))         // strip hop-by-hop + Content-Length
    w.WriteHeader(start.Status); flush(w)
    for f := range ch {
        if f.Type=="response_chunk" { w.Write(decode(f.Data)); flush(w) }
        if f.Type=="response_end"   { return }
    }
}
```

### 6.3 Client core (`internal/client/client.go`)

- Connection state machine: `disconnected` → `connecting` → `online` → `backoff` (reconnect backoff).
- `Run(ctx)` loop: connect → hello → readLoop; on failure/disconnect → retry with exponential backoff.
- Heartbeat: send `ping` every 25s; if no frame at all arrives within 2× the heartbeat interval, consider the connection dead.
- Request forwarding: `request_start` → target `http.Client.Do` → `response_start` → stream the response body → `response_chunk` → `response_end`.
- Concurrency cap of 8 (semaphore).
- WebSocket write timeout 60s (a 256KB frame is ~350KB after base64; on a low-bandwidth tunnel 10s may not be enough, wrongly killing responses).
- On `cancel`, cancel the `http.Client` request context of the corresponding reqId.
- Config saving: after a successful connection, if the server delivered/confirmed the tunnelID and secret, write them back to `config.json`.

### 6.4 Deployment

The server serves WS and the relay endpoint on a single port. Production **must** be exposed as HTTPS/WSS via a reverse proxy (443) — under mixed-content rules browsers/clients cannot connect to `ws://` from a secure context; the README includes an nginx example (`proxy_http_version 1.1` + `Upgrade`/`Connection` headers + `proxy_read_timeout 86400s`).

Client distribution:

- `build.sh` produces:
  - `selftunnel-server` (local platform)
  - `selftunnel-server-linux` (Linux amd64)
  - `dist/selftunnel-client-{windows,darwin,linux}-{amd64,arm64}[.exe]` (cgo-free static binaries)
  - `dist/selftunnel-client-gui-{GOOS}-{GOARCH}[.exe]` (GUI client for the build platform)
  - `dist/selftunnel-server-gui-{GOOS}-{GOARCH}[.exe]` (GUI server for the build platform; same platform rules as the GUI client — see scripts/release/build.sh)
- Windows users run the `.exe` directly; macOS/Linux users run the matching binary; no installation and no administrator privileges anywhere.

### 6.5 Persistence

Server `data/tunnels.json`: `[{id, secretHash, target, createdAt}]`. Written on every change (temp file + rename). Loaded on startup. **Online status and request counters are not persisted.**

Client `config.json`: `{server, target, customId, tunnelId, secret}`.

## 7. Non-functional Requirements

- ≥ 100 concurrent tunnels; ≥ 8 concurrent relayed requests per tunnel.
- Extra relay latency: < 50ms for small requests (<64KB) on LAN/low-latency links (excluding the target's own processing time).
- No body size limit (chunked streaming; the response side must never buffer the whole body — the request side is allowed to accumulate in memory in v1, see §6.2 known limitation).
- Throughput: ≥ 10 MB/s on an intranet link (base64+JSON overhead included; an accepted cost of a message-level protocol).
- Memory: server < 5MB per tunnel when idle; CLI/GUI client < 100MB steady-state.
- Streaming reliability: SSE/streaming responses must not silently drop frames or truncate under backpressure; streams longer than 120s must arrive complete — the relay layer must never become the duration bottleneck.

## 8. Explicit Non-goals (do not build)

- Any server-side web UI, any HTML page.
- Caller authentication, user accounts, quotas, automated HTTPS certificate issuance.
- UDP, generic TCP port forwarding, any P2P/WebRTC element.
- Browser extensions, Firefox/Safari adaptations, pure web-page relaying.
- Programmatic handling of self-signed target HTTPS certificates (users trust them in their OS/browser themselves; the CLI offers an `-insecure` debug switch).

## 9. Acceptance Criteria (all must pass)

1. After `selftunnel-server` starts, only the three routes of §4 are exposed; `GET /healthz` responds normally.
2. Once the CLI client is configured with a server address and connects, the terminal prints the tunnelID and entry URL; the GUI client shows an online status and the 8-character tunnelID.
3. Restarting the client → automatic reconnect and **the same tunnelID back** (`idReused:true`).
4. Set a custom tunnelID and reconnect → get that ID; from another machine (without the secret) request the same custom ID → be downgraded to a random ID (`desiredDenied:true`) with the original owner's tunnel unaffected.
5. Multiple clients: two native clients online simultaneously, each with an independent tunnelID; `/t/{idA}/x` and `/t/{idB}/x` reach their own targets without cross-talk.
6. With target `http://<intranet HTTP service>` configured, `curl -X GET/POST/PUT/DELETE/PATCH/OPTIONS/HEAD https://server/t/{id}/any/path?q=1` all forward correctly: method, path, query, custom headers and body arrive at the target unchanged; response status, headers and body return unchanged.
7. Large files: a 500MB download through the tunnel completes with stable memory usage on both server and client; throughput ≥ 10 MB/s (intranet link).
8. Streaming: with an SSE endpoint as target, events arrive at the caller in real time (per-chunk Flush works); an SSE stream longer than 120s (e.g. 160s) arrives complete — no truncation, no dropped frames, terminal event intact.
9. Network outage of 30s then recovery: automatic reconnect with the original tunnelID restored; during the outage, relayed requests fail fast with 502 (<100ms).
10. While the tunnel is online, the system does not idle-sleep; after a manual disconnect the default power policy is restored (verify at least the current development platform among Windows/macOS/Linux).
11. Target unreachable → 502; waiting for `response_start` exceeding 120s → 504 and the client receives cancel; 300s without data during streaming → disconnect and the client receives cancel; nonexistent tunnelID → 404.
12. Concurrency: 8 simultaneous relayed requests all return correctly, interleaved reqIds never cross wires.
13. The GUI client's request log area shows recent request/status/reconnect logs; after clearing the config and reconnecting, a new tunnelID is generated.
14. The quality gate passes cleanly: `go vet ./...`; golangci-lint with
    zero findings (depguard enforcing the dependency whitelist); the full
    test suite under `go test -race` with at least 70% statement coverage
    of `internal/`; CGO-free cross-compilation of the server and CLI
    client for windows/linux/darwin on amd64 and arm64; and `build.sh`
    one-shot producing the server binary, cross-platform CLI binaries and
    the local GUI binary.
15. `selftunnel-server-gui` starts the embedded server from the settings
    form; `GET /healthz` answers and tunnels connect and relay exactly as
    against the CLI server; Stop shuts the listener down gracefully and
    sessions close per §3.1.
16. With a valid ngrok authtoken, one-click exposure yields a working
    public https entry point: a client connecting through it relays
    requests successfully; stopping the server stops the ngrok
    subprocess; an invalid token surfaces a clear error dialog.
17. The generated client config file loads in `selftunnel-client` (and
    prefills the GUI client form) and connects without edits beyond the
    target address.
18. The session table lists online clients with live counters (requests
    forwarded, connected-since, target); Disconnect drops the chosen
    session — its in-flight requests fail 502 while other tunnels are
    unaffected — and offline tunnels remain listed; the log pane filters
    by level and by text.

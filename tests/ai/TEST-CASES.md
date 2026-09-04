# AI Test Cases — selftunnel

Executable test plan for an AI agent (or a human) covering the acceptance
criteria of `specs/selftunnel-Spec.md` §9 that **cannot run in the CI
pipeline**: GUI behaviour, cross-machine flows, multi-minute streams, real
network outages, sleep prevention, TLS and throughput.

The loopback-verifiable criteria already run in CI — see
[`tests/README.md`](../README.md) for the coverage map. Do not re-run those
here unless a case explicitly says so.

## How to execute

1. Work through the cases below in order; skip a case only if its
   environment prerequisites are unavailable, and record it as **SKIP**
   with the reason.
2. Follow the steps exactly; do not improvise equivalents. Where a case
   says "record", capture the evidence (command output, screenshot,
   timings) verbatim.
3. A case passes only if **every** expected result holds. Record PASS /
   FAIL / SKIP per case in the results table at the end, with evidence
   references.
4. Environment defaults: build fresh binaries first (`./build.sh`), run
   the server as `./selftunnel-server -addr :18080 -data ./data` unless a case
   says otherwise, and use two terminals (server side / client side).

---

## AI-01 — GUI client basic flow

- **Spec ref:** §9.2, §9.13 · **Priority:** P1 · **Type:** GUI
- **Environment:** desktop with display (macOS/Windows/Linux)
- **Preconditions:** `./selftunnel-client-gui` built; relay server running.

**Steps**

1. Start `./selftunnel-client-gui`. Verify the window title is
   `selftunnel client` and the initial size is 720×540.
2. Fill Server = `ws://127.0.0.1:18080`, Target = `http://127.0.0.1:18081`
   (any local HTTP service), leave Custom ID empty. Click **Connect**.
3. Observe the status bar while connecting and after.

**Expected**

- Status cycles `Connecting…` → `Online`; the TunnelID label shows an
  8-character ID; the Connect button flips to **Disconnect**.
- **Copy ID** puts exactly the displayed ID on the clipboard.
- The log pane shows status/connection lines (roughly the last 500 lines
  are kept).
- Clicking **Disconnect** returns the UI to `Disconnected`, TunnelID `-`.

## AI-02 — GUI debug mode

- **Spec ref:** debug feature · **Priority:** P1 · **Type:** GUI
- **Preconditions:** AI-01 environment; a local target service.

**Steps**

1. In the GUI, tick **Debug mode (log every network request)**, connect.
2. `curl http://127.0.0.1:18080/t/<tunnelID>/` twice.
3. Untick the box, disconnect, reconnect, curl again.

**Expected**

- With the box ticked, the log pane shows one `forwarding request` line
  and one `request done` line (with `status=`, `respBytes=`, `duration=`)
  per request through the tunnel.
- With the box unticked, requests still succeed but no per-request lines
  appear.

## AI-03 — CLI restart reclaims the ID

- **Spec ref:** §9.3 · **Priority:** P1 · **Type:** CLI + UX
- **Preconditions:** `./selftunnel-client -server ws://127.0.0.1:18080 -target
  http://127.0.0.1:18081` run once (config.json written).

**Steps**

1. Run `./selftunnel-client`. Record the boxed output (tunnel URL).
2. Ctrl+C. Run `./selftunnel-client` again.

**Expected**

- The first run prints a box with `http://127.0.0.1:18080/t/<id>/`.
- The second run gets the **same** tunnelID; the log line
  `tunnel online` shows `reused=true`.
- Ctrl+C exits **immediately** (well under 2s; no 20s hang).

## AI-04 — Custom ID squatting from a second machine

- **Spec ref:** §9.4 · **Priority:** P1 · **Type:** cross-machine
- **Preconditions:** machine A holds custom ID `aaaaaaaa` (connect once
  with `-custom-id aaaaaaaa`); machine B (or a VM/second config dir) has
  **no** secret for it.

**Steps**

1. On A: `./selftunnel-client -custom-id aaaaaaaa` (uses persisted secret).
2. On B: `./selftunnel-client -config /tmp/fresh.json -custom-id aaaaaaaa`.
3. On B: curl its own tunnel URL; on A: curl A's tunnel URL.

**Expected**

- A keeps `aaaaaaaa` (`reused=true`).
- B is downgraded to a random 8-char ID, log shows `desiredDenied=true`,
  and B's tunnel still works (target content returns).
- A's tunnel keeps working; its ID never changes.

## AI-05 — Multi-client isolation (GUI + CLI mixed)

- **Spec ref:** §9.5 · **Priority:** P2 · **Type:** mixed
- **Steps:** run one GUI client (target A) and one CLI client (target B,
  a different local service) against the same server; curl both tunnel
  URLs alternately, several times each.
- **Expected:** every request returns the content of **its own** target;
  no cross-talk; both IDs remain distinct and stable.

## AI-06 — Adversarial request forwarding

- **Spec ref:** §9.6 · **Priority:** P2 · **Type:** exploratory
- **Steps:** against a local echo service, send through the tunnel:
  unicode paths and query values (`/echo/日本語?q=ünïcode`), very long
  headers (8KB), duplicate headers with different values, `Expect:
  100-continue`, chunked upload (`curl -H 'Transfer-Encoding: chunked'`),
  empty-body POST, and a 4MB body (near the per-frame 4MB limit is fine —
  bodies are chunked at 256KB, but confirm the frames survive).
- **Expected:** method, path, query, headers and body arrive at the target
  semantically unchanged (echo them back and diff); every response returns
  with status, headers and body intact. Record any discrepancy verbatim.

## AI-07 — 500MB throughput and memory

- **Spec ref:** §9.7 · **Priority:** P2 · **Type:** long-run
- **Steps:** serve a 500MB file locally (`dd if=/dev/zero ...`), download
  it through the tunnel with `curl -o /dev/null -w '%{speed_download}'`,
  while sampling `ps -o rss=` of both processes every 5s.
- **Expected:** download completes; speed ≥ 10 MB/s on a LAN loop; RSS of
  server and client stays roughly flat (no monotonic growth — record
  min/median/max).

## AI-08 — Long SSE stream (>120s)

- **Spec ref:** §9.8 · **Priority:** P1 · **Type:** long-run
- **Steps:** target an SSE endpoint that emits an event every 2s for at
  least 160s (write one with `curl`-able pseudo-code or use any LLM
  streaming endpoint). Consume through the tunnel with
  `curl -N http://<server>/t/<id>/sse`, timing the first byte and the last.
- **Expected:** first event arrives within ~1s of the target emitting it
  (per-chunk flush works through the relay); the stream completes with
  **zero** dropped or reordered events; total duration ≈ 160s (the relay
  never truncates long streams; idle timeout is 300s and must not fire).

## AI-09 — Real network outage and recovery

- **Spec ref:** §9.9 · **Priority:** P1 · **Type:** network
- **Steps:** connect a client from a machine/VM whose network can be cut
  (turn off Wi-Fi / disconnect the vSwitch). While online, curl the tunnel
  — then cut the network for 30s (curl during the outage and time it),
  then restore.
- **Expected:** during the outage requests fail fast with 502 (< 100ms,
  never hang); after restoration the client reconnects by itself
  (`status changed … online`), reclaims the **same** tunnelID
  (`reused=true`), and requests work again.

## AI-10 — Sleep prevention

- **Spec ref:** §9.10 · **Priority:** P2 · **Type:** platform
- **Steps:** with the tunnel online, verify the platform inhibitor is
  active: Windows `powercfg /requests` (SYSTEM entry held by the client);
  macOS `pgrep -fl caffeinate` (a `caffeinate -i -w <pid>` child); Linux
  `systemd-inhibit --list` (sleep:idle block by the client). Then
  disconnect and re-check.
- **Expected:** inhibitor present while online, gone after disconnect;
  failures here would only warrant a warning, never a tunnel failure.
  Optionally let the machine idle 10+ minutes while online to confirm it
  does not sleep.

## AI-11 — Timeout paths

- **Spec ref:** §9.11 · **Priority:** P2 · **Type:** long-run
- **Steps:**
  1. Point the client at a target that accepts connections but never
     responds (e.g. `nc -l 9999`). Curl the tunnel and time it — expect
     504 after ~120s with the client receiving a `cancel`.
  2. Target an SSE endpoint that sends one event then stalls. Expect the
     relay to cut the connection after ~300s idle (client receives
     `cancel`).
  3. Unknown tunnelID → immediate 404 (also covered in CI, re-verify
     against the deployed form).
- **Expected:** 504 at ~120s (first-frame timeout), disconnect at ~300s
  (idle timeout), 404 immediately. Record actual timings.

## AI-12 — TLS end-to-end

- **Spec ref:** §6.4, §3.5 · **Priority:** P1 · **Type:** deployment
- **Steps:** start the server with `-cert`/`-key` (self-signed pair is
  fine): it must serve WSS/HTTPS. Connect a client with `-server
  wss://<host>` (add `-server-insecure` only for the self-signed lab
  setup). Optionally front it with the nginx snippet from the README
  instead. Repeat one forwarding check through `https://`.
- **Expected:** handshake succeeds over TLS; a plain `ws://` client is
  rejected by the browser/mixed-content rules or by the server config;
  forwarding works identically over TLS.

## AI-13 — Build matrix and fresh-ID flow

- **Spec ref:** §9.14, §9.13 · **Priority:** P2 · **Type:** build
- **Steps:** run `./build.sh` on the current platform; verify every
  promised artifact exists (`selftunnel-server`, `selftunnel-server-linux`,
  `dist/selftunnel-client-*`, `dist/selftunnel-client-gui-*`). Delete `config.json`,
  reconnect, and confirm a **new** tunnelID is allocated.
- **Expected:** all artifacts present; fresh config → new random ID (no
  reuse of the old one), and the GUI log area shows the allocation.

## AI-14 — Docker images

- **Spec ref:** §6.4 · **Priority:** P2 · **Type:** docker
- **Steps:** `docker build -t tr:full .`;
  `docker build -f Dockerfile.build -t tr:builder .` (and `docker cp` the
  binary out); `./build.sh && docker build -f Dockerfile.runtime -t
  tr:runtime .`; then `docker compose up -d` and curl `/healthz` and one
  tunneled request.
- **Expected:** all three images build; the runtime image contains only
  the static binary; compose brings the server up on :8080 with data
  mounted at `./data`; healthz and one relayed request succeed.

---

## Results

| Case | Verdict | Evidence | Notes |
|---|---|---|---|
| AI-01 | | | |
| AI-02 | | | |
| AI-03 | | | |
| AI-04 | | | |
| AI-05 | | | |
| AI-06 | | | |
| AI-07 | | | |
| AI-08 | | | |
| AI-09 | | | |
| AI-10 | | | |
| AI-11 | | | |
| AI-12 | | | |
| AI-13 | | | |
| AI-14 | | | |

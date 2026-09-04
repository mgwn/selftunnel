# Testing

This project verifies the acceptance criteria of
[`specs/selftunnel-Spec.md`](../specs/selftunnel-Spec.md) §9 with two
complementary tracks:

| Track | Location | Runs where | Scope |
|---|---|---|---|
| **Pipeline (automated)** | Go tests + `tests/pipeline/smoke.sh` | CI on every push/PR, `make test`, `make smoke` | Everything that can be verified on loopback within seconds |
| **AI / exploratory** | [`tests/ai/TEST-CASES.md`](ai/TEST-CASES.md) | Executed by an AI agent or a human against a real environment | GUI behaviour, cross-machine flows, multi-minute streams, network outages, sleep prevention, TLS, throughput |

## Pipeline track

- **Unit tests** (`go test ./internal/...`) cover the registry (ID
  allocation, secret ownership, persistence), relay helpers (path splitting,
  hop-by-hop header filtering, status writing), the debug-log middleware,
  the frame codec, and client-side config/URL utilities.
- **End-to-end tests** (`go test ./test/e2e/`) wire the real server and
  client packages together over loopback: full method/header/query/body
  forwarding, response passthrough, 1MB binary roundtrip, SSE streaming
  with pacing, 16-way concurrency without cross-talk, fast-fail 502 when a
  client goes offline, 502 on unreachable targets, 404 on unknown tunnels,
  tunnelID reuse after restart, custom-ID squatting denial, two-client
  isolation, and the `-debug` request log.
- **Smoke script** (`tests/pipeline/smoke.sh`) builds the actual binaries,
  runs them as real processes with a Python HTTP target, and checks
  responses **plus the debug log output** of both sides — and that logging
  stays silent without `-debug`. This is the automated twin of the
  original manual smoke test.

Run everything locally:

```bash
make test     # go test ./...
make smoke    # tests/pipeline/smoke.sh (needs curl + python3)
```

## Coverage map (spec §9 → tests)

| Spec §9 criterion | Pipeline coverage | AI case |
|---|---|---|
| 1. Three routes only, healthz | `TestE2ERelay/healthz`, `/unknown-route-404` | AI-14 (docker) |
| 2. CLI prints tunnelID/URL, GUI online | — (GUI/UX) | AI-01, AI-02 |
| 3. Restart reclaims tunnelID | `TestE2EIDReuse` | AI-03 |
| 4. Custom ID; outsider denied | `TestE2ECustomIDDenied` | AI-04 (second machine) |
| 5. Multi-client isolation | `TestE2EClientIsolation` | AI-05 (GUI + CLI mixed) |
| 6. All methods/headers/bodies forwarded | `TestE2ERelay/method-*`, `/binary-roundtrip-1mb` | AI-06 (adversarial) |
| 7. 500MB download, ≥10MB/s | 1MB variant only | AI-07 |
| 8. >120s SSE stream | 5-event variant only | AI-08 |
| 9. 30s outage → reconnect + fast-fail | `TestE2EOfflineFastFail` (fast-fail) | AI-09 (real outage) |
| 10. Sleep prevention | — (platform) | AI-10 |
| 11. 502 / 504 / idle-timeout / 404 | `TestE2ETargetUnreachable`, `/unknown-tunnel-404` | AI-11 (timeouts) |
| 12. 8 concurrent requests, no cross-talk | `TestE2ERelay/concurrency-16` | — |
| 13. GUI log area, fresh ID after reset | — (GUI) | AI-01, AI-13 |
| 14. vet/build/build.sh clean | CI (`go vet`, `go build`, image build) | AI-12 (full matrix) |

Debug feature (`-debug`) regressions are covered on both tracks:
`TestLogRequestsLogsEveryRequest`, `TestE2EDebugServer`, and both phases of
the smoke script.

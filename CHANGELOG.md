# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-09-04

Initial release, published under the name **selftunnel** and implementing
specification v0.1.0.

### Added

- Multi-tenant reverse tunnel server (`selftunnel-server`): WebSocket tunnel
  intake with 8-character tunnelID allocation, ownership secrets, custom IDs,
  and a streaming relay endpoint (`ANY /t/{tunnelID}/*`) with SSE support.
- Command-line client (`selftunnel-client`) with automatic reconnect,
  exponential backoff, persistent ID reuse and prompt SIGINT/SIGTERM
  shutdown.
- Cross-platform GUI client (`selftunnel-client-gui`, Fyne).
- Cross-platform sleep prevention (Windows `SetThreadExecutionState`,
  macOS `caffeinate`, Linux `systemd-inhibit`).
- mTLS client certificates for the target service.
- Two-stage relay timeouts (120s waiting for the first response frame, 300s
  idle timeout during streaming) and backpressure-safe frame routing —
  streaming responses are never silently dropped or truncated.
- Debug observability: `-debug` on the server and CLI client, plus a
  "Debug mode" checkbox in the GUI. When enabled, every network request is
  printed on both sides — arrival (method, path/URL, peer) and completion
  (status code, byte count, duration) — and logging stays completely silent
  by default.
- Two-track test infrastructure mapped to the spec's acceptance criteria
  (§9): Go unit tests, loopback end-to-end tests and a real-binary smoke
  script (all wired into CI, `make test` / `make smoke`), plus an
  AI-executable test plan for GUI, cross-machine and long-running
  scenarios (`tests/ai/TEST-CASES.md`).
- Engineering governance: MIT `LICENSE`, `CONTRIBUTING.md`, `Makefile`, a
  GitHub Actions CI workflow, Conventional Commits enforcement with
  version-controlled git hooks (`make hooks`), and `AGENTS.md` — binding
  rules that keep spec, tests and code synchronized when developing with
  AI agents.
- Bilingual documentation in a standard open-source layout: English is
  official (`README.md`, `specs/selftunnel-Spec.md`) with synchronized
  Chinese counterparts; all code comments, UI strings and log messages
  are English.

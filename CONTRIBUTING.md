# Contributing to selftunnel

Thanks for your interest in contributing! This project follows a
**spec-driven development (SDD)** workflow: the specification is the source
of truth, and the implementation is verified against it.

## Repository layout

```
├── README.md / README_CN.md   # user-facing documentation (English is official)
├── specs/                     # specifications — the source of truth
│   ├── selftunnel-Spec.md        # English (official)
│   └── selftunnel-Spec_CN.md     # Chinese translation
├── cmd/                       # entry points: selftunnel-server, selftunnel-client, selftunnel-client-gui
├── internal/                  # implementation packages (proto, client, server)
├── test/e2e/                  # loopback end-to-end tests (real server + client)
├── tests/                     # test strategy: pipeline smoke script + AI test plan
├── scripts/hooks/             # version-controlled git hooks (make hooks)
├── build.sh                   # one-command build for all binaries
├── Makefile                   # fmt / vet / test / build convenience targets
├── Dockerfile*                # server images (multi-stage, build-only, runtime-only)
└── docker-compose*.yml        # deployment examples
```

## The SDD workflow

1. **Start from the spec.** Read `specs/selftunnel-Spec.md` (§9 lists the
   acceptance criteria). If a change alters behaviour, update the spec first —
   both the English and the Chinese version — including a short rationale.
2. **Implement.** Keep the technology stack and dependency whitelist fixed
   (spec §1: Go standard library + `gorilla/websocket`; Fyne for the GUI
   only). Do not add third-party dependencies without a spec change.
3. **Verify.** Every acceptance criterion your change touches must still pass.
   At minimum:

   ```bash
   make vet          # go vet ./...
   make lint         # golangci-lint (depguard enforces the dependency whitelist)
   make test         # go test ./... (unit + end-to-end)
   make coverage     # full suite under -race + >=70% coverage floor
   make crosscompile # CGO-free builds for all supported platforms
   make smoke        # tests/pipeline/smoke.sh — real binaries + debug logs
   make build        # build.sh — all binaries
   ```

   The coverage map in [`tests/README.md`](tests/README.md) shows which spec
   §9 criteria are verified automatically; criteria that need a GUI, a
   second machine, or long-running streams are listed as manual cases in
   [`tests/ai/TEST-CASES.md`](tests/ai/TEST-CASES.md) — run the ones your
   change touches.

4. **Record it.** Add an entry under `[Unreleased]` in `CHANGELOG.md`
   (Keep a Changelog format).

## Development setup

- Go ≥ 1.25 (builds and `make lint` automatically use the toolchain pinned
  in `go.mod`: go1.26.8 — golangci-lint requires it).
- Building the GUI client requires cgo (mingw-w64 when cross-compiling for
  Windows); the server and CLI clients build without it.
- For a local end-to-end smoke test:

  ```bash
  ./selftunnel-server -addr :18080 -data ./data -debug &
  ./selftunnel-client -server ws://127.0.0.1:18080 -target http://127.0.0.1:18081 -debug
  curl http://127.0.0.1:18080/t/{tunnelID}/hello
  ```

## Code and comment style

- Every exported identifier (and non-obvious unexported ones) carries an
  English doc comment starting with the identifier name. Include the
  behavioral contract, a spec section reference where applicable (e.g.
  `spec §3.2`), and concurrency guarantees. One doc comment serves both
  human and AI readers — do not split audiences.
- Test functions carry BDD doc comments: one line naming the verified
  criterion, then `Given` / `When` / `Then`. Tests stay Go-native
  (table-driven, `t.Run` subtests); no BDD framework.
- UI strings, log messages and code comments are English. Documentation is
  English (official) with a synchronized Chinese counterpart.
- **AI agents additionally follow [`AGENTS.md`](AGENTS.md)** — the binding
  checklist for spec/test/code synchronization, mandatory verification
  commands and test-record updates.

## Pull requests

- One logical change per PR, with a clear link to the spec section it
  implements or modifies.
- Keep the CLI and GUI clients in feature parity where the spec demands it.
- Run `make vet test` before submitting; CI runs the same checks.
- Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org/):
  `<type>(<scope>)?!?: <subject>` with type one of
  `feat fix docs style test refactor perf build ci chore revert`
  (e.g. `feat(server): add -debug request logging`). Keep the subject in
  the imperative mood, concise, lowercase after the colon.
- After cloning, run `make hooks` once: it installs the version-controlled
  hooks from `scripts/hooks/` — a `commit-msg` lint for the format above
  and a six-stage `pre-commit` gate (spec-sync, gofmt, `go vet`,
  golangci-lint, race+coverage, cross-compilation, smoke) that runs
  whenever Go files are staged. Docs-only commits skip the Go stages;
  individual stages can be skipped with `SKIP_LINT` / `SKIP_RACE` /
  `SKIP_XCOMPILE` / `SKIP_SMOKE`.
- **Spec ↔ code consistency is enforced:** a commit that stages Go changes
  without a `specs/` change is rejected unless you assert behaviour-
  neutrality with `SPEC_SYNC=1`. Every PR must name the spec section it
  implements or modifies — both the English and the Chinese spec are
  updated in the same change.

## Reporting issues

Include the selftunnel version (`git rev-parse --short HEAD`), the platform,
the relevant log output (run both sides with `-debug`), and — for protocol
problems — the frames involved. Never post your `config.json` secret publicly;
a fresh tunnel ID can be allocated by deleting `config.json`.

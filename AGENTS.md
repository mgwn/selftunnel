# AGENTS.md — Operating Rules for AI Coding Agents

This file is the binding checklist for any AI agent (or human contributor
using AI assistance) that changes this repository. It exists to keep the
specifications, the tests and the code in sync, and to guarantee that every
change is actually verified. Read it fully before your first change; follow
it on every change.

## Project in one paragraph

`selftunnel` is a multi-tenant reverse tunnel system in Go: a public
`selftunnel-server` exposes `ANY /t/{tunnelID}/*` and forwards requests over a
WebSocket to a `selftunnel-client` (CLI + Fyne GUI) running next to the real
intranet target. The full contract lives in
[`specs/selftunnel-Spec.md`](specs/selftunnel-Spec.md) — §9 is the
acceptance criteria list.

## Source of truth and sync rules (SDD)

1. **The spec is the contract.** `specs/selftunnel-Spec.md` (English,
   official) and `specs/selftunnel-Spec_CN.md` (Chinese) must never
   diverge: any spec edit is made to **both files in the same change**.
2. **Behavior changes are spec-first.** A change that alters externally
   visible behavior (endpoints, flags, frames, timeouts, error codes, UI)
   starts with a spec edit — including a one-line rationale — then the
   implementation, then tests, then CHANGELOG. Never let the code drift
   ahead of the spec.
3. **Pure refactors and performance work** (no behavior change) do not
   touch the spec, but every test must stay green.
4. **Dependency whitelist (spec §1) is absolute**: Go standard library +
   `github.com/gorilla/websocket` + `fyne.io/fyne/v2` (GUI client only).
   Adding anything else requires a spec change first. The whitelist is
   machine-enforced by depguard (`make lint`).
5. **Documentation set to keep in sync** for user-visible changes:
   `README.md` + `README_CN.md` (always together), `CHANGELOG.md`
   (`[Unreleased]` section, Keep a Changelog style), and the coverage map
   in `tests/README.md` if the test surface moved.

### What to update when you change X

| You changed … | You must also update … |
|---|---|
| Wire protocol / frames | spec §5 (EN + CN), tests in `internal/proto` |
| Server endpoints or flags | spec §4, both READMEs, `tests/pipeline/smoke.sh` if observable |
| Client flags / GUI behaviour | spec §3.5–§3.6, both READMEs |
| Anything an acceptance criterion depends on | spec §9 + the matching pipeline test or AI test case |
| Test surface (added/renamed/moved a test) | coverage map in `tests/README.md` |
| Anything user-visible | `CHANGELOG.md` |

## Verification — the definition of done

A change that touches Go code, the module files, or the build/test
scripts is done only when **all** of the following pass:

```bash
gofmt -l .                            # must print nothing
go vet ./...                          # must pass
make lint                             # golangci-lint: 0 issues (depguard
                                      #   enforces the dependency whitelist)
bash tests/pipeline/coverage.sh       # full suite under -race, with a
                                      #   >=70% coverage floor on ./internal/...
bash tests/pipeline/crosscompile.sh   # CGO-free server+client builds on
                                      #   windows/linux/darwin, amd64+arm64
bash tests/pipeline/smoke.sh          # real-binary smoke: SMOKE PASS
```

CI additionally runs `govulncheck` (`make vuln`).

How these run — do not double-execute:

- **If the next step is a commit, just commit.** The pre-commit hook runs
  every check above automatically whenever Go files are staged (see the
  next section); do not run them again by hand beforehand. A commit that
  succeeded is itself proof the checks passed.
- **Run them manually** only when finishing without committing (leaving
  changes for review), on a fresh clone before `make hooks` has been run,
  or to re-verify after fixing something the hook rejected.
- Documentation-only changes (no `.go`, `go.mod`, `go.sum` involved) need
  none of the Go checks — the hook applies the same gating.

### Spec ↔ code consistency (enforced)

Behaviour lives in exactly two places — the spec (EN + CN) and the code —
and they must never diverge. This is enforced, not merely requested:

- The pre-commit hook's **spec-sync gate** rejects any commit that stages
  Go changes without a `specs/` change. If the change is provably
  behaviour-neutral (pure refactor, comments, tests only), assert that
  explicitly with `SPEC_SYNC=1 git commit …`; using that flag to land an
  unspecified behaviour change is itself a spec violation.
- You must be able to name the spec section your change implements or
  modifies — in the commit message or PR. If you cannot, the change is
  not specified: write the spec first (rule 2 above).
- The EN and CN spec versions are updated in the same commit (rule 1);
  a change touching only one is rejected in review.

Rules of honesty:

- **Never claim a check passed without having seen it pass in this
  change** — your own run or the hook's output both count.
- If a check fails, either fix the code or explicitly report the failure
  with its output — never silently drop a failing check.
- If a check cannot run in your environment (e.g. no Docker for an AI
  case), record it as **SKIP with the reason** instead of pretending it
  passed.

## Commit messages and git hooks

**Format (Conventional Commits, enforced by the commit-msg hook):** the
subject line must be `<type>(<scope>)?!?: <subject>` with type one of
`feat fix docs style test refactor perf build ci chore revert`; the scope
(e.g. `server`, `client`, `gui`) and the breaking-change marker `!` are
optional. Examples:

```
feat(server): add -debug request logging
fix(client): unblock pending reads on context cancellation
docs: expand the testing section in the README
refactor(server)!: split the relay timeout into two stages
```

**Hooks (run once after cloning):** `make hooks` points
`core.hooksPath` at the version-controlled `scripts/hooks/`:

- `commit-msg` — rejects non-conventional subjects (merge commits and
  automated reverts are exempt).
- `pre-commit` — six stages, run automatically whenever Go files are
  staged: the spec-sync gate (see above), gofmt, `go vet`, golangci-lint
  (pinned to v2.13.2, installed into `.gobin/` on first run), the
  race+coverage gate and cross-compilation, and the smoke test. A
  successful commit is itself proof the checks passed. Stage skips:
  `SPEC_SYNC=1`, `SKIP_LINT=1`, `SKIP_RACE=1`, `SKIP_XCOMPILE=1`,
  `SKIP_SMOKE=1`; `git commit --no-verify` bypasses everything —
  discouraged, and CI runs the same checks on push anyway.

Agents must not disable or work around the hooks to land a change; if a
check fails, fix the change.

## Test records

- **Pipeline track** (`go test ./...`, smoke script): CI runs the same
  commands on push/PR. For local work, your final report must include the
  actual outputs — from your own runs or from the pre-commit hook; the
  commit itself is the durable record.
- **AI track** ([`tests/ai/TEST-CASES.md`](tests/ai/TEST-CASES.md)): after
  executing any case, update its row in the **Results** table at the end
  of that file — verdict (PASS / FAIL / SKIP), date, evidence reference,
  notes. A case whose steps or expectations you changed must have the case
  body updated too, not just the result.
- A FAIL anywhere blocks the change: fix it or surface it to the human
  before finishing.

## Code and comment style

- **Doc comments on every exported identifier** (and non-obvious
  unexported ones), in English, starting with the identifier name. Include:
  the behavioral contract, the spec section reference (e.g. `spec §3.2`),
  and concurrency guarantees (e.g. "safe for concurrent use"). These
  comments serve both humans and agents — do not split them into separate
  audiences.
- **Test functions carry BDD doc comments**: one line stating which spec
  criterion (or behavior) they verify, then `Given` / `When` / `Then`
  lines. Keep tests Go-native (table-driven, `t.Run` subtests) — no BDD
  framework, no new dependencies.
- **UI strings and log messages are English.** Documentation is English
  (official) with a synchronized Chinese counterpart.
- Match the surrounding code's naming and idiom; comments state contracts
  and constraints, never narrate the obvious.

## Things that must never be committed

`config.json` (contains the ownership secret), `data/` (runtime state),
built binaries, `dist/`, OS/editor junk, and local AI-tool state
(`.claude/`, `.cursor/`, `.codex/`, `.ok/`, `.mcp.json`). The root
`.gitignore` already excludes them — do not work around it.

## Quick orientation

| Path | Content |
|---|---|
| `specs/` | the contract (EN official + CN) |
| `cmd/selftunnel-server`, `cmd/selftunnel-client`, `cmd/selftunnel-client-gui` | entry points |
| `internal/proto` | frame codec (spec §5) |
| `internal/server` | registry, relay handler, sessions, debug log |
| `internal/client` | client core, sleep prevention |
| `test/e2e/` | loopback end-to-end tests |
| `tests/pipeline/*.sh` | pipeline gates: smoke / coverage (race) / crosscompile |
| `tests/ai/TEST-CASES.md` | manual/AI test plan + results table |
| `tests/README.md` | spec §9 → test coverage map |
| `scripts/hooks/` | version-controlled git hooks (commit-msg lint, pre-commit checks) |

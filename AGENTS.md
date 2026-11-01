# remote-shell-mcp

Persistent SSH, SFTP, port forwarding, and Docker over MCP, served by a
long-lived daemon. Two binaries: `remote-shell-mcp` (stdio launcher the MCP
client spawns; auto-starts the daemon and proxies stdio ↔ SSE) and
`remote-shell-mcpd` (daemon holding all session/shell/forward/Docker state
on 127.0.0.1:7800). Package map: README.md "Project layout".

## Commands

- Build: `go build ./...`
- Lint: `go vet ./...`
- Unit tests: `go test -race -count=1 ./...` (same as CI)
- E2E: `go test -race -tags e2e -count=1 -timeout 10m ./test/e2e/` — needs Docker

## Single source of truth

Each fact lives in exactly one doc. Read the canonical file, not a summary:

| Topic | Canonical doc |
|-|-|
| User-facing behavior, env vars, flags, tool list, project layout | `README.md` |
| Fork divergence from upstream | `FORK.md` |
| Architectural decisions | `docs/adr/*.md` |
| Vocabulary | `GLOSSARY.md` (lazy — absent means not yet written) |

When you change code that a doc describes, update that doc **in the same
commit** — never let the docs trail the code. If a doc and the code
disagree, the doc is the intent and the code is suspect; surface the
discrepancy to the user instead of silently picking one.

This file is an index. It deliberately does not restate flag lists,
package details, or feature matrices — those live in the docs above. Add
something here only if it has no better home (build/test commands, agent
workflow rules).

## Conventions

- Platform support is macOS + Linux only (ADR-0001) — no `runtime.GOOS`
  branches for other platforms; single-implementation files are named
  `*_impl.go`, not `*_unix.go`.
- E2E tests live behind the `e2e` build tag and run real sshd/docker, so
  prefer them over mocks when changing `sshx`/`dockerx` behavior.
- Module path is still `github.com/jaenster/remote-shell-mcp` (upstream
  name kept so imports don't churn); repo/issue tracker is
  `andrewfung729/remote-shell-mcp`.

## Agent skills

### Issue tracker

Issues are tracked in this repo's GitHub Issues (`andrewfung729/remote-shell-mcp`) via the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` + `docs/adr/` at the repo root (created lazily). See `docs/agents/domain.md`.

### Fork divergence

This is a cherry-pick fork of `jaenster/remote-shell-mcp`. Before pulling an upstream fix or feature, read `FORK.md` — it lists the surfaces we've deliberately removed or changed, so you can tell whether an upstream patch is a no-op here or needs adapting.

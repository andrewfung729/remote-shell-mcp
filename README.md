# remote-shell-mcp

[![Latest release](https://img.shields.io/github/v/release/andrewfung729/remote-shell-mcp?logo=github&label=release&color=blue)](https://github.com/andrewfung729/remote-shell-mcp/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/andrewfung729/remote-shell-mcp/test.yml?branch=main&label=tests)](https://github.com/andrewfung729/remote-shell-mcp/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/andrewfung729/remote-shell-mcp?color=informational)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/andrewfung729/remote-shell-mcp?logo=go)](go.mod)

**Persistent SSH, PTY shells, and SFTP over the [Model Context Protocol](https://modelcontextprotocol.io). Open the connection once — sessions and PTY shells survive across Claude Code / Claude Desktop / Cursor / Codex CLI restarts.**

> **Fork of [jaenster/remote-shell-mcp](https://github.com/jaenster/remote-shell-mcp)** — macOS + Linux only (no Windows/FreeBSD). See [FORK.md](FORK.md) for divergences from upstream.

`remote-shell-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server that gives any MCP client (Claude Code, Claude Desktop, Cursor, OpenAI's Codex CLI, …) a real toolbox for working on remote machines — one `ssh_shell` call runs commands on per-host PTY shells with state preserved between calls, plus SFTP round-trips.

It runs as a single long-lived daemon. Your MCP client connects to the daemon's HTTP endpoint directly — there is no stdio shim to install or keep alive. The daemon outlives every client restart, every reload, every "did the connection just drop?" — so the `vim` you opened over a PTY, the keepalive on a flaky link, all keep running.

> **This fork** runs the daemon as a local-only MCP server (see
> docs/adr/0004). The stdio launcher, per-client `setup` registrar, and curl
> installer have been removed. The daemon serves the spec Streamable HTTP
> transport at `/mcp` on a fixed loopback port. Loopback-only binds are
> enforced at startup — `/mcp` needs no auth token and there is no way to
> expose the daemon on the network. The WebUI's `/api` is gated by a
> one-time-token cookie (ADR-0006).

```
┌──────────────┐   streamable HTTP    ┌────────────────────────┐
│ MCP client   │ ◀──────────────────▶ │ remote-shell-mcpd      │
│ (registers   │   127.0.0.1:47180    │ daemon: SSH sessions,  │
│  a URL)      │      /mcp            │ shells, SFTP           │
└──────────────┘                      └────────────────────────┘
```

## Features

| Area | What |
|-|-|
| **SSH** | transparent sessions — the session id is the host alias, the first call dials automatically (parallel first calls on a cold host wait for that one dial rather than failing on a half-built session). The alias must be a concrete `Host` entry in the daemon user's `~/.ssh/config` — user, hostname, port, ProxyJump chain, and auth (IdentityFile/IdentityAgent) all resolve from that entry; callers never supply credentials, and a missing or wildcard-only entry fails. keepalive + auto-reconnect always on; strict known_hosts |
| **Shells** | one `ssh_shell` tool on a per-host PTY: `cd` + env + `vim` survive between calls and client restarts; `command`/`poll`/`input`/`interrupt`/`reset`, named parallel shells (omit `shell` for `default`; a throwaway name such as `scratch` keeps a one-off off that shell). Output is the command's text plus one status line (exit code and cwd, or still running). `input` and `interrupt` wait briefly and return that same shape. Idle shells, including `default`, are closed after 30 minutes unless a command is in flight; the next call reopens the shell and the status line says cwd/env were lost. 10k head+tail truncation. Long-lived, not persistent — a daemon restart kills them (the remote bash dies with the connection). Hosts without bash are supported through a degraded fallback (`bash --norc`/`sh`, noted on the status line), and a command that ends the shell (`exit 7`, `exec …`) still returns its exit code. For state that outlives the daemon, run `tmux`/`screen` inside the shell. Shells are scoped to the MCP client that opened them: a second client on a shared session sees `shell not found`, while the WebUI sees all shells.
| **SFTP** | list / stat, a bounded text channel (read / write / exactly-once edit, 256 KiB inline budget) and bulk upload / download. Every path may be absolute, relative to the remote home, or written with `~` — the daemon expands it (SFTP itself expands nothing), and the resolved path is what read-before-edit checks compare. Read returns a line window; write truncates or appends and edit is a byte-exact single match. A file larger than the budget or not valid UTF-8 is an error that points at `ssh_download`. `chmod`, `mkdir`, `rm`, `mv` and recursive search (`find`, `grep -rn`) are shell commands, not tools |
| **Host exposure** | the WebUI decides which `~/.ssh/config` aliases agents may use. Every tool refuses an unexposed host with `host not exposed` and names the exposed aliases in the refusal — so a typo gets corrected, while the refusal still never reveals whether the asked-for alias exists in ssh config; `ssh_hosts` + `ssh_shell` are hidden until one host is on; turning a host off disconnects it immediately |
| **State** | `$XDG_CONFIG_HOME/remote-shell-mcp/state.json` holds only the exposure list + disabled tools — never sessions, hosts' details, or credentials |
| **Bind** | fixed loopback port `127.0.0.1:47180`; startup refuses non-loopback addresses and exits non-zero if the port is taken. `/mcp` needs no token; the WebUI's `/api` requires a one-time-token cookie |
| **Transports** | `/mcp` Streamable HTTP (spec remote-MCP transport) — the single MCP transport. |
| **Output** | every tool returns plain text only — no `structuredContent`, no output schema. Shell output and file contents are verbatim, followed by one short status line; small results are a sentence or one record per line. A connection failure is one fixed sentence (host, category, and a hand-off to the user) — the raw error stays on the WebUI hosts page |

## Run the daemon

There is one binary: `remote-shell-mcpd`. Build it from source (macOS / Linux, amd64 or arm64):

```
git clone https://github.com/andrewfung729/remote-shell-mcp && cd remote-shell-mcp
make run
```

`make` writes `bin/remote-shell-mcpd` with `CGO_ENABLED=0` and stamps `main.version` from `git describe` (same `-X main.version` injection as `.goreleaser.yaml`). `make run` builds that binary and starts it.

```
# Or via Go (module path kept from upstream):
go install github.com/jaenster/remote-shell-mcp/cmd/remote-shell-mcpd@latest
remote-shell-mcpd
```

On first run it binds the fixed loopback port `127.0.0.1:47180` and
writes `{addr, pid}` to `daemon.json` in the config dir so clients can
find it. Point any MCP client at `http://127.0.0.1:47180/mcp` — no
auth header needed. If the port is already taken the daemon exits non-zero;
override with `-addr`/`REMOTE_SHELL_MCP_ADDR` (loopback only — a
non-loopback address is rejected at startup).

There is no stdio launcher and no auto-spawn helper — the daemon is a
single long-lived process you start once (e.g. via your service manager or a
login item) and point every client at.

## How it compares

| Approach | Sessions | PTY shell state | SFTP | ssh_config / 1Password / agent forwarding |
|-|-|-|-|-|
| `remote-shell-mcp` | Long-lived, daemon outlives the client; auto-reconnect with backoff | Survives across MCP calls (`cd`, env, `vim`, `tail -f`) | Bounded text read / write / exactly-once edit, list, stat, upload, download — plus the shell for chmod / mkdir / rm / mv | Yes: `~/.ssh/config` is the *only* source of connection details and auth — IdentityAgent (1Password / gpg-agent) supported |
| Stdio MCPs that wrap `ssh` per call | Re-dialed for every tool call; nothing survives | None (no persistent shell) | Usually missing | Usually missing |
| Running `ssh` from a generic Bash MCP | Re-dialed per call | None | Via shelling out to `scp`/`rsync` | Manual |
| `mcp-server-fetch` / custom HTTP wrappers around the remote | n/a | n/a | n/a | n/a |

Practical consequence: `remote-shell-mcp` lets a model run a `tail -f` in a `ssh_shell` in the morning and have it still running, on the same PTY, after the client restarts twice and your laptop went to sleep.

## Why a daemon?

A stdio MCP server lives and dies with each client connection. That's fine for stateless tools but ruinous for SSH: every Claude Code reload, every `ctrl-C`, every transient shutdown, drops every session, every tunnel, every PTY. With a long-running daemon:

- An `ssh_shell` you `cd /var/log`'d into is still there when the client reconnects.
- A session that lost its TCP transport at 2am is reconnected by 2:05am — keepalive + auto-reconnect are always on, no flag needed. After `kill -9 daemon` every session and shell is gone, and the next `ssh_shell` call on an exposed host re-dials transparently.

## Configuration

The daemon takes no required flags. Environment overrides (also accepted as `-flags`):

| Variable | Effect |
|-|-|
| `REMOTE_SHELL_MCP_ADDR` | bind address (default `127.0.0.1:47180`, a fixed loopback port; loopback only) |
| `REMOTE_SHELL_MCP_STATE` | state file path |
| `REMOTE_SHELL_MCP_LOCK` | lock file path |
| `REMOTE_SHELL_MCP_HANDLE` | handle file path (where `{addr,pid}` is published) |

Defaults live in `$XDG_CONFIG_HOME/remote-shell-mcp/` (`~/.config/remote-shell-mcp/` on Linux, `~/Library/Application Support/remote-shell-mcp/` on macOS).

## Bind safety

The daemon only ever binds loopback. A non-loopback `-addr` is rejected at
startup and a taken port exits non-zero — there is no way to accidentally
expose command execution to the network. On loopback `/mcp` needs no
token; instead a single guard wraps the whole HTTP mux so a web page you visit can't
drive the daemon: the request's `Host` must be a loopback name on the bound
port (blocks DNS rebinding), any `Origin` header must be same-origin (blocks
cross-site `fetch()`), and `POST`s under `/api` must carry
`application/json` (blocks preflight-free simple requests). Native MCP
clients and `curl` send no `Origin` and pass through — which is why
`/api` adds its own cookie gate on top (next section).

The exposure list is a real boundary *for agents*, not a sandbox: an agent
running as the same OS user can still edit the state file, restart the
daemon, or read your SSH keys. Real isolation needs a separate OS user or
sandbox. And because the WebUI token prints to the daemon's stdout once,
do not point its stdout (e.g. launchd `StandardOutPath`) at a
world-readable log — that would publish the token to anyone who can read
the file.

## WebUI

Point a browser at the daemon's address (default `http://127.0.0.1:47180`)
to open the configuration/monitoring page. On first visit it asks for the
one-time token the daemon prints to its terminal at startup; entering it
mints a session cookie (the token is single-use, and restarting the daemon
regenerates it). It shows:

- **Status** — uptime, version, listen address, state-file path.
- **Sessions** — every live session with state, user@addr, last
  error, and a read-only list of its open shells (name + owning
  client). Sessions are created transparently on an agent's first
  call to an exposed host; the page offers disconnect / reconnect only.
- **MCP Clients** — every connected MCP client (name + version from the
  `initialize` handshake, transport session id, connect time).
- **SSH hosts** — concrete `Host` aliases from the daemon user's
  `~/.ssh/config` (alias, user, hostname, port, ProxyJump). Wildcard
  patterns and `Match` blocks are omitted, and key paths are not shown.
  The page always lists them, including the raw cause and time of the
  last failed connection for that alias (cleared on the next successful
  dial). Each host has its own switch: on exposes
  that alias to agents through `ssh_hosts`, which returns only the
  exposed alias names — never user, hostname, port, or other config
  details. The tool stays hidden until at least one host is exposed.
  The choice persists across restarts.
- **MCP tools** — every tool with an on/off switch, grouped by name prefix.
  The group switch turns every tool in that group on or off at once
  (mixed groups show indeterminate; clicking it turns them all on).
  Disabling a tool removes it from the MCP `tools/list` and makes calls
  fail; re-enabling restores it instantly (clients get a
  `tools/list_changed` notification). The choice persists across restarts.
  `ssh_hosts` and `ssh_shell` start hidden and turn on only while at least one host is exposed.

The page is plain HTML + vanilla JS embedded in the binary — no Node, no
bundler, no build step; `go build` is the only build. It deliberately has
**no command execution, file browsing, or terminal** — those stay MCP-only
capabilities for the agent.

## Tests

Unit tests need no Docker, no sshd and no daemon process:

```
go vet ./...
go test -race -count=1 ./...
```

They build the daemon's HTTP surface with `daemon.NewApp` and serve it from
`httptest`, so `/mcp` and `/api` are exercised exactly as the binary serves
them: the Host/Origin guard, the WebUI API and one-time token auth, exposure
gating, tool toggles and their persistence, plus the shell-output
post-processing, host-argument validation, ssh_config resolution, and the
state store and daemon lock.

End-to-end tests drive the real binary. The shell/file ones need Docker — the
first run builds a small `alpine + openssh + busybox-httpd` test image
(~20 MB); the process-boundary ones (startup exit codes, the printed WebUI
token) need only the built binary.

```
go test -race -tags e2e -count=1 ./test/e2e/
```

That suite runs the real daemon binary against a real sshd container,
driving it through the spec Streamable HTTP `/mcp` endpoint with the same
mcp-go client a real MCP client uses, and covers what a fake can't:

- SSH shells: single-call command with exit code and cwd, `cd`/`export`
  persistence, named parallel shells, timeout → `running` → `poll`,
  `interrupt`, `input` to an interactive command, `shell busy`, `reset`,
  reconnect after an sshd kill, and rc fixtures (broken rc → `shell_degraded`,
  noisy banner/PS1 → clean output)
- Exposure boundary: unexposed and revoked hosts refused by the shell and
  file tools; revoking a host closes its session and shells
- File tools keyed by `host`; the state file never holds session material
- WebUI sessions endpoint over a live session (list / reconnect / disconnect)
- Process boundary: non-loopback `-addr` rejected, port conflict exits
  non-zero, and the one-time WebUI token printed on stdout works exactly once

## Project layout

```
cmd/
  remote-shell-mcpd/   daemon entry point (the only binary)
Makefile               local build of remote-shell-mcpd into ./bin/
internal/
  sshx/                SSH manager, dial chain, PTY shells, SFTP, keepalive
  mcptools/            MCP tool registrations, debounced state flusher, tool toggler
  webui/               /api handlers + embedded single-page UI (no build step)
  state/               on-disk state (exposed hosts + disabled tools)
  daemon/              pidfile lock, handle publish, Host/Origin guard, NewApp wiring
  mcptest/             MCP client helper shared by unit and e2e tests
test/e2e/              end-to-end tests + sshd container Dockerfile
```

## License

MIT. See `LICENSE`.

## Status

Built iteratively with [Claude Code](https://claude.com/claude-code) across ~11 rounds of write/test/audit. Each audit round was an independent agent looking for bugs; every round through 11 surfaced real issues that the previous round missed, and all P0/P1 findings are tracked and fixed in-tree.

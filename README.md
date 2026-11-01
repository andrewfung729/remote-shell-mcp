# remote-shell-mcp

[![Latest release](https://img.shields.io/github/v/release/andrewfung729/remote-shell-mcp?logo=github&label=release&color=blue)](https://github.com/andrewfung729/remote-shell-mcp/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/andrewfung729/remote-shell-mcp/test.yml?branch=main&label=tests)](https://github.com/andrewfung729/remote-shell-mcp/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/andrewfung729/remote-shell-mcp?color=informational)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/andrewfung729/remote-shell-mcp?logo=go)](go.mod)

**Persistent SSH, PTY shells, and SFTP over the [Model Context Protocol](https://modelcontextprotocol.io). Open the connection once — sessions and PTY shells survive across Claude Code / Claude Desktop / Cursor / Codex CLI restarts.**

> **Fork of [jaenster/remote-shell-mcp](https://github.com/jaenster/remote-shell-mcp)** — macOS + Linux only (no Windows/FreeBSD). See [FORK.md](FORK.md) for divergences from upstream.

`remote-shell-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server that gives any MCP client (Claude Code, Claude Desktop, Cursor, OpenAI's Codex CLI, …) a real toolbox for working on remote machines — `ssh_exec`, persistent PTY shells with state preserved between calls, and SFTP round-trips.

It runs as a single long-lived daemon. Your MCP client connects to the daemon's HTTP endpoint directly — there is no stdio shim to install or keep alive. The daemon outlives every client restart, every reload, every "did the connection just drop?" — so the `vim` you opened over a PTY, the keepalive on a flaky link, all keep running.

> **This fork** runs the daemon as a local-only MCP server (see
> docs/adr/0004). The stdio launcher, per-client `setup` registrar, and curl
> installer have been removed. The daemon serves the spec Streamable HTTP
> transport at `/mcp` on a fixed loopback port. Loopback-only binds are
> enforced at startup — there is no auth token and no way to expose the
> daemon on the network.

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
| **SSH** | named long-lived sessions, multi-address try-in-order, ProxyJump-style jump hosts, `ssh-agent`/key auth only (password and key passphrase are rejected — they can't be persisted for restore), keepalive + auto-reconnect with backoff, persistent across daemon restart |
| **Persistent shells** | full PTY, `cd` + env + `vim` survive between MCP calls, write/read/resize/close, parallel-safe writes |
| **SFTP** | read / write / list / stat / mkdir / chmod (octal string) / rename / delete / upload / download, 64 MiB read cap |
| **Persistence** | session specs (no secrets) saved to `$XDG_CONFIG_HOME/remote-shell-mcp/state.json`; rehydrated on daemon startup |
| **Bind** | fixed loopback port `127.0.0.1:47180`; startup refuses non-loopback addresses and exits non-zero if the port is taken. No auth token — the loopback guard (Host/Origin check) is the boundary. |
| **Transports** | `/mcp` Streamable HTTP (spec remote-MCP transport) — the single MCP transport. |
| **Output** | tool results are JSON. |

## Run the daemon

There is one binary: `remote-shell-mcpd`. Build it from source (macOS / Linux, amd64 or arm64):

```
git clone https://github.com/andrewfung729/remote-shell-mcp && cd remote-shell-mcp
go build -o bin/remote-shell-mcpd ./cmd/remote-shell-mcpd
./bin/remote-shell-mcpd
```

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
| `remote-shell-mcp` | Long-lived, daemon outlives the client; auto-reconnect with backoff | Survives across MCP calls (`cd`, env, `vim`, `tail -f`) | Full: read / write / chmod / mkdir / rename / upload / download | Yes: resolves `~/.ssh/config`, uses `IdentityAgent` (1Password / gpg-agent), `ForwardAgent` works |
| Stdio MCPs that wrap `ssh` per call | Re-dialed for every tool call; nothing survives | None (no persistent shell) | Usually missing | Usually missing |
| Running `ssh` from a generic Bash MCP | Re-dialed per call | None | Via shelling out to `scp`/`rsync` | Manual |
| `mcp-server-fetch` / custom HTTP wrappers around the remote | n/a | n/a | n/a | n/a |

Practical consequence: `remote-shell-mcp` lets a model open a session in the morning, run a `tail -f` in a `ssh_shell` for hours, and have all of it still working after the client restarts twice and your laptop went to sleep.

## Why a daemon?

A stdio MCP server lives and dies with each client connection. That's fine for stateless tools but ruinous for SSH: every Claude Code reload, every `ctrl-C`, every transient shutdown, drops every session, every tunnel, every PTY. With a long-running daemon:

- An `ssh_shell` you `cd /var/log`'d into is still there when the client reconnects.
- An `auto_reconnect: true` session that lost its TCP transport at 2am is back by 2:05am.
- A `persistent: true` session written to disk is back after `kill -9 daemon`.

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
expose command execution to the network. On loopback there is no auth token;
instead a single guard wraps the whole HTTP mux so a web page you visit can't
drive the daemon: the request's `Host` must be a loopback name on the bound
port (blocks DNS rebinding), any `Origin` header must be same-origin (blocks
cross-site `fetch()`), and `POST`s under `/api` must carry
`application/json` (blocks preflight-free simple requests). Native MCP
clients and `curl` send no `Origin` and pass through.

## WebUI

Point a browser at the daemon's address (default `http://127.0.0.1:47180`)
to open the configuration/monitoring page. It shows:

- **Status** — uptime, version, listen address, state-file path.
- **Sessions** — every live session with state, user@addr, flags
  (persistent / auto_reconnect / forward_agent), last error, and a read-only
  list of its open shells (id + age). Connect a new session, or disconnect /
  reconnect an existing one. Editing a session = disconnect + reconnect with
  a new spec.
- **MCP tools** — every tool with an on/off switch, grouped by name prefix.
  Disabling a tool removes it from the MCP `tools/list` and makes calls
  fail; re-enabling restores it instantly (clients get a
  `tools/list_changed` notification). The choice persists across restarts.

The page is plain HTML + vanilla JS embedded in the binary — no Node, no
bundler, no build step; `go build` is the only build. It deliberately has
**no command execution, file browsing, or terminal** — those stay MCP-only
capabilities for the agent.

## End-to-end tests

Requires Docker. First run builds a small `alpine + openssh + busybox-httpd` test image (~20 MB).

```
go test -race -tags e2e -count=1 ./test/e2e/
```

The suite (30+ tests, race-clean) drives the daemon through the spec
Streamable HTTP `/mcp` endpoint using the mcp-go client (same path a real
MCP client takes) and exercises:

- Auth: ephemeral key file (suite fixture + per-test keypairs), ssh-agent
- SSH: exec, persistent PTY shell with state preservation, jump hosts, multi-address fallback, auto-reconnect after sshd kill, persistence across full daemon restart
- SFTP: mkdir / chmod / stat / rename / upload / download / delete; rejects `data` + `data_base64` set together
- Concurrency: 8 parallel writers to the same shell, 6 simultaneous reconnects, 10 parallel sessions, 12 overlapping connect/disconnect pairs
- Daemon: daemon survives client disconnect/reconnect, streamable client reconnect across a full daemon restart, 2 MiB MCP response round-trip
- WebUI/guard: Host/Origin rejection (hostile Origin + spoofed Host), JSON content-type on /api POSTs, bound-port Host check, /api status/sessions, static page, tool disable/enable removing and restoring the tool from MCP tools/list, disabled state persisting across restart

## Project layout

```
cmd/
  remote-shell-mcpd/   daemon entry point (the only binary)
internal/
  sshx/                SSH manager, dial chain, PTY shells, SFTP, keepalive
  mcptools/            MCP tool registrations, debounced state flusher, tool toggler
  webui/               /api handlers + embedded single-page UI (no build step)
  state/               on-disk session persistence/restore
  daemon/              pidfile lock, handle publish, loopback listener, Host/Origin guard
test/e2e/              end-to-end tests + sshd container Dockerfile
```

## License

MIT. See `LICENSE`.

## Status

Built iteratively with [Claude Code](https://claude.com/claude-code) across ~11 rounds of write/test/audit. Each audit round was an independent agent looking for bugs; every round through 11 surfaced real issues that the previous round missed, and all P0/P1 findings are tracked and fixed in-tree.

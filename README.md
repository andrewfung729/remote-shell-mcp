# remote-shell-mcp

[![Latest release](https://img.shields.io/github/v/release/jaenster/remote-shell-mcp?logo=github&label=release&color=blue)](https://github.com/jaenster/remote-shell-mcp/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/jaenster/remote-shell-mcp/test.yml?branch=main&label=tests)](https://github.com/jaenster/remote-shell-mcp/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/jaenster/remote-shell-mcp?color=informational)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/jaenster/remote-shell-mcp?logo=go)](go.mod)

**Persistent SSH, SFTP, and port forwarding over the [Model Context Protocol](https://modelcontextprotocol.io). Open the connection once — sessions, tunnels, and PTY shells survive across Claude Code / Claude Desktop / Cursor / Codex CLI restarts.**

> **Fork of [jaenster/remote-shell-mcp](https://github.com/jaenster/remote-shell-mcp)** — macOS + Linux only (no Windows/FreeBSD). See [FORK.md](FORK.md) for divergences from upstream.

`remote-shell-mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server that gives any MCP client (Claude Code, Claude Desktop, Cursor, OpenAI's Codex CLI, …) a real toolbox for working on remote machines — `ssh_exec`, persistent PTY shells with state preserved between calls, `-L`/`-R`/`-D` port forwards, and SFTP round-trips.

It runs as a single long-lived daemon. Your MCP client connects to the daemon's HTTP endpoint directly — there is no stdio shim to install or keep alive. The daemon outlives every client restart, every reload, every "did the connection just drop?" — so the `vim` you opened over a PTY, the SOCKS proxy you set up to reach a database, the keepalive on a flaky link, all keep running.

> **This fork is mid-pivot** to a remote-MCP server + Mac app (see
> docs/adr/0002). The stdio launcher, per-client `setup` registrar, and curl
> installer have been removed. The daemon now serves the spec Streamable
> HTTP transport at `/mcp` plus a one-shot `/rpc` POST endpoint. Loopback
> binds are unauthenticated (local clients and the embedded WebUI are
> trusted); a non-loopback bind still requires the bearer token.

```
┌──────────────┐   remote MCP (HTTP)   ┌────────────────────────┐
│ MCP client   │ ◀──────────────────▶ │ remote-shell-mcpd      │
│ (registers   │    Bearer token      │ daemon: SSH sessions,  │
│  a URL)      │                      │ port forwards          │
└──────────────┘                      └────────────────────────┘
```

## Features

| Area | What |
|-|-|
| **SSH** | named long-lived sessions, multi-address try-in-order, ProxyJump-style jump hosts, `ssh-agent`/key/password auth, keepalive + auto-reconnect with backoff, persistent across daemon restart, `ssh_clone` |
| **Persistent shells** | full PTY, `cd` + env + `vim` survive between MCP calls, write/read/resize/close, parallel-safe writes |
| **Port forwards** | `-L`, `-R`, `-D` (SOCKS5); local forwards auto-rebound after reconnect; 30s per-conn dial timeout |
| **SFTP** | read / write / list / stat / mkdir / chmod (octal string) / rename / delete / upload / download, 64 MiB read cap |
| **Persistence** | session and forward specs (no secrets) saved to `$XDG_CONFIG_HOME/remote-shell-mcp/state.json`; rehydrated on daemon startup |
| **Auth** | 32-byte random Bearer token, rotated each daemon restart, stored 0600 in the same config dir. Loopback binds skip auth by default (`-require-auth` to force); non-loopback always requires it. |
| **Transports** | `/mcp` Streamable HTTP (spec remote-MCP transport); `/rpc` one-shot JSON-RPC POST. |
| **Output** | [TOON](https://github.com/toon-format/toon) by default — compact, line-oriented format that's ~30–50% smaller than JSON for the uniform-array payloads this daemon returns (`ssh_file_list`, forward lists, etc.). `-format json` reverts to JSON for clients that prefer it. |

## Run the daemon

There is one binary: `remote-shell-mcpd`. Build it from source (macOS / Linux, amd64 or arm64):

```
git clone https://github.com/andrewfung729/remote-shell-mcp && cd remote-shell-mcp
go build -o bin/remote-shell-mcpd ./cmd/remote-shell-mcpd
./bin/remote-shell-mcpd
```

```
# Or via Go:
go install github.com/jaenster/remote-shell-mcp/cmd/remote-shell-mcpd@latest
remote-shell-mcpd
```

On first run it binds a kernel-picked loopback port, generates a bearer
token, and writes `{addr, token, pid}` to `daemon.json` in the config dir
so clients can find it. On a loopback bind, point any remote-MCP client at
`http://<addr>/mcp` — no auth header needed. For one-shot calls,
`http://<addr>/rpc` takes a plain JSON-RPC POST.

A headless/bootstrap caller can also bring a daemon up on demand via
`daemon.Ensure(binary, args)` in-process — the same helper the future Mac
app uses — but there is no stdio launcher binary to exec from an MCP host.

## How it compares

| Approach | Sessions | Tunnels (`-L`/`-R`/`-D`) | PTY shell state | SFTP | ssh_config / 1Password / agent forwarding |
|-|-|-|-|-|-|-|
| `remote-shell-mcp` | Long-lived, daemon outlives the client; auto-reconnect with backoff | All three; auto-rebound after reconnect | Survives across MCP calls (`cd`, env, `vim`, `tail -f`) | Full: read / write / chmod / mkdir / rename / upload / download | Yes: resolves `~/.ssh/config`, uses `IdentityAgent` (1Password / gpg-agent), `ForwardAgent` works |
| Stdio MCPs that wrap `ssh` per call | Re-dialed for every tool call; nothing survives | Not practical (per-call lifetime) | None (no persistent shell) | Usually missing | Usually missing | Inherits whatever the shell wrapper does |
| Running `ssh` from a generic Bash MCP | Re-dialed per call | Awkward via `nohup ssh -fN -L …`; you manage the lifetime | None | Via shelling out to `scp`/`rsync` | Manual |
| `mcp-server-fetch` / custom HTTP wrappers around the remote | n/a | n/a | n/a | n/a | n/a |

Practical consequence: `remote-shell-mcp` lets a model open a session in the morning, run a `tail -f` in a `ssh_shell` for hours, hold a `-L 5432:db:5432` tunnel for the same hours, and have all of it still working after the client restarts twice and your laptop went to sleep.

## Why a daemon?

A stdio MCP server lives and dies with each client connection. That's fine for stateless tools but ruinous for SSH: every Claude Code reload, every `ctrl-C`, every transient shutdown, drops every session, every tunnel, every PTY. With a long-running daemon:

- An `ssh_shell` you `cd /var/log`'d into is still there when the client reconnects.
- A `-L 5432:db:5432` tunnel into a remote database stays bound for the day.
- An `auto_reconnect: true` session that lost its TCP transport at 2am is back by 2:05am.
- A `persistent: true` session written to disk is back after `kill -9 daemon`.

## Configuration

The daemon takes no required flags. Environment overrides (also accepted as `-flags`):

| Variable | Effect |
|-|-|
| `REMOTE_SHELL_MCP_ADDR` | bind address (default `127.0.0.1:0`, a kernel-picked free port) |
| `REMOTE_SHELL_MCP_STATE` | state file path |
| `REMOTE_SHELL_MCP_LOCK` | lock file path |
| `REMOTE_SHELL_MCP_HANDLE` | handle file path (where `{addr,token,pid}` is published) |
| `REMOTE_SHELL_MCP_FORMAT` | tool result output: `toon` (default) or `json` |

Defaults live in `$XDG_CONFIG_HOME/remote-shell-mcp/` (`~/.config/remote-shell-mcp/` on Linux, `~/Library/Application Support/remote-shell-mcp/` on macOS).

## Auth

The daemon generates a fresh 32-byte random token on startup and publishes it in `daemon.json` (mode 0600). Requests to a **non-loopback** bind require `Authorization: Bearer <token>` (RFC 7235 case-insensitive scheme; constant-time compare) — unauthenticated requests get `401 Unauthorized`. On loopback binds auth is skipped by default (the threat model is any local process already owns you; `-require-auth` forces the check anyway). A client reads the token from the handle file before connecting, so a daemon restart that rotates the token just means re-reading the file.

## End-to-end tests

Requires Docker. First run builds a small `alpine + openssh + busybox-httpd` test image (~20 MB).

```
go test -race -tags e2e -count=1 ./test/e2e/
```

The suite (30+ tests, race-clean) exercises:

- Auth: password, key file, ssh-agent, daemon bearer token (incl. case-insensitive scheme)
- SSH: exec, persistent PTY shell with state preservation, jump hosts, multi-address fallback, clone, auto-reconnect after sshd kill, persistence across full daemon restart
- Forwards: `-L` via real `http.Get`, `-R` with the container `curl`ing back to a Go server in the test process, `-D` SOCKS5 via `golang.org/x/net/proxy`
- SFTP: mkdir / chmod / stat / rename / upload / download / delete; rejects `data` + `data_base64` set together
- Concurrency: 8 parallel writers to the same shell, 6 simultaneous reconnects, 10 parallel sessions, 12 overlapping connect/disconnect pairs
- Daemon: `daemon.Ensure` auto-spawn, daemon survives client disconnect/reconnect, 2 MiB MCP response round-trip
- Auth: 401 path with multiple `Bearer` casings, 0600 token file mode

## Project layout

```
cmd/
  remote-shell-mcpd/   daemon entry point (the only binary)
internal/
  sshx/                SSH manager, dial chain, forwards, PTY shells, SFTP, keepalive
  mcptools/            MCP tool registrations, debounced state flusher
  state/               on-disk snapshot/restore
  daemon/              pidfile lock, auth token, handle publish, /rpc endpoint, auto-spawn (Ensure)
test/e2e/              end-to-end tests + sshd container Dockerfile
```

## License

MIT. See `LICENSE`.

## Status

Built iteratively with [Claude Code](https://claude.com/claude-code) across ~11 rounds of write/test/audit. Each audit round was an independent agent looking for bugs; every round through 11 surfaced real issues that the previous round missed, and all P0/P1 findings are tracked and fixed in-tree.

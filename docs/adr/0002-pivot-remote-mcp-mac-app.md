# ADR-0002: Pivot to remote-MCP daemon + Mac app, drop stdio client surface

- **Status**: accepted
- **Date**: 2026-08-26

## Context

The upstream product is two binaries: a stdio launcher (`remote-shell-mcp`)
that an MCP client spawns, which auto-starts and proxies to a long-lived
daemon (`remote-shell-mcpd`) over SSE/HTTP. This shape exists because
historically MCP clients only spoke stdio.

Two things changed:

1. **Remote MCP servers** (Streamable HTTP transport, OAuth 2.1 auth) are now
   a first-class part of the MCP spec and supported by the major clients.
   Registering a *URL* instead of a *binary path* removes the need for a
   per-client stdio shim entirely.
2. The maintainer wants to ship this as a **Mac app** — a menu-bar wrapper
   that manages the daemon, not a curl-installed CLI pair.

Under that model, three surfaces we built are dead weight: the stdio↔SSE
proxy, the client-config registrar, and the curl installer.

## Decision

Delete the client-facing stdio surface and keep only the daemon:

- **Removed**:
  - `cmd/remote-shell-mcp` (the whole stdio launcher binary: stdio proxy,
    `setup` subcommand, one-shot CLI)
  - `internal/launcher/proxy.go` (stdio↔SSE bridge)
  - `internal/setup/` (wrote launcher commands into client MCP configs)
  - `install.sh` + `.github/workflows/install-smoke.yml` (curl-install path)
- **Kept / moved**:
  - `internal/launcher/spawn.go` → `internal/daemon/spawn.go`, renamed
    `Ensure`/`EnsureAt`. The daemon can still be auto-spawned on demand —
    the future Mac app and any remote-MCP bootstrap need this; it just no
    longer lives behind a stdio binary.
  - The daemon's `/rpc` one-shot JSON-RPC POST endpoint — this is now the
    primary client transport in e2e tests (was a side channel for the CLI).
- **e2e**: `test/e2e/mcp_client.go` rewritten to POST `/rpc` over HTTP with
  the bearer token instead of spawning a launcher subprocess. Same
  `Call`/`CallText`/`CallJSON` API; tests unchanged except the two that
  specifically exercised the stdio bridge.

## Consequences

- **One binary**: `remote-shell-mcpd`. The daemon serves `/rpc` (+ `/sse`,
  `/message`) behind bearer auth on a kernel-picked loopback port.
- **Current MCP clients cannot connect over stdio anymore.** Until a
  Streamable HTTP server + OAuth land, the only working transports are
  `/rpc` POST and the SSE endpoint — both need the bearer token, not a
  binary an MCP host can exec. This is a deliberate gap: the stdio path is
  removed *before* the remote path is built so the deleted surface can't rot.
- **Resolved since**:
  - Streamable HTTP transport: done. The daemon serves the spec transport
    at `/mcp` (mcp-go's StreamableHTTPServer) alongside one-shot `/rpc`;
    `/sse`+`/message` was removed with it.
  - Interim auth model: loopback binds are unauthenticated by default
    (local clients and the embedded WebUI are already trusted; `-require-auth`
    overrides), non-loopback binds still require the bearer token. OAuth 2.1
    remains open for real remote exposure.
- **Still unresolved**:
  - OAuth 2.1 / proper remote auth for non-loopback deployments.
  - TLS termination for non-loopback binds.
  - Mac app: thin wrapper around `remote-shell-mcpd` vs Swift-native — the
    intermediate step is an embedded WebUI + `config.toml` daemon config
    (validate the product surface before committing to a native app).
  - Whether SSH sessions/forwards dial out from the daemon's host (current
    semantics) — that question matters once the daemon may not be the
    user's own machine.
- golang.org/x/net stays a direct dep only for the e2e SOCKS5 test.

## Fork impact

This widens divergence from upstream beyond platform support (ADR-0001):
upstream keeps the stdio launcher + client registrar; this fork drops them.
Upstream patches touching `cmd/remote-shell-mcp`, `internal/launcher`, or
`internal/setup` are no-ops here. See FORK.md.

# ADR-0004: Local-only daemon with an embedded WebUI

- **Status**: accepted
- **Date**: 2026-11-02

## Context

ADR-0002 pivoted to a "remote-MCP daemon + Mac app". That direction assumed
the daemon would be exposed over the network (needing OAuth/TLS later) and
managed by a native Mac app. Neither happened. Meanwhile the daemon had no
human-facing surface at all — you couldn't see what sessions it held or
which tools it exposed without reading logs — and a verified PoC showed the
loopback listener was open to any web page: a request with a hostile
`Origin` and a `text/plain` body, and a request with a spoofed `Host`,
were both accepted (200), and `/mcp` ignored `Origin` entirely.

The product is narrower and safer than the ADR-0002 framing: a local-only
daemon that turns a remote host into local MCP tools, plus a small WebUI for
configuration and monitoring.

## Decision

- **Local-only.** The daemon binds a fixed loopback port
  (`127.0.0.1:47180`) and refuses a non-loopback `-addr` at startup. There
  is no remote-MCP exposure, no Mac app, no stdio launcher.
- **Fixed port, no fallback.** If the port is taken the daemon exits
  non-zero; there is no silent relocation to a random port (a bookmarkable
  URL is the point). `-addr`/`REMOTE_SHELL_MCP_ADDR` still override; tests
  use port 0.
- **No auth token; a Host/Origin guard instead.** The bearer token, its
  handle-file publication, `-require-auth`, and the auth middleware are
  removed. A single guard wraps the whole HTTP mux: `Host` must be a
  loopback name on the actual bound port (DNS rebinding), any `Origin` must
  be same-origin with it (cross-site `fetch()`), and `POST`s under
  `/api` must be `application/json` (blocks preflight-free simple
  requests). Requests with no `Origin` (curl, native MCP clients) pass.
- **`/mcp` is the single MCP transport.** The one-shot `/rpc` endpoint is
  deleted.
- **`/api` + embedded page.** A `webui` package serves a JSON API and a
  plain HTML/vanilla-JS page (embedded, no build step) for monitoring and
  configuration: daemon status, live sessions (disconnect/reconnect,
  read-only shell list), and per-tool on/off switches. Sessions are
  created by agents via `ssh_connect` — the page has no manual-connect
  form and the API no connect route. Its handlers call the
  SSH manager / state store / tool toggler directly — never MCP tools — so
  disabling a tool can't lock the page out. Command execution, file
  browsing, and terminal stay MCP-only.
- **Tool toggling in the state file.** A disabled-tools list lives in the
  existing state file (no new config file); disabling removes the tool from
  `tools/list` at runtime via mcp-go's `DeleteTools`/`AddTools`, and the
  choice persists across restarts.

## Considered options

- **Keep the bearer token on loopback** — rejected: a browser has nowhere to
  enter a token, so the WebUI couldn't authenticate; the Host/Origin guard
  is the right boundary for a page-serving daemon.
- **Reuse the `/rpc` endpoint for the UI** — rejected: it would expose
  command execution to the page, exactly what the WebUI deliberately omits.
- **A REST surface per tool** — rejected: it would duplicate the MCP tools;
  the API only covers what a human configures/monitors, not what an agent
  calls.

## Consequences

- A web page the user visits can no longer drive the daemon (the PoC is now
  e2e regression tests).
- Browser-based MCP clients (e.g. a web inspector) are blocked by the guard;
  native clients are unaffected — a deliberate trade-off.
- Every "Still unresolved" item in ADR-0002 is closed: OAuth and TLS are out
  of scope, the Mac app shape is moot, and "does the daemon dial out from
  the user's host" is answered yes by local-only.
- The WebUI gives the daemon the human-facing surface it lacked, without a
  config file (state file only) and without a native app.

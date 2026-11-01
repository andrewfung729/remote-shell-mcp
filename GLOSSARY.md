# Glossary

One vocabulary for the UI, docs, and code. Each term is defined once here.

## daemon

The single long-lived binary `remote-shell-mcpd`. It binds a loopback
port, holds all session/shell state, serves the MCP endpoint (`/mcp`), the
WebUI API (`/api`) and the WebUI page (`/`). There is no other binary —
no stdio launcher, no app.

## session

A named, long-lived SSH connection held by the daemon (created via
`ssh_connect` or the WebUI's connect form). A session has a state
(connecting / connected / reconnecting / disconnected), a user, one or more
addresses, and flags. It outlives any single MCP client — the whole point of
the daemon.

## shell

A persistent PTY on a session (opened via `ssh_shell_open`). A shell keeps
its working directory, environment and running programs between MCP calls —
`cd`, `vim`, `tail -f` all survive. A shell is *long-lived*: it lasts as
long as the session (or until closed), but it is not restored after a daemon
restart. Not to be confused with *persistent*.

## persistent

A flag on a **session** meaning its connection spec is written to the state
file and **rehydrated after a daemon restart** — the session dials itself
back up. `persistent: true` sessions come back; non-persistent ones
disappear on restart. Distinct from a *long-lived shell*: persistence is
about the daemon surviving a restart, not about a shell staying open. Only
`key_path`/`use_agent` auth can be persistent, because no secret is ever
stored.

## tool

An MCP tool the daemon exposes to clients (`ssh_connect`, `ssh_exec`,
`ssh_shell_*`, `ssh_file_*`, `ssh_upload`/`ssh_download`, …). The WebUI
can switch each tool on or off; a disabled tool drops out of `tools/list`
and fails calls until re-enabled. Whether a tool belongs in the set is
judged by the scope rule in ADR-0005: does it make the remote host usable as
a local tool?

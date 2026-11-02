# Glossary

One vocabulary for the UI, docs, and code. Each term is defined once here.

## daemon

The single long-lived binary `remote-shell-mcpd`. It binds a loopback
port, holds all session/shell state, serves the MCP endpoint (`/mcp`), the
WebUI API (`/api`) and the WebUI page (`/`). There is no other binary —
no stdio launcher, no app.

## session

A long-lived SSH connection held by the daemon, internal to the agent
surface: agents never create or name one — the session id *is* the host
alias and the first call on an exposed host dials it transparently. A
session has a state (connecting / connected / reconnecting /
disconnected), a user, one or more addresses, and flags. It outlives any
single MCP client — the whole point of the daemon.

## shell

A named PTY on a session (created lazily by `ssh_shell` on first use,
named by the `shell` argument, default `default`). A shell keeps its
working directory, environment and running programs between MCP calls —
`cd`, `vim`, `tail -f` all survive. A shell is *long-lived*: it lasts
as long as the session (or until `reset`), but it is not restored after a
daemon restart. Not to be confused with *persistent*.

## persistent

Removed concept (issue #14). It used to flag a **session** whose
connection spec was written to the state file and **rehydrated after a
daemon restart**. Sessions are now transparent — created on first use per
host, never stored — and the state file holds only the exposure list and
disabled tools. Distinct from a *long-lived shell*: shells never survive
a daemon restart regardless.

## tool

An MCP tool the daemon exposes to clients (`ssh_hosts`,
`ssh_shell`, `ssh_file_*`, `ssh_upload`/`ssh_download`). The WebUI
can switch each tool on or off; a disabled tool drops out of `tools/list`
and fails calls until re-enabled. Whether a tool belongs in the set is
judged by the scope rule in ADR-0005: does it make the remote host usable as
a local tool? The collapsed agent surface is recorded in ADR-0006.

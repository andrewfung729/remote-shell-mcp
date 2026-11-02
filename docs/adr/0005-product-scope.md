# ADR-0005: Product scope — remote host as local MCP tools

- **Status**: accepted
- **Date**: 2026-11-02

## Context

The codebase had drifted from its purpose. It carried features that served
other products or an abandoned direction: port forwarding, session cloning,
Docker-era state compatibility, a spawn helper for a Mac app that is no
longer planned, bearer-token auth and non-loopback binding for remote
exposure that will not happen, a one-shot RPC transport used only by tests,
an alternative TOON output format, and password / key-passphrase auth that
couldn't be persisted (so persistent sessions silently failed to restore).

A recorded rule is needed so future additions are judged consistently.

## Decision

**A capability belongs in the tool set only if it makes the remote host
usable as a local tool** — the way an agent would use a local machine: run
commands, hold a shell, move files. That is the selection filter; it is not
negotiated per feature.

Applying it removed:

- **Port forwarding** (all of `-L`/`-R`/`-D`, list, cancel, persistence)
  — it turns the daemon into an SSH management tool, not the remote host
  into a local one.
- **Session cloning, the status/snapshot/info tools** — `ssh_sessions` is the
  single place to discover sessions; the rest was overlapping surface.
- **The TOON output format** — one result format (JSON); this also removed
  the parser-default(json)/flag-default(toon) inconsistency.
- **Password and key-passphrase auth** — they cannot be persisted, so a
  persistent session using them silently failed to restore. Credentials
  are not part of the spec at all: `ssh_connect` has no `auth` object, and
  the daemon resolves auth daemon-side from `~/.ssh/config`
  (`IdentityFile` / `IdentityAgent`). A first address with no concrete
  Host entry — and therefore nothing to resolve against — fails before
  any dial.
- **The one-shot `/rpc` transport, bearer token, `-require-auth`, and
  non-loopback binding** — see ADR-0004.
- **Dead code**: the auto-spawn helper, unused JSON/SFTP/wait helpers, the
  docker CLI in the test image, the Docker-era state field.

Kept: session connect/disconnect/reconnect/list, exec, persistent shells,
jump hosts, agent forwarding, auto-reconnect, keepalive, ssh config
resolution, multi-address fallback, the persistent flag.

### Amendment — the file tool set (issue #24)

The same rule, applied to the file tools: **list, stat, bounded text read,
bounded text write, exactly-once text edit, upload, and download** are kept.
A file's mode, a directory, a delete, and a rename are **shell commands, not
tools** — `ssh_shell` can already do them, and a second tool for one job only
makes an agent pick. The removed delete tool was also the weaker of the two:
it could not remove a non-empty directory.

`ssh_file_edit` passes the rule for a reason the shell cannot match: the
shell can change a file, but it cannot check that a change hit exactly one
place in content the agent has seen, and it cannot refuse an edit of a file
that changed since that read.

The text read and write carry the shell's context cost, not the transport's
plumbing: one shared 256 KiB inline budget, a line window instead of byte
offsets, and an error that names `ssh_download` / `ssh_upload` when the
payload does not fit — never a shortened body returned as success.

## Considered options

- **Hide forwarding only in the UI** — rejected: the surface would remain in
  the product and the tool list; the rule removes the capability, not just
  its visibility.
- **Scrub passwords on persist** — rejected: a persistent session would
  silently fail to restore after restart. Rejecting the credential up front
  makes the failure loud and correctable.

## Consequences

- The MCP tool list now contains only what serves the product purpose — and,
  for files, only what the shell cannot replace.
- The state file can never hold a credential, so every persistent session is
  restorable and the file is safe to inspect/back up.
- This rule is the documented filter the repo's agent instructions point to
  when judging future additions.

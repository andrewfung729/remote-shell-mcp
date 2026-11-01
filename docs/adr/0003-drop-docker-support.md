# ADR-0003: Drop Docker support

- **Status**: accepted
- **Date**: 2026-11-01

> **Amended 2026-11-02**: two claims no longer hold after the issue-#1
> refocus. (a) "Kept: `Snapshot.docker_hosts`" — the field is now deleted;
> old state files are not migrated (unknown keys are ignored by the decoder,
> so a stale `docker_hosts` entry simply drops, with no warn-log). (b) The
> Context's framing of the product as "remote-MCP daemon + Mac app" is
> superseded by ADR-0004 — the product is a local-only daemon with a WebUI.
> The decision to remove Docker and its rationale still stand.

## Context

Upstream ships a full Docker toolbox: `internal/dockerx` (a manager that
dials the engine over unix/tcp/ssh) plus ~15 `docker_*` MCP tools
(connect, list, exec, in-container persistent shells, run, images, upload/
download). Together with the SSH side it made the daemon a
"remote-everything" box.

The maintainer's actual product — remote-MCP daemon + Mac app — needs only
the SSH/SFTP/port-forward half. Docker management isn't part of the
workflows the app will expose, and keeping it costs:

- ~1,600 lines of dockerx + ~830 lines of mcptools glue
- the moby client dependency tree (moby/api, moby/client, image-spec,
  containerd errdefs, otel transitives) — the heaviest dep cluster in go.mod
- six e2e tests that need a real Docker daemon to exercise MCP-level
  behavior the product won't ship

## Decision

Delete the entire Docker surface:

- **Removed**: `internal/dockerx/`, `internal/mcptools/docker{,_shell}.go`,
  the six Docker e2e tests, `DaemonOpts.MountDockerSocket`, and all moby/
  docker deps (via `go mod tidy`, which also dropped the go-toml orphan
  left by the stdio-surface removal).
- **Changed**: `mcptools.State.Docker` field removed; `state.Capture` /
  `state.Restore` take only the SSH manager; the `status` tool drops the
  `hosts` list.
- **Kept**: `Snapshot.docker_hosts` stays in the schema as
  `json.RawMessage` so state files written by older daemons still parse —
  Restore logs a warning with the count instead of failing. The e2e
  harness still drives the `docker` CLI for its sshd test container:
  that's test infrastructure, not product surface.

## Consequences

- The MCP tool list shrinks to SSH/SFTP/forwards/meta only — a much
  clearer product story for the WebUI to present.
- Old state.json files with docker_hosts entries parse but their hosts
  are not rehydrated (warn-logged). No migration needed: the entries are
  inert data.
- Fork divergence widens again: upstream dockerx/docker_* patches are
  no-ops here. See FORK.md.
- If Docker support ever comes back it should be re-evaluated as a
  product feature (does the Mac app need it?) rather than restored
  wholesale — likely a much smaller surface (e.g. only the ssh://
  transport path, reusing sshx).

# FORK.md — divergence from upstream

Upstream: https://github.com/jaenster/remote-shell-mcp

This fork tracks upstream via **cherry-pick / patch**, not merge. When pulling an
upstream fix or feature, check this file first: if the upstream change touches a
divergence listed below, the patch may be a no-op for us (skip it) or may need
adapting to our narrower surface.

## Divergences

### Platform support: macOS + Linux only

- **What**: dropped Windows, FreeBSD, linux/armv7, linux/riscv64. Supported
  targets: darwin amd64/arm64/universal, linux amd64/arm64.
- **Why**: single-user fork; maintainer only runs macOS and Linux. Full
  rationale: docs/adr/0001-drop-non-unix-support.md.
- **Removed**: install.ps1, internal/daemon/lock_windows.go,
  internal/launcher/spawn_windows.go, all runtime.GOOS == "windows"
  branches, //go:build !windows tags (files renamed *_unix.go ->
  *_impl.go), windows/freebsd/exotic-arch entries in .goreleaser.yaml,
  windows-latest in install-smoke CI, %APPDATA% config paths in
  internal/setup/clients.go. (internal/setup, internal/launcher, and
  install.sh were later deleted entirely — see the next divergence.)
- **Cherry-pick note**: skip upstream patches touching install.ps1,
  *_windows.go, or windows/freebsd release-matrix entries. Resolves upstream
  issue jaenster/remote-shell-mcp#1 by deletion, not fix.

### Client transport: remote-MCP daemon only, no stdio launcher

- **What**: dropped the entire stdio client surface. Only the daemon binary
  (`remote-shell-mcpd`) remains. Upstream ships `remote-shell-mcp` (stdio
  launcher → SSE proxy → daemon) plus `remote-shell-mcp setup` (writes the
  launcher into client configs) plus a curl installer. All three are gone.
- **Why**: the product direction is a remote-MCP daemon + Mac app, where a
  client registers a URL rather than exec'ing a stdio binary. Full
  rationale: docs/adr/0002-pivot-remote-mcp-mac-app.md.
- **Removed**: cmd/remote-shell-mcp, internal/launcher/proxy.go,
  internal/setup/, install.sh, .github/workflows/install-smoke.yml.
- **Later**: the auto-spawn helper (internal/daemon/spawn.go,
  `Ensure`/`EnsureAt`) was deleted — the Mac app it was kept for is no
  longer planned (see the local-only divergence below). The `/rpc`
  one-shot endpoint was also removed — `/mcp` is now the single MCP
  transport and the e2e transport. `/sse`+`/message` was replaced by the
  spec Streamable HTTP transport at `/mcp`.
- **Cherry-pick note**: upstream patches to the stdio launcher, the `setup`
  command, install.sh, or stdio-transport fixes are no-ops here — that code
  does not exist. Daemon-side fixes (sshx, mcptools, state,
  daemon's own server) still apply. Docker support was later removed
  entirely (see next divergence).

### Docker tools: removed entirely

- **What**: dropped the whole Docker surface — `internal/dockerx` (manager,
  unix/tcp/ssh dial, container/image ops, in-container shells, copy in/out)
  and the `docker_*` MCP tools it served.
- **Why**: the maintainer no longer needs Docker management through MCP;
  the surface was a large fraction of the tool list and the dependency
  tree (moby client + image-spec + otel transitives).
- **Removed**: internal/dockerx/, internal/mcptools/docker{,_shell}.go,
  Docker e2e tests, `DaemonOpts.MountDockerSocket`, moby deps.
- **Later**: the `Snapshot.docker_hosts` compatibility field is now
  deleted too — old state files are not migrated (unknown keys are ignored
  by the decoder). The e2e harness still uses the `docker` CLI to run its
  sshd test container — that's test infrastructure, not a product feature.
- **Cherry-pick note**: upstream dockerx/docker_* fixes are no-ops here.

### Product surface: local-only daemon + WebUI, narrowed tool set

- **What**: the "remote-MCP + Mac app" direction is dropped in favour of a
  local-only daemon with an embedded WebUI. The tool set is narrowed to what
  makes a remote host usable as a local tool. Full rationale:
  docs/adr/0004-local-only-daemon-webui.md and
  docs/adr/0005-product-scope.md.
- **Removed**: all port forwarding (-L/-R/-D, list, cancel, persistence);
  session cloning; the status/snapshot/info tools; the TOON output format
  (results are JSON only); password and key-passphrase auth; the `/rpc`
  endpoint, bearer token and `-require-auth`; the auto-spawn helper and
  other dead code; the Docker-era state field.
- **Added / changed**: fixed loopback port `127.0.0.1:47180` (startup
  rejects non-loopback and exits on conflict); a Host/Origin guard around
  the whole HTTP mux (replaces bearer auth on loopback); the `webui`
  package (`/api` + embedded single-page UI, no build step); per-tool
  on/off switches persisted in the state file; key-file/ssh-agent auth only.
- **Cherry-pick note**: upstream patches to forwarding, cloning, the
  removed meta tools, TOON, password/passphrase auth, the bearer token, or
  the auto-spawn helper are no-ops here. This is the fork's largest
  divergence — treat upstream "new tool"/"new transport" features with
  suspicion against the ADR-0005 scope rule.

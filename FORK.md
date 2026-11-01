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
  internal/setup/clients.go.
- **Cherry-pick note**: skip upstream patches touching install.ps1,
  *_windows.go, or windows/freebsd release-matrix entries. Resolves upstream
  issue jaenster/remote-shell-mcp#1 by deletion, not fix.

# ADR-0001: Drop Windows, FreeBSD, and exotic-arch support

- **Status**: accepted
- **Date**: 2026-08-26

## Context

This repo is a personal fork of jaenster/remote-shell-mcp. Upstream shipped
Windows as a first-class target in v0.1.6 (install.ps1, LockFileEx daemon
lock, Windows spawn-detach flags) plus FreeBSD, linux/armv7, and
linux/riscv64 release archives — eleven archives per tag. Upstream issue #1
reports install.ps1 failing on PowerShell 7 (a 302 redirect promoted to a
terminating error by $ErrorActionPreference='Stop').

The maintainer only runs macOS and Linux. Every non-unix code path is
untested, unmaintained surface that upstream fixes will keep touching.

## Decision

Drop all non-darwin/linux support:

- Delete install.ps1 and the Windows-specific implementations
  (lock_windows.go, spawn_windows.go).
- Remove all runtime.GOOS == "windows" branches and !windows build tags;
  rename *_unix.go -> *_impl.go since there is one implementation now.
- Narrow .goreleaser.yaml to darwin amd64/arm64 (+universal) and linux
  amd64/arm64; drop windows zip overrides, goarm, and ignore blocks.
- Drop windows-latest from install-smoke CI.
- Remove %APPDATA% branches in internal/setup/clients.go.

Upstream issue #1 is resolved by removing the affected surface, not by fixing
the 302 bug — the Windows installer no longer exists.

## Consequences

- No Windows/FreeBSD artifacts are produced or supported. Users on those
  platforms should use upstream.
- Cherry-picking upstream patches requires checking FORK.md: patches
  touching the removed surfaces are no-ops here.
- golang.org/x/sys drops to an indirect dependency (no more x/sys/windows
  import).
- If Windows support is ever re-added, the pre-drop code is recoverable from
  git history and upstream.

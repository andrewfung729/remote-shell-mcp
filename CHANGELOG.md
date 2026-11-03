# Changelog

All notable changes to remote-shell-mcp. Versions follow [Semantic Versioning](https://semver.org/).

## Unreleased

- **A new shell does not say it restarted.** `shell restarted: cwd/env lost` is only for an open that replaces a shell whose cwd/env are actually gone — the PTY died, `reset`, idle reap, or the session was replaced. The first open of a name, including `default` and a new parallel shell, does not carry that line.
- **Tool results are plain text (issue #27).** Every tool returns one text block and declares no output schema. `ssh_shell` output is verbatim, then one status line (`[exit 0 · cwd /srv/app]`, `[running — poll to continue]`, plus truncation, restart, degraded-shell, and the shell name only when it is not `default`). `ssh_file_read` is the lines plus `[lines a–b of N]` (and `more: start_line=K` when the window stops early). `ssh_hosts` is one alias per line; list is an `ls -l`-style line per entry (directories end in `/`); stat is `key: value`; write, edit, upload and download are one sentence. `input` and `interrupt` wait 2s by default (or `timeout_ms`) and return that same shell status, so answering a prompt or sending Ctrl-C is one call.
- **Connection failures hand off to the user (issue #27).** A failed dial is `cannot connect to "<alias>" (<category>)` plus an instruction not to retry, not to use local ssh tools, and not to edit ssh config — ask the user. The raw error is never sent to the agent. It is logged and shown on the WebUI hosts page for that alias, with the time, and cleared on the next successful connect. There is no negative cache.
- **Idle shells are reaped (issue #27).** A shell with no `ssh_shell` call for 30 minutes and no command in flight is closed, including `default`. The next call reopens it and the status line says cwd/env were lost. The `shell` parameter documents a throwaway name such as `scratch` for a one-off that must not touch the default shell.
- **Exposure refusals name the exposed aliases.** `host not exposed` now reads `host not exposed: "x" — exposed hosts: a, b`: a mistyped alias corrects itself in one message instead of sending the agent hunting through ssh config. The refusal still never reveals whether the asked-for alias exists.
- **`ssh_shell` output no longer leaks PTY escapes.** The ANSI stripper missed the two-byte escapes in the 0x30–0x3F range (`ESC=`/`ESC>` keypad modes, `ESC7`/`ESC8` cursor save/restore, `ESCD`/`E`/`M`/`c`) and n-byte charset selects (`ESC(B`, `ESC%G`), which showed up as stray `\u001b=` in captured output. All C1-adjacent two- and three-byte sequences are now stripped.

- **`ssh_shell` works on hosts without bash again.** The degraded fallback (no bash on the remote, or rc files that never come back) configured the completion marker through `PROMPT_COMMAND` — a bash-only variable that busybox ash, dash and toybox sh all ignore. The marker therefore never arrived: every `ssh_shell` command on such a host ran to its full `timeout_ms` and reported `status: running`, and a client whose own timeout matched got a transport error instead of a result. The fallback now picks its mechanism per shell — `PROMPT_COMMAND` for bash, a `PS1` command substitution for a POSIX sh (readline eats the `\x01` in the bash-form marker, so the sh form is SOH-free) — and all three of `exit_code`, `cwd` and state persistence work there. Two related shell-layer fixes fell out: a dead PTY ends the wait immediately instead of burning the caller's timeout, and a command that ends the shell (`exit 7`, `exec …`) now returns `status: done` with the shell's own exit status and the cwd it was last in, instead of hanging until timeout (`shell_restarted` follows on the next call). The bootstrap no longer prints a redundant sentinel marker either: a marker arriving right after the open was digested as the next command's result, an empty `done` that swallowed that command's output.
- **Shell state is drained, not raced.** Two streams (stdout/stderr) feed one output reader, so "the PTY is gone" now means *both* are drained — closing on whichever saw EOF first could drop output still in flight and turn a completed command into an empty result. Opening a shell is serialized per session: two concurrent first calls used to open two PTYs, silently leak one on registration and let the second caller miss the first one's `shell busy`.
- **e2e covers both shells and runs in parallel.** The sshd fixture now installs bash, so the suite exercises the primary `bash -i -l` path (rc loading, `PROMPT_COMMAND`, `read -p`) that every real host gets, with `~/.profile` sourcing `~/.bashrc` the way a normal login does. A second, bash-less image (same Dockerfile, `--build-arg WITH_BASH=0`) pins the degraded path via a new `TestShellWithoutBash`; image tags carry a hash of the Dockerfile so a changed fixture never reuses a cached image. The suite is now fully parallel (per-test container, per-test daemon port, per-test `$HOME`/`state.json`, `sync.Once` for the binary, images and suite key), starting a daemon retries on a fresh port when two tests race for the same free one, and `SweepStaleContainers` documents that it must never run inside a parallel test. Wall clock: 8m15s with 13 failures → ~35s green.
- **Paths are `~`-aware, on both ends.** Every file tool resolves a leading `~` (`~`, `~/x`) against the remote home — the SFTP server's own working directory, asked once per connection and cached until the next reconnect — so `ssh_file_list {"path": "~"}` lists the home directory instead of failing with `file does not exist`. Relative paths already started at that home and still do; `~user/…` is deliberately left literal, since another user's home lives in the remote passwd database. The resolved path is also the read-before-edit guard's key, so a read spelled `~/notes.txt` licenses an edit spelled `/home/me/notes.txt`. `ssh_upload`/`ssh_download` expand `~` on the daemon side (their `local_path`) against the daemon user's home, and both path arguments now carry the syntax in their tool description.
- **A cold host's parallel first calls share one dial.** The session is published before its dial finishes, so a second call used to answer `session "host" is connecting` and an agent that fired several tools at once had to retry. Callers now wait for the in-flight dial — bounded by the request context — and get the same session or the same dial error; the session is still created exactly once, and a failed dial leaves nothing behind. Reconnect-induced `is reconnecting` answers are unchanged.
- **Breaking: bounded text file tools, an exactly-once edit, and four shell-equivalent SFTP mutators removed (issue #24).** `ssh_file_read` and `ssh_file_write` are now the in-context text channel with one shared 256 KiB (262144-byte) budget. Read returns a **line window** — `start_line` (default 1), `max_lines` (omit or 0 for the whole file), and a result of `data`, `start_line`, `lines`, `total_lines` (`truncated` only when true; the old `bytes` field is gone). A file larger than the budget, or one that is not valid UTF-8, is a tool error naming `ssh_download` instead of a silently shortened or replacement-charred body; size is checked before UTF-8. A write whose `data` exceeds the budget is a tool error naming `ssh_upload` (the cap counts this call's data, not the file afterwards, so appending to a large file still works). The old byte parameters `offset` and `length` are **rejected by name** — even when the value is `0` or `null` — and are never reinterpreted as lines. New `ssh_file_edit` replaces `old_text` with `new_text` when it occurs exactly once, byte for byte (CRLF included, overlapping matches counted), refusing to touch a file this client has not read or written in this daemon run or one that changed since — and writing in place so owner, mode, hard links and symlinks survive. Removed from MCP registration with no aliases: `ssh_file_chmod`, `ssh_file_mkdir`, `ssh_file_delete`, `ssh_file_rename` — run `chmod`/`mkdir`/`rm`/`mv` through `ssh_shell`. `ssh_file_list`, `ssh_file_stat`, `ssh_upload` and `ssh_download` are unchanged. Every tool now checks exposure, then arguments, then dials — an unexposed host always answers `host not exposed`, `ssh_shell` included (it used to validate its action first).
- **Breaking: collapsed the agent surface to `ssh_shell` + `ssh_hosts` + file tools (issue #14, ADR-0006).** One `ssh_shell` call runs a command on a per-host PTY shell and blocks until it ends (clean output + exit code + cwd) or `timeout_ms` expires (`status: running` — `poll` follows, `input` feeds prompts, `interrupt` sends Ctrl-C, `reset` respawns a polluted shell). `cd`/`export` persist between calls; named parallel shells via `shell`. Removed from MCP registration with no backward compatibility: `ssh_connect`, `ssh_disconnect`, `ssh_sessions`, `ssh_reconnect`, `ssh_exec`, `ssh_shell_open`, `ssh_shell_write`, `ssh_shell_read`, `ssh_shell_resize`, `ssh_shell_close`, `ssh_shell_list`. File tools take `host` instead of `session_id`; the `client` parameter is gone (ownership derives from MCP clientInfo).
- **Transparent sessions; `persistent`, `insecure` and rehydration removed.** The session id is the host alias — the first call dials automatically with keepalive + auto-reconnect always on and strict known_hosts (an unknown key tells the user to ssh once themselves). Sessions are never written to disk: the state file holds only the exposure list and disabled tools, and old state files are not migrated.
- **Host exposure is now an enforced boundary.** Every tool refuses a non-exposed host with `host not exposed` (without revealing whether the alias exists in ssh config); `ssh_hosts` and `ssh_shell` are hidden from `tools/list` until at least one host is exposed; turning a host off disconnects its session and closes its shells immediately.
- **WebUI one-time-token auth (supersedes ADR-0004's "no auth token" for `/api`).** The daemon prints a random token to stdout at startup; the page exchanges it once for a 30-day HttpOnly SameSite=Strict cookie, after which the token is dead and sessions live in memory only. `/api/*` without the cookie returns 401 — an agent with `curl` can no longer mint exposure. `/mcp` stays unauthenticated; the Host/Origin guard is unchanged. Documented limit: a same-OS-user agent can still edit the state file or restart the daemon — the exposure list is an agent boundary, not a sandbox, and the daemon's stdout must not go to a world-readable log.
- **Remote shell layer.** Shells start as `bash -i -l` so rc files (nvm, conda, pyenv) load, then a bootstrap disables echo/prompt/bracketed-paste; completion is a marker carrying exit code + cwd. No first marker within 10 s → respawn on `bash --norc` (or `sh`) with `shell_degraded`. Reconnect yields `shell_restarted`. Output is ANSI/echo/marker-stripped and capped at 10,000 chars head+tail (full output via `poll`).
- **Faster tests: no-sshd coverage moved out of e2e.** The Host/Origin guard,
  the WebUI `/api` surface (status, sessions, hosts, tools, one-time token
  auth, the static page), exposure gating and tool-toggle persistence are now
  unit tests that serve the production wiring (`daemon.NewApp`) from
  `httptest`, so they run in `go test ./...` without Docker. Handler wiring
  moved out of `cmd/remote-shell-mcpd/main.go` into `internal/daemon/app.go`;
  `main` is flags, listener, lock, handle file and signals. The `e2e` suite
  keeps only what needs real sshd or the process boundary (exit codes, the
  printed WebUI token). `internal/mcptest` holds the single streamable-HTTP
  MCP client both test layers use.
- **Fixed a data race in the e2e harness.** The daemon's stdout/stderr buffers
  are now written through a mutex-guarded writer, so `-race` no longer trips
  when a test scrapes the WebUI token while the process is still writing.
- **MCP server instructions.** The initialize response now carries a short instruction: use the `ssh_*` tools here, not the local `ssh`/`scp`/`sftp`/`rsync` CLI; hosts must be exposed in the WebUI before `ssh_hosts` will name them.

- **`ssh_hosts` returns alias names only.** It no longer leaks user, hostname, port, or ProxyJump to the agent — the daemon resolves those internally. An exposed host is exactly an opaque name the agent can hand back to `ssh_shell` or the file tools as `host`.
- **Renamed `ssh_list` → `ssh_sessions` (later removed entirely with the session tool set — see the `ssh_shell` collapse above).** The tool listed live connections held by the daemon, not hosts you can dial — the old name kept being misread as "list of SSH hosts".

- **SSH host list.** The WebUI shows concrete `Host` aliases from the daemon user's `~/.ssh/config`, each with its own switch. `ssh_hosts` returns only the hosts the user turned on, and stays hidden until at least one is on. Wildcard and `Match` blocks are omitted, and key paths are not returned.
- **Refocused the product on "remote host as local MCP tools"** (issue #1;
  ADR-0004 local-only daemon + WebUI, ADR-0005 product-scope rule). See the
  sub-bullets below.
- **Local-only daemon.** Fixed loopback port `127.0.0.1:47180` (was a
  kernel-picked random port); startup rejects a non-loopback `-addr` and
  exits non-zero on port conflict — no random-port fallback. A Host/Origin
  guard now wraps the whole HTTP mux (loopback Host on the bound port,
  same-origin Origin, `application/json` required for `/api` POSTs).
- **WebUI.** New `internal/webui` package: a plain HTML/vanilla-JS page at
  `/` plus `/api` (status, sessions, connect/disconnect/reconnect, tool
  list + on/off). Per-tool toggles apply to MCP clients instantly via
  `tools/list_changed` and persist in the state file. No build step.
- **Removed** (per the ADR-0005 scope rule): all port forwarding
  (-L/-R/-D + persistence), session cloning, the status/snapshot/info
  tools, the TOON output format (results are JSON only), password and
  key-passphrase auth, the `/rpc` endpoint + bearer token +
  `-require-auth`, non-loopback binding, the auto-spawn helper, assorted
  dead code, and the Docker-era state field.
- **Auth narrowed** to key-file and ssh-agent only; the old
  `ssh_connect` rejected a password or key passphrase with a corrective
  error (that tool has since been removed with the session surface — the
  daemon now resolves auth from `~/.ssh/config` on transparent dial).
  The state file can never hold a secret.
- **Released binaries now report their real version** — the
  `-X main.version={{.Version}}` ldflag previously named a variable that
  didn't exist; `var version = "dev"` is now injected.

- **Fork: removed Docker support.** Deleted `internal/dockerx` and all `docker_*` MCP tools (connect/list/exec/shell/run/images/upload/download). `mcptools.State` drops the `Docker` field; `state.Capture`/`Restore` take only the SSH manager. `Snapshot.docker_hosts` is retained as `json.RawMessage` so old state files still parse — a warn-log notes the count and skips them. The moby/docker dependency tree is gone (`go mod tidy` also dropped the orphaned `go-toml` from the earlier setup removal).
- **Fork: dropped Windows and FreeBSD support.** This fork targets macOS and Linux only. Removed `install.ps1`, the Windows daemon lock (`LockFileEx`) and spawn-detach implementations, all `runtime.GOOS == "windows"` branches, and the Windows/FreeBSD/armv7/riscv64 release and CI matrices. Supported targets: darwin amd64/arm64/universal, linux amd64/arm64. (Resolves upstream jaenster/remote-shell-mcp#1 by removing the affected surface rather than fixing the PowerShell 302 bug — Windows install is no longer offered.)
- **Fork: removed the stdio client surface — daemon-only, heading for remote-MCP + Mac app.** Deleted `cmd/remote-shell-mcp` (stdio launcher, `setup` subcommand, one-shot CLI), `internal/launcher/proxy.go`, `internal/setup/` (client config registrar), `install.sh`, and the `install-smoke` CI. The daemon auto-spawn helper moved to `internal/daemon/spawn.go` (`Ensure`/`EnsureAt`); e2e then talked to the daemon over HTTP `/rpc` instead of spawning a stdio launcher. (Superseded: `/rpc` was later removed — `/mcp` is now the single transport, and remote auth/OAuth were dropped for a local-only Host/Origin guard; see ADR-0004.)

## v0.1.8 — 2026-05-14

- **`ssh_upload` / `ssh_download` now handle directories.** Pass a directory as the source and the tree is mirrored recursively at the destination — intermediate dirs created, file permissions preserved best-effort, symlinks skipped. Single-file behavior is unchanged. No base64, no buffering into the MCP message; bytes stream over the SSH channel via SFTP.
- **New `docker_upload` / `docker_download` tools.** Symmetric to the SSH versions, but the bytes go over the Docker engine's archive API (tar streams), not the SSH channel. Handles files or directories. Use these instead of routing payloads through `docker_exec` stdin/stdout — much faster for anything non-trivial and doesn't blow up the MCP message size.
- **Dropped the base64 paths from `ssh_file_write` / `ssh_file_read`.** `ssh_file_write` now takes `data` (text) only; `ssh_file_read`'s `base64=true` option is gone. The tools were a footgun for LLMs that would default to base64 for any binary, inflating bytes 33% and tripping JSON-RPC size limits. Descriptions now explicitly point at `ssh_upload` / `ssh_download` for binary or large content.
- **`REMOTE_SHELL_MCP_HANDLE` env var works in CLI mode too.** Was previously stdio-only. Tests and scripts that pin a sandboxed daemon path no longer need a separate `-handle` flag plumbing — just set the env.

## v0.1.7 — 2026-05-13

- **Daemon now picks a free port at startup** instead of hard-coding `127.0.0.1:7800`. The previous default was JGroups' well-known port (used by JBoss/Wildfly clustering) and could clash with anything from a local game server to a Cassandra dev cluster — symptom was a launcher that couldn't connect because something else already owned 7800. Daemon binds `127.0.0.1:0`, the kernel hands back a free port, and the actual `host:port` is written into `daemon.json` along with the bearer token and PID. The launcher reads the handle to know where to connect — no port assumptions on either side. `--addr` is still respected if you want to pin a specific port.
- **Launcher self-heals on stale daemon state.** Previous symptom: if the daemon ended up listening on the port but `daemon.token` had been removed (or any other "process alive, auth file gone" half-state), every subsequent launcher invocation would fail with "token file never appeared" — *forever*, because `EnsureDaemon` saw a live listener and refused to respawn. The new flow probes the recorded address; if the handle is missing/corrupt or the daemon isn't responsive, the launcher SIGTERMs the PID in `daemon.lock`, waits for the port to free, escalates to SIGKILL if needed, and spawns fresh. Hosed daemons no longer wedge the MCP integration.
- **Replaced `daemon.token` with `daemon.json`** (`{addr, token, pid}`). One source of truth for the launcher/daemon rendezvous; one defer to clean up. Atomic tmp-then-rename write so a partial file never gets observed.
- New tests for the handle round-trip (perms 0600, atomic replace, refuses corrupt or incomplete files).

## v0.1.6 — 2026-05-13

- **Windows is a first-class target now.** Daemon lock split into `lock_unix.go` (flock) and `lock_windows.go` (`LockFileEx` with `LOCKFILE_FAIL_IMMEDIATELY` — the moral equivalent of `flock(LOCK_EX|LOCK_NB)`). Same "already-running → error" semantics either way. Also fixed launcher's same-directory probe for the daemon binary to honor `.exe` on Windows (`exec.LookPath` already does this via PATHEXT, but a manual `os.Stat` does not).
- **Release matrix expanded** to: darwin amd64 / arm64 / universal, linux amd64 / arm64 / armv7 / riscv64, windows amd64 / arm64, freebsd amd64 / arm64. Eleven archives per tag. `linux/armv7` covers Raspberry Pi 2/3/Zero 2; `darwin_universal` is a fat Mach-O so one tarball runs on both Intel and Apple Silicon Macs.
- **`install.ps1`** mirrors `install.sh` for Windows: zip download with SHA-256 check, drops binaries into `%LOCALAPPDATA%\Programs\remote-shell-mcp`, adds them to the user PATH, runs `setup`. One-liner: `irm https://raw.githubusercontent.com/jaenster/remote-shell-mcp/main/install.ps1 | iex`.
- **`POST /rpc` endpoint on the daemon** — single JSON-RPC request → single JSON-RPC response, no SSE bookkeeping. Same bearer auth as `/sse`/`/message`. Useful when an MCP host is wedged and you need to talk to the daemon directly.
- **CLI mode on the launcher binary.** Run `remote-shell-mcp <tool> [key=value ...]` to invoke any MCP tool from the shell. Args use httpie typing: `key=string`, `key:=raw-json`, `key@=file-as-string`, `key:@=file-as-json`. `tools` lists them, `<tool> --help` prints the input schema. Output is the daemon's default format; `--json`/`--toon` transcode locally so piping to `jq` always works. Stdio-proxy mode (no args) is unchanged.
- **`install-smoke` CI workflow.** Builds the binaries on ubuntu / macos / windows, runs `setup --yes`, then asserts `claude mcp list` actually sees `remote-shell` — and hits the new `/rpc` surface to close the loop.
- **More MCP clients in `setup`:** Cursor (`~/.cursor/mcp.json`), Windsurf (`~/.codeium/windsurf/mcp_config.json`), Zed (`settings.json` under `context_servers`), Continue.dev (`~/.continue/config.json`; if only the YAML variant exists we skip with an error pointing at it). One-command install now wires us into seven clients.
- **First batch of unit tests** for the new surfaces: daemon lock (exclusive, re-acquire after release, idempotent Release), `/rpc` (405 on GET, tools/list, tools/call, JSON-RPC parse errors, notification → 204, oversized body refused), and the CLI argument parser (all four httpie operators, identifier rules, JSON↔TOON transcoder).
- **Cleaner `status` output.** Dropped redundant count fields (`ssh_sessions`, `docker_hosts`, the integer `forwards`) — TOON's tabular header `[N]{…}` already surfaces the count. Renamed `forwards_list` → `forwards` for naming consistency with `sessions` / `hosts`. Fixed `ListForwards` returning `nil` (→ JSON `null`) when no forwards are active; it now returns `[]` like the schema promises. 9 fields → 6, no lies-as-nulls.

## v0.1.5 — 2026-05-13

- **TOON list output is now compact-tabular.** `ssh_list`, `status.sessions`, `status.hosts`, `docker_list_hosts`, `docker_containers`, and `docker_image_list` project into primitive-only "row" types so TOON renders them as `[N]{fields}: row,row,...` instead of expanded per-element form. ~5× smaller output for `docker_containers` against a busy host. Detailed nested info still available via `ssh_info` and `docker_container_inspect`.

## v0.1.4 — 2026-05-13

- **`ForwardAgent` actually does something now.** Previously a JSON-tagged dead field. Sessions opened with `forward_agent: true` register the local agent (incl. 1Password / gpg-agent) and call `agent.RequestAgentForwarding` on each new channel, so the remote `SSH_AUTH_SOCK` points back at your laptop.
- **`ssh_exec` / `docker_exec` enforce a 30-second timeout by default**, configurable via `timeout_ms` up to 1h. Long-running work belongs in `ssh_shell_open` / `docker_shell_open`, which still have no timeout.
- **Launcher SSE watchdog**: if no SSE event arrives for 45s the launcher force-closes the body so the outer reconnect loop fires. Catches the case where TCP doesn't propagate EOF after a daemon restart.
- **Tighter reconnect backoff** (200ms first retry, was 500ms). Most daemon restarts complete inside the first backoff window.
- **Single stdin reader for the launcher's whole lifetime**. Reconnects no longer spawn dueling `os.Stdin` readers fighting over the parent client's bytes.

## v0.1.3 — 2026-05-13

- **Smart `ssh_connect`**: missing fields are filled from `~/.ssh/config`, the same way `ssh` does it. Pass `{"addresses": ["myhost"]}` and the daemon resolves `Hostname`, `Port`, `User`, `IdentityFile`, `IdentityAgent`, `ProxyJump`. Explicit fields always win; `disable_ssh_config: true` opts out.
- **`AuthSpec.AgentSocket`** override — explicit path supersedes `$SSH_AUTH_SOCK`. Required when the daemon doesn't inherit user env (e.g. 1Password's `IdentityAgent` socket).
- **`HostKeyAlgorithmsFor` derives the algorithm list from `known_hosts`** so the server can't pick a key type for which we have no entry and trip a spurious `knownhosts: key mismatch`.

## v0.1.2 — 2026-05-13

- TOON is now the default output format. `-format json` reverts.
- Round-tripping through `json.Marshal` so `toon-go` (which reads `toon:` tags) honors our `json:` tags transparently.

## v0.1.1 — 2026-05-12

- Added `install.sh` with checksum verification.
- Added `remote-shell-mcp setup` subcommand: detects Claude Code / Claude Desktop / Codex CLI configs and offers to register the daemon. Idempotent, backs up to `.bak`, supports `--dry-run`/`--yes`/`--client`/`--name`.
- GoReleaser-driven release workflow producing darwin/linux × amd64/arm64 archives with `checksums.txt`.

## v0.1.0 — 2026-05-12

Initial release. SSH (password / key / agent / multi-address / jump hosts / keepalive + auto-reconnect / persistent), SFTP, three forward modes, persistent PTY shells, Docker over unix/tcp/ssh, container lifecycle, image management, daemon + stdio launcher with bearer-token auth on the SSE endpoint.

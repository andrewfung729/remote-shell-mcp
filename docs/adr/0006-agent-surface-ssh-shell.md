# ADR-0006: Single ssh_shell tool — transparent sessions, enforced exposure, WebUI token

- **Status**: accepted
- **Date**: 2026-11-02
- **Issue**: #14 — partially supersedes ADR-0004 ("/api unauthenticated"),
  narrows the tool set under the ADR-0005 scope rule

## Context

The MCP surface exposed 22 tools, and agents only ever used `ssh_exec` —
the only tool that finished in one call. A persistent shell took
`ssh_shell_open` + `ssh_shell_write` (agent appends `\n`) +
`ssh_shell_read` (agent guesses when output ends and copes with ANSI),
plus `ssh_connect` first and a `session_id` on every call. `cd` and
`export` did not survive, so the remote host never felt like a local
shell.

The host-exposure list was also not a real boundary: `ssh_connect` never
checked it, and `/api/*` answered any request with no Origin — a local
agent could `curl` the daemon and expose hosts to itself.

## Decision

**One shell tool.** `ssh_shell` takes `host` plus exactly one of
`command` / `input` / `interrupt` / `poll` / `reset`, and an
optional `shell` name for parallel PTYs — omitted, the call opens `default`.
A throwaway name (for example `scratch`) is how a one-off command avoids
the default shell's cwd and env; there is no second command tool. A command
returns its output as text, then one bracketed status line (`exit N · cwd P`,
or `running — poll to continue`). `input` and `interrupt` wait up to 2s
(or `timeout_ms`) and return that same shape. A shell idle for 30 minutes
with no command in flight is closed, `default` included; the next call
reopens it and the status line says cwd/env were lost. Past
`timeout_ms` the command is not killed — `poll` follows it.
`cd`/`export` persist per host+shell. Output is stripped of ANSI,
echoed input and markers, and capped at 10,000 chars head+tail (full
output via `poll`). A second `command` while one runs fails with
`shell busy` — never silently queued. `reset` respawns a polluted
shell; there is no open/close/list/resize tool.

**Transparent sessions.** The session concept is hidden from agents: the
session id *is* the host alias, the first call dials automatically — and a
call that arrives while that dial is still running waits for it, so parallel
first calls on a cold host share one connection instead of seeing a
half-built session (keepalive + auto-reconnect always on, strict known_hosts, no
`insecure`), and reconnect reports on the next call's status line that
the shell restarted. The `persistent` flag, rehydration and `insecure` are deleted —
the state file holds only the exposure list and disabled tools.

**Remote shell layer.** Shells start as `bash -i -l` so rc files (nvm,
conda, pyenv) land on PATH; a bootstrap then overrides the prompt and
disables echo/bracketed-paste so user rc cannot corrupt completion
detection, which is a marker carrying exit code + cwd. If the first marker
does not arrive within 10 s — or the initial shell dies outright, which is
what `exec bash` does on a host without bash — the shell respawns on a minimal
`bash --norc` (or `sh`) and the status line says `degraded: sh fallback`. The fallback picks its
marker mechanism per shell: `PROMPT_COMMAND` for bash, a `PS1` command
substitution for a POSIX sh, which has no `PROMPT_COMMAND`. A command that
ends the shell (`exit 7`, `exec …`) reports that exit on the status line
instead of waiting out `timeout_ms`; the next call respawns it and says
the shell restarted.

**Enforced exposure.** Every tool rejects a non-exposed host with
`host not exposed`, without revealing whether the alias exists in ssh
config; the refusal does name the currently exposed aliases — the one
piece of config the agent is already allowed to see — so a mistyped
alias is self-correcting instead of sending the agent rummaging through
ssh config. `ssh_hosts` and `ssh_shell` are hidden from `tools/list`
until at least one host is exposed. Turning a host off disconnects its
session and shells immediately — no grace window.

**Text results only.** Every tool returns one text block. No tool declares
an output schema and no result sets `structuredContent`. Shell output and
file contents stay verbatim (real newlines); a short status line carries
exit code, cwd, truncation, restart, the degraded-shell flag, and — only
when it is not `default` — the shell name. Small results are a sentence or
one record per line. The status line is the only place an agent reads
status from.

**Connection failures hand off to the user.** A failed dial, on any tool,
is one fixed sentence: `cannot connect to "<alias>" (<category>)`, then an
instruction not to retry, not to run local ssh/scp/sftp/rsync, and not to
edit ssh config — ask the user, and the raw error is on the WebUI hosts
page. The category is one of: ssh config incomplete, host name does not
resolve, connection refused, timed out, port does not speak SSH, host key
not trusted, authentication rejected, or a generic connection failed.
The raw library text never reaches the agent. The failure is recorded per
alias with its time, cleared on the next successful connect, and never
cached as a reason to skip the next dial. "host not exposed" still runs
first and still names the exposed aliases.

**WebUI token auth.** Supersedes ADR-0004's "no auth token" for `/api`:
the daemon mints one random token per start, prints it once on stdout, and
the WebUI exchanges it for a 30-day HttpOnly SameSite=Strict cookie —
after which the token is dead and sessions live in memory only. `/api/*`
without the cookie returns 401; `/mcp` stays unauthenticated (same
documented limit: a same-OS-user agent can still edit the state file or
restart the daemon — real isolation needs a separate OS user). The
Host/Origin guard is unchanged and stays.

**Removed from MCP registration** (no backward compatibility):
`ssh_connect`, `ssh_disconnect`, `ssh_sessions`, `ssh_reconnect`,
`ssh_exec`, `ssh_shell_open`, `ssh_shell_write`, `ssh_shell_read`,
`ssh_shell_resize`, `ssh_shell_close`, `ssh_shell_list`. The WebUI
keeps calling the SSH manager directly. Remaining surface:
`ssh_hosts`, `ssh_shell`, and the `ssh_file_*`/`ssh_upload`/
`ssh_download` set (which now take `host` instead of `session_id`).

## Considered options

- **Keep `ssh_exec` alongside `ssh_shell`** — rejected: two ways to run
  a command splits the tool list and re-invites the stateless habit the
  issue set out to break; `ssh_shell` already returns in one call.
- **Queue a second command on a busy shell** — rejected: the agent cannot
  tell the outputs apart; a loud `shell busy` plus named parallel
  shells covers the real need.
- **Cookie per MCP client on /api too** — rejected: the WebUI's token is
  for the human at the browser; agents are supposed to be blocked from
  `/api`, not authenticated to it.
- **Keep the exposure list advisory** — rejected: an advisory boundary an
  agent can curl past is worse than none; enforcement is the point of the
  change.
- **Return `structuredContent` (alone or beside the text block), or pick
  the channel from clientInfo** — rejected: clients disagree about which
  channel they read, a client that forwards both doubles the tokens, and a
  client list goes stale. Text only is the backward-compatible shape and
  makes grok-build's unknown `structuredContent` handling irrelevant.

## Consequences

- Any agent prompt or skill hardcoding `session_id` or a removed tool
  name breaks — deliberate breaking change, no compat shim.
- Shell ownership derives from MCP clientInfo; the `client` parameter is
  gone.
- The state file can never hold a session spec or credential again.
- The exposure list is now a real boundary *for agents*, documented with
  its same-OS-user limit so it is not over-trusted.
- `bash -i -l` startup means a slow or hostile rc gets one 10 s window,
  then the shell degrades — bounded damage, visible on the status line.

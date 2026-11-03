package mcptools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

// SSHShellTool is the single remote-shell tool: one call runs a command on a
// per-host PTY shell that keeps cd/env between calls, or drives a running
// command (poll / input / interrupt), or resets the shell. It is hidden
// from tools/list until at least one host is exposed — same switch as
// ssh_hosts.
const SSHShellTool = "ssh_shell"

func RegisterSSH(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool(SSHShellTool,
		mcp.WithDescription("Run commands on an exposed SSH host as if it were a local shell. Pass host + command: the call returns the command's output as text, then one status line, when the command finishes or timeout_ms expires. cd and export persist between calls on the same host. Exactly one of command/input/interrupt/poll/reset per call: command runs text; a long command keeps running past timeout — poll collects more output; input feeds an interactive program and then waits; interrupt sends Ctrl-C and then waits; reset replaces a polluted shell. Omit shell to use default. Hosts must be exposed in the WebUI first; ssh_hosts lists the exposed aliases."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Host alias the user exposed in the WebUI (see ssh_hosts).")),
		mcp.WithString("command", mcp.Description("Shell command to run. Returns its output, then a status line with the exit code and cwd, or 'running — poll to continue' on timeout.")),
		mcp.WithString("input", mcp.Description("Bytes to send to the running command's stdin (answers prompts, drives vim/less). Include a newline to submit a line. The call then waits (default 2s, or timeout_ms) and returns the output and status, the same text shape as command and poll.")),
		mcp.WithBoolean("interrupt", mcp.Description("Send Ctrl-C to the running command, then wait (default 2s, or timeout_ms) and return the output and status, including the exit code if the program ended.")),
		mcp.WithBoolean("poll", mcp.Description("Collect output from a running (or just-finished) command. Returns its output and a status line (exit code and cwd once done).")),
		mcp.WithBoolean("reset", mcp.Description("Kill and respawn the named shell — recovers from a polluted environment (cd/env are lost).")),
		mcp.WithString("shell", mcp.Description("Shell name on this host. Omit it to use default. Use a throwaway name such as scratch for a one-off command that must not touch the default shell's cwd or env; separate names run in parallel. A shell idle for 30 minutes is closed, and the next call reopens it — the status line says the shell restarted and cwd/env were lost.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Max wait in milliseconds. command and poll default to 30000; input and interrupt default to 2000. Clamped to 3600000. On expiry a command keeps running — poll (or another input) continues it.")),
	), handleSSHShell(st))
}

// shellArgs is the raw ssh_shell argument shape. command and input are
// *string so "present but empty" still counts as selecting the action.
type shellArgs struct {
	Host      string  `json:"host"`
	Command   *string `json:"command,omitempty"`
	Input     *string `json:"input,omitempty"`
	Interrupt *bool   `json:"interrupt,omitempty"`
	Poll      *bool   `json:"poll,omitempty"`
	Reset     *bool   `json:"reset,omitempty"`
	Shell     string  `json:"shell,omitempty"`
	TimeoutMs int     `json:"timeout_ms,omitempty"`
}

const (
	shellDefaultTimeout = 30_000
	shellReportTimeout  = 2_000     // input and interrupt wait this long unless timeout_ms is set
	shellMaxTimeout     = 3_600_000 // 1h
)

// action is which of the five ssh_shell verbs the caller picked.
type shellAction int

const (
	actNone shellAction = iota
	actCommand
	actInput
	actInterrupt
	actPoll
	actReset
)

// validateShellArgs enforces exactly-one-action. It is deliberately a pure
// function of the args — unit-tested without a live session.
func validateShellArgs(a shellArgs) (shellAction, error) {
	if a.Host == "" {
		return actNone, errors.New("host is required")
	}
	n := 0
	var act shellAction
	if a.Command != nil {
		n++
		act = actCommand
	}
	if a.Input != nil {
		n++
		act = actInput
	}
	if a.Interrupt != nil && *a.Interrupt {
		n++
		act = actInterrupt
	}
	if a.Poll != nil && *a.Poll {
		n++
		act = actPoll
	}
	if a.Reset != nil && *a.Reset {
		n++
		act = actReset
	}
	if n == 0 {
		return actNone, errors.New("ssh_shell needs exactly one action: command, input, interrupt, poll, or reset")
	}
	if n > 1 {
		return actNone, errors.New("ssh_shell takes exactly one of command, input, interrupt, poll, reset — not more")
	}
	return act, nil
}

// targetHost reads the call's host alias. Every tool starts here.
func targetHost(req mcp.CallToolRequest) (string, error) {
	host := req.GetString("host", "")
	if host == "" {
		return "", errors.New("host is required")
	}
	return host, nil
}

// checkExposed is the exposure gate shared by ssh_shell and every file tool.
// It runs before any argument check so an unexposed host always gets the same
// refusal. The message names the exposed aliases — they are the one piece of
// config the agent is already allowed to see (ssh_hosts returns the same list).
func checkExposed(st *State, host string) error {
	if !st.IsExposed(host) {
		return errNotExposed(st, host)
	}
	return nil
}

// dial is the transparent-connect path: an exposed host's session connects on
// first use. It is the last step, after the exposure check and the argument
// checks, so a typo never opens a connection. A call that lands while another
// is still dialing the same host waits for that dial (bounded by ctx) instead
// of failing on a half-built session. Every failure comes back in one fixed
// shape — the raw error is logged and kept for the WebUI, never sent to the agent.
func dial(ctx context.Context, st *State, host string) (*sshx.Session, error) {
	sess, err := st.SSH.Connect(ctx, host)
	if err != nil {
		if st.Log != nil {
			st.Log.Warn("ssh connect failed", "host", host, "err", err)
		}
		return nil, agentConnectError(host, err)
	}
	return sess, nil
}

// agentConnectError is the only connection-failure text an agent sees.
func agentConnectError(host string, err error) error {
	return fmt.Errorf("cannot connect to %q (%s). Do not retry, run local ssh/scp/sftp/rsync, or edit ssh config — ask the user to check this host; details are on the WebUI hosts page.", host, sshx.ConnectCategory(err))
}

// shellTimeout clamps timeout_ms to [def, 1h].
func shellTimeout(ms, def int) time.Duration {
	if ms <= 0 {
		ms = def
	}
	if ms > shellMaxTimeout {
		ms = shellMaxTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

func finish(res *sshx.RunResult, sh *sshx.Shell, name string, restarted bool) (*mcp.CallToolResult, error) {
	return resultText(formatShell(res.Output, res.Status, res.CWD, name, res.ExitCode, res.Truncated, restarted, sh.Degraded))
}

// errNotExposed formats the exposure refusal with the list the caller can
// actually use — "host not exposed" alone sent agents rummaging through ssh
// config when they just typo'd the alias.
func errNotExposed(st *State, host string) error {
	exposed := st.ExposedHosts()
	if len(exposed) == 0 {
		return fmt.Errorf("host not exposed: %q — no hosts are exposed; ask the user to expose one in the WebUI", host)
	}
	return fmt.Errorf("host not exposed: %q — exposed hosts: %s", host, strings.Join(exposed, ", "))
}

func handleSSHShell(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		// Exposure first, then the action arguments, then the dial — the same
		// order every other tool follows.
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		var args shellArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		act, err := validateShellArgs(args)
		if err != nil {
			return resultErr(err)
		}
		sess, err := dial(ctx, st, host)
		if err != nil {
			return resultErr(err)
		}
		name := args.Shell
		if name == "" {
			name = "default"
		}
		owner := caller(ctx)
		timeout := shellTimeout(args.TimeoutMs, shellDefaultTimeout)

		// reset drops the shell first; everything else lazily (re)opens it.
		if act == actReset {
			sess.DropShell(name, owner)
		}
		sh, restarted, err := sess.OpenOrRespawn(name, owner)
		if err != nil {
			return resultErr(err)
		}
		// Every call is activity, including one that then blocks on a long
		// command — touch again when it returns so idle time starts then.
		sh.Touch()
		defer sh.Touch()

		switch act {
		case actCommand:
			res, err := sh.Run(*args.Command, timeout)
			if err != nil {
				return resultErr(err)
			}
			return finish(res, sh, name, restarted)
		case actPoll:
			res, err := sh.Poll(timeout)
			if err != nil {
				return resultErr(err)
			}
			return finish(res, sh, name, restarted)
		case actInput:
			if err := sh.Input(*args.Input); err != nil {
				return resultErr(err)
			}
			res, err := sh.Poll(shellTimeout(args.TimeoutMs, shellReportTimeout))
			if err != nil {
				return resultErr(err)
			}
			return finish(res, sh, name, restarted)
		case actInterrupt:
			if err := sh.Interrupt(); err != nil {
				return resultErr(err)
			}
			res, err := sh.Poll(shellTimeout(args.TimeoutMs, shellReportTimeout))
			if err != nil {
				return resultErr(err)
			}
			return finish(res, sh, name, restarted)
		case actReset:
			return resultText(formatShell("", "done", "", name, nil, false, restarted, sh.Degraded))
		}
		return resultErr(fmt.Errorf("unknown action"))
	}
}

package mcptools

import (
	"context"
	"errors"
	"fmt"
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

// ErrHostNotExposed is the refusal every tool gives for a host the user has
// not exposed in the WebUI. The message never reveals whether the alias
// exists in ssh config.
var ErrHostNotExposed = errors.New("host not exposed")

func RegisterSSH(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool(SSHShellTool,
		mcp.WithDescription("Run commands on an exposed SSH host as if it were a local shell. Pass host + command: the call returns when the command finishes or timeout_ms expires, with clean output, exit code and cwd. cd and export persist between calls on the same host. Exactly one of command/input/interrupt/poll/reset per call: command runs text; a long command keeps running past timeout (status running) — poll collects more output, input feeds an interactive program or prompt (append \n), interrupt sends Ctrl-C, reset replaces a polluted shell. Use shell to name a second parallel shell on the same host (default: 'default'). Hosts must be exposed in the WebUI first; ssh_hosts lists the exposed aliases."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Host alias the user exposed in the WebUI (see ssh_hosts).")),
		mcp.WithString("command", mcp.Description("Shell command to run. Returns output + exit_code + cwd when done, or status running on timeout.")),
		mcp.WithString("input", mcp.Description("Bytes to send to the running command's stdin (answers prompts, drives vim/less). Include \n to submit a line.")),
		mcp.WithBoolean("interrupt", mcp.Description("Send Ctrl-C to the running command.")),
		mcp.WithBoolean("poll", mcp.Description("Collect output from a running (or just-finished) command. Returns its output, exit code and cwd once done.")),
		mcp.WithBoolean("reset", mcp.Description("Kill and respawn the named shell — recovers from a polluted environment (cd/env are lost).")),
		mcp.WithString("shell", mcp.Description("Shell name on this host (default 'default'). Separate names run in parallel shells.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Max wait for command/poll to finish (default 30000, max 3600000). On expiry the command keeps running.")),
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
// refusal — the message never reveals whether the alias exists in ssh config.
func checkExposed(st *State, host string) error {
	if !st.IsExposed(host) {
		return ErrHostNotExposed
	}
	return nil
}

// dial is the transparent-connect path: an exposed host's session connects on
// first use. It is the last step, after the exposure check and the argument
// checks, so a typo never opens a connection. A call that lands while another
// is still dialing the same host waits for that dial (bounded by ctx) instead
// of failing on a half-built session.
func dial(ctx context.Context, st *State, host string) (*sshx.Session, error) {
	return st.SSH.Connect(ctx, host)
}

// shellTimeout clamps timeout_ms to [default, 1h].
func shellTimeout(ms int) time.Duration {
	if ms <= 0 {
		ms = shellDefaultTimeout
	}
	if ms > shellMaxTimeout {
		ms = shellMaxTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// shellResult is the JSON shape ssh_shell returns.
type shellResult struct {
	Output         string `json:"output"`
	Status         string `json:"status"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	Truncated      bool   `json:"truncated,omitempty"`
	ShellRestarted bool   `json:"shell_restarted,omitempty"`
	ShellDegraded  bool   `json:"shell_degraded,omitempty"`
}

func finish(st *State, res *sshx.RunResult, sh *sshx.Shell, restarted bool) (*mcp.CallToolResult, error) {
	return st.resultJSON(shellResult{
		Output:         res.Output,
		Status:         res.Status,
		ExitCode:       res.ExitCode,
		CWD:            res.CWD,
		Truncated:      res.Truncated,
		ShellRestarted: restarted,
		ShellDegraded:  sh.Degraded,
	})
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
		timeout := shellTimeout(args.TimeoutMs)

		// reset drops the shell first; everything else lazily (re)opens it.
		if act == actReset {
			sess.DropShell(name, owner)
		}
		sh, restarted, err := sess.OpenOrRespawn(name, owner)
		if err != nil {
			return resultErr(err)
		}

		switch act {
		case actCommand:
			res, err := sh.Run(*args.Command, timeout)
			if err != nil {
				return resultErr(err)
			}
			return finish(st, res, sh, restarted)
		case actPoll:
			res, err := sh.Poll(timeout)
			if err != nil {
				return resultErr(err)
			}
			return finish(st, res, sh, restarted)
		case actInput:
			if err := sh.Input(*args.Input); err != nil {
				return resultErr(err)
			}
			return st.resultJSON(shellResult{Status: "running", ShellRestarted: restarted, ShellDegraded: sh.Degraded})
		case actInterrupt:
			if err := sh.Interrupt(); err != nil {
				return resultErr(err)
			}
			return st.resultJSON(shellResult{Status: "running", ShellRestarted: restarted, ShellDegraded: sh.Degraded})
		case actReset:
			return st.resultJSON(shellResult{Status: "done", ShellRestarted: restarted, ShellDegraded: sh.Degraded})
		}
		return resultErr(fmt.Errorf("unknown action"))
	}
}

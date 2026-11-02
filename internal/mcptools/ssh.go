package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func RegisterSSH(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_connect",
		mcp.WithDescription("Open a named, long-lived SSH session. The first entry in addresses must be a concrete Host alias in the daemon user's ~/.ssh/config — user, hostname, port, ProxyJump, and auth (IdentityFile/IdentityAgent) all resolve from that entry; callers never supply credentials, and a missing/wildcard-only entry fails. Returns the session id and connection state."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Identifier for this session. Reuse it in subsequent ssh_* tool calls.")),
		mcp.WithObject("spec", mcp.Required(),
			mcp.Description("Connection spec. Auth is not part of it — credentials resolve daemon-side from ssh_config."),
			mcp.Properties(map[string]any{
				"user":      map[string]any{"type": "string", "description": "Remote login user. Optional when the ssh_config entry sets User."},
				"addresses": map[string]any{"type": "array", "description": "Hosts to try in order, each \"host\" or \"host:port\". The first must match a concrete Host in ~/.ssh/config; subsequent entries are explicit fallbacks tried in order. Required.", "items": map[string]any{"type": "string"}},
				"jump_hosts": map[string]any{
					"type":        "array",
					"description": "Explicit ProxyJump chain, applied in order. Each entry: {user, addresses, known_hosts_path?, insecure?} — auth resolves from ssh_config per hop. Omitted → the entry's ProxyJump is used.",
					"items":       map[string]any{"type": "object"},
				},
				"insecure":         map[string]any{"type": "boolean", "description": "Skip ~/.ssh/known_hosts host-key verification. Use only for testing."},
				"known_hosts_path": map[string]any{"type": "string", "description": "Override default ~/.ssh/known_hosts location."},
				"timeout":          map[string]any{"type": "string", "description": "Per-dial timeout, e.g. \"15s\". Default 15s."},
				"keepalive":        map[string]any{"type": "string", "description": "SSH keepalive interval, e.g. \"30s\". Enables drop detection. Required for auto_reconnect to fire."},
				"auto_reconnect":   map[string]any{"type": "boolean", "description": "Re-dial automatically when keepalive notices a drop, with exponential backoff."},
				"persistent":       map[string]any{"type": "boolean", "description": "Survive daemon restart. Spec is rehydrated on startup and re-resolved against ~/.ssh/config — the entry must still resolve on boot."},
			}),
		),
	), handleSSHConnect(st))

	srv.AddTool(mcp.NewTool("ssh_disconnect",
		mcp.WithDescription("Close an SSH session and free its resources."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Session id to close.")),
	), handleSSHDisconnect(st))

	srv.AddTool(mcp.NewTool("ssh_sessions",
		mcp.WithDescription("List currently open SSH sessions (live connections this daemon is holding). Each entry has the session id, connection state, addresses, and open shells. This does NOT list hosts you can connect to — for those use ssh_hosts. To open a session call ssh_connect."),
	), handleSSHList(st))

	srv.AddTool(mcp.NewTool("ssh_reconnect",
		mcp.WithDescription("Force re-dial of an SSH session."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Session id to reconnect.")),
	), handleSSHReconnect(st))

	srv.AddTool(mcp.NewTool("ssh_exec",
		mcp.WithDescription("Run a one-shot command on an SSH session. Each call uses a fresh channel and is bounded by a 30-second timeout — for anything long-running (builds, tails, deploys) or stateful (sequences relying on cd/env, interactive programs like vim/top/ssh) open a persistent shell with ssh_shell_open instead and run commands in it via ssh_shell_write + ssh_shell_read. timeout_ms raises the per-call cap (max 1h)."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session id.")),
		mcp.WithString("command", mcp.Required(), mcp.Description("Shell command to run.")),
		mcp.WithObject("env", mcp.Description("Environment variables {KEY: value}. Many servers ignore Setenv unless AcceptEnv is configured.")),
		mcp.WithString("stdin", mcp.Description("Optional data to write to the command's stdin.")),
		mcp.WithNumber("timeout_ms", mcp.Description("Override the 30s default. Capped at 1h. On expiry the remote process gets SIGKILL and the channel is force-closed.")),
	), handleSSHExec(st))
}

type sshConnectArgs struct {
	ID   string           `json:"id"`
	Spec sshx.ConnectSpec `json:"spec"`
}

func handleSSHConnect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args sshConnectArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		sess, err := st.SSH.Connect(args.ID, args.Spec)
		if err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return st.resultJSON(sess.Info())
	}
}

func handleSSHDisconnect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return resultErr(err)
		}
		if err := st.SSH.Disconnect(id); err != nil {
			return resultErr(err)
		}
		_ = st.Persist()
		return mcp.NewToolResultText("disconnected " + id), nil
	}
}

func handleSSHList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		full := st.SSH.List()
		rows := make([]sshx.SessionRow, 0, len(full))
		for _, s := range full {
			rows = append(rows, s.Row())
		}
		return st.resultJSON(rows)
	}
}

func handleSSHReconnect(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := req.RequireString("id")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Reconnect(id)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(s.Info())
	}
}

type sshExecArgs struct {
	SessionID string            `json:"session_id"`
	Command   string            `json:"command"`
	Env       map[string]string `json:"env,omitempty"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
}

func handleSSHExec(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args sshExecArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(args.SessionID)
		if err != nil {
			return resultErr(err)
		}
		cctx, cancel := execContext(ctx, args.TimeoutMs, 30_000)
		defer cancel()
		res, err := s.Exec(cctx, args.Command, sshx.ExecOptions{Env: args.Env, Stdin: args.Stdin})
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(res)
	}
}

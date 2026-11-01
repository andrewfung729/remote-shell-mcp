package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func RegisterSSH(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_connect",
		mcp.WithDescription("Open a named, long-lived SSH session. Supports multiple addresses (tried in order — e.g. [lan-ip, wan-host]), ProxyJump-style jump hosts, and key/agent/password auth. Returns the session id and connection state."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Identifier for this session. Reuse it in subsequent ssh_* tool calls.")),
		mcp.WithObject("spec", mcp.Required(),
			mcp.Description("Connection spec."),
			mcp.Properties(map[string]any{
				"user":      map[string]any{"type": "string", "description": "Remote login user. Required."},
				"addresses": map[string]any{"type": "array", "description": "Hosts to try in order, each \"host\" or \"host:port\". Required.", "items": map[string]any{"type": "string"}},
				"auth": map[string]any{
					"type":        "object",
					"description": "Authentication. Provide at least one of: key_path, use_agent, password.",
					"properties": map[string]any{
						"key_path":       map[string]any{"type": "string", "description": "Path to private key on the daemon host."},
						"key_passphrase": map[string]any{"type": "string", "description": "Passphrase for the private key, if any."},
						"use_agent":      map[string]any{"type": "boolean", "description": "Use ssh-agent for auth. Reads SSH_AUTH_SOCK unless agent_socket is set."},
						"agent_socket":   map[string]any{"type": "string", "description": "Explicit path to an ssh-agent unix socket. Required when SSH_AUTH_SOCK isn't propagated to the daemon (e.g. 1Password agent at ~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock). Supports leading ~/."},
						"password":       map[string]any{"type": "string", "description": "Password auth (not persisted across daemon restart)."},
					},
				},
				"jump_hosts": map[string]any{
					"type":        "array",
					"description": "ProxyJump chain, applied in order. Each entry: {user, addresses, auth?, known_hosts_path?, insecure?}.",
					"items":       map[string]any{"type": "object"},
				},
				"insecure":         map[string]any{"type": "boolean", "description": "Skip ~/.ssh/known_hosts host-key verification. Use only for testing."},
				"known_hosts_path": map[string]any{"type": "string", "description": "Override default ~/.ssh/known_hosts location."},
				"timeout":          map[string]any{"type": "string", "description": "Per-dial timeout, e.g. \"15s\". Default 15s."},
				"keepalive":        map[string]any{"type": "string", "description": "SSH keepalive interval, e.g. \"30s\". Enables drop detection. Required for auto_reconnect to fire."},
				"auto_reconnect":   map[string]any{"type": "boolean", "description": "Re-dial automatically when keepalive notices a drop, with exponential backoff. Forwards are re-bound after reconnect."},
				"persistent":       map[string]any{"type": "boolean", "description": "Survive daemon restart. Spec is rehydrated on startup; secrets (password, key_passphrase) are scrubbed and must be re-resolvable from disk/agent."},
				"forward_agent":    map[string]any{"type": "boolean", "description": "Enable SSH agent forwarding to the remote session."},
			}),
		),
	), handleSSHConnect(st))

	srv.AddTool(mcp.NewTool("ssh_disconnect",
		mcp.WithDescription("Close an SSH session and free its resources."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Session id to close.")),
	), handleSSHDisconnect(st))

	srv.AddTool(mcp.NewTool("ssh_list",
		mcp.WithDescription("List all SSH sessions with state, addresses, and attached forwards/shells."),
	), handleSSHList(st))

	srv.AddTool(mcp.NewTool("ssh_reconnect",
		mcp.WithDescription("Force re-dial of an SSH session."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Session id to reconnect.")),
	), handleSSHReconnect(st))

	srv.AddTool(mcp.NewTool("ssh_exec",
		mcp.WithDescription("Run a one-shot command on an SSH session. Each call uses a fresh channel and is bounded by a 30-second timeout — for anything long-running (builds, tails, deploys) open a persistent shell with ssh_shell_open instead, which has no timeout. timeout_ms raises the per-call cap (max 1h)."),
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

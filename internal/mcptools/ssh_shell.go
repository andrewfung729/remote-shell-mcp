package mcptools

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func RegisterSSHShell(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_shell_open",
		mcp.WithDescription("Open a long-lived PTY shell on an SSH session. Run commands in it via ssh_shell_write (with a trailing \\n) and collect output with ssh_shell_read. State (cd, env, vim) survives between calls until ssh_shell_close. Does NOT survive a daemon restart — the remote shell process dies with the connection; only the session is re-dialed."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Parent SSH session id.")),
		mcp.WithString("shell_id", mcp.Required(), mcp.Description("Identifier for this shell.")),
		mcp.WithString("term", mcp.Description("TERM value (default: xterm-256color).")),
		mcp.WithNumber("rows", mcp.Description("PTY rows (default 24).")),
		mcp.WithNumber("cols", mcp.Description("PTY cols (default 80).")),
		mcp.WithString("command", mcp.Description("Command to run instead of an interactive login shell.")),
		mcp.WithObject("env", mcp.Description("Environment variables.")),
		mcp.WithString("client", mcp.Description("Optional stable name for the owning client (e.g. \"pi\", \"claude-code\"). Scopes the shell so only callers passing the same client can reach it; defaults to the MCP clientInfo.name or transport session id.")),
	), handleShellOpen(st))

	srv.AddTool(mcp.NewTool("ssh_shell_write",
		mcp.WithDescription("Send input to a persistent shell — this is how you run commands in it. Include a trailing \\n to execute a line, then call ssh_shell_read for the output."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithString("data", mcp.Required(), mcp.Description("Bytes to send to the shell stdin.")),
		mcp.WithString("client", mcp.Description("Client name owning the shell (same value passed to ssh_shell_open). Defaults to caller identity.")),
	), handleShellWrite(st))

	srv.AddTool(mcp.NewTool("ssh_shell_read",
		mcp.WithDescription("Read output produced by commands sent via ssh_shell_write. Returns immediately if data is buffered; otherwise waits up to timeout_ms for new output."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("timeout_ms", mcp.Description("How long to wait for new output if buffer is empty. Default 1000.")),
		mcp.WithString("client", mcp.Description("Client name owning the shell. Defaults to caller identity.")),
	), handleShellRead(st))

	srv.AddTool(mcp.NewTool("ssh_shell_resize",
		mcp.WithDescription("Resize the shell's PTY."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithNumber("rows", mcp.Required()),
		mcp.WithNumber("cols", mcp.Required()),
		mcp.WithString("client", mcp.Description("Client name owning the shell. Defaults to caller identity.")),
	), handleShellResize(st))

	srv.AddTool(mcp.NewTool("ssh_shell_close",
		mcp.WithDescription("Close a persistent shell."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("shell_id", mcp.Required()),
		mcp.WithString("client", mcp.Description("Client name owning the shell. Defaults to caller identity.")),
	), handleShellClose(st))

	srv.AddTool(mcp.NewTool("ssh_shell_list",
		mcp.WithDescription("List shells on a session, or across all sessions if session_id is omitted. Returns only shells owned by the calling client."),
		mcp.WithString("session_id", mcp.Description("Optional session id.")),
		mcp.WithString("client", mcp.Description("Client name to list for. Defaults to caller identity.")),
	), handleShellList(st))
}

type shellOpenArgs struct {
	SessionID string            `json:"session_id"`
	ShellID   string            `json:"shell_id"`
	Term      string            `json:"term,omitempty"`
	Rows      int               `json:"rows,omitempty"`
	Cols      int               `json:"cols,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Client    string            `json:"client,omitempty"`
}

func handleShellOpen(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args shellOpenArgs
		if err := bind(req, &args); err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(args.SessionID)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.OpenShell(args.ShellID, sshx.ShellOptions{
			Term: args.Term, Rows: args.Rows, Cols: args.Cols,
			Command: args.Command, Env: args.Env,
			Owner: owner(ctx, req),
		})
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(sh.Info())
	}
}

func handleShellWrite(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		data, err := req.RequireString("data")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid, owner(ctx, req))
		if err != nil {
			return resultErr(err)
		}
		n, err := sh.Write([]byte(data))
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes_written": n})
	}
}

func handleShellRead(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		timeoutMs := clampReadTimeout(req.GetInt("timeout_ms", 1000))
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid, owner(ctx, req))
		if err != nil {
			return resultErr(err)
		}
		out, err := sh.Read(time.Duration(timeoutMs) * time.Millisecond)
		if err != nil {
			return st.resultJSON(map[string]any{"data": string(out), "eof": true})
		}
		return st.resultJSON(map[string]any{"data": string(out), "eof": false})
	}
}

func handleShellResize(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		rows, err := req.RequireInt("rows")
		if err != nil {
			return resultErr(err)
		}
		cols, err := req.RequireInt("cols")
		if err != nil {
			return resultErr(err)
		}
		if rows < 1 || rows > 1000 || cols < 1 || cols > 1000 {
			return resultErr(errors.New("rows/cols must be in [1, 1000]"))
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		sh, err := s.GetShell(shid, owner(ctx, req))
		if err != nil {
			return resultErr(err)
		}
		if err := sh.Resize(rows, cols); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("ok"), nil
	}
}

func handleShellClose(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		shid, err := req.RequireString("shell_id")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.CloseShell(shid, owner(ctx, req)); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("closed " + shid), nil
	}
}

func handleShellList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid := req.GetString("session_id", "")
		if sid == "" {
			var all []sshx.ShellInfo
			for _, s := range st.SSH.Sessions() {
				all = append(all, s.ListShells(owner(ctx, req))...)
			}
			return st.resultJSON(all)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(s.ListShells(owner(ctx, req)))
	}
}

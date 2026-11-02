package mcptools

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// This file is the file half of the agent surface. Four of the six operations
// an SFTP client offers are also one shell command each, so they are not
// tools: chmod, mkdir, rm and mv go through ssh_shell. What stays is what the
// shell cannot replace — list, stat, and the bounded text channel (read,
// write, exactly-once edit) — plus the bulk transfer pair that keeps bytes out
// of the model context.

// RegisterSFTP registers the file tools.
func RegisterSFTP(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_file_list",
		mcp.WithDescription("List a directory on the remote host via SFTP."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("path", mcp.Required()),
	), handleFileList(st))

	srv.AddTool(mcp.NewTool("ssh_file_stat",
		mcp.WithDescription("Stat a remote file."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("path", mcp.Required()),
	), handleFileStat(st))

	srv.AddTool(mcp.NewTool("ssh_file_read",
		mcp.WithDescription("Read a remote text file as a window of lines. Up to 256 KiB (262144 bytes) of file per call: a larger file, or one that is not valid UTF-8, is an error — use ssh_download for those. start_line defaults to 1; max_lines omitted or 0 reads to the end of the file. Lines keep their endings (CRLF stays CRLF) so the data can be written back verbatim."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("start_line", mcp.Description("First line of the window, 1-based (default 1).")),
		mcp.WithNumber("max_lines", mcp.Description("How many lines to return (default: to the end of the file).")),
	), handleFileRead(st))

	srv.AddTool(mcp.NewTool("ssh_file_write",
		mcp.WithDescription("Write text to a remote file, truncating it unless append is set. At most 256 KiB (262144 bytes) of data per call — for anything larger or binary use ssh_upload."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("data", mcp.Required(), mcp.Description("Text content to write (at most 262144 bytes).")),
		mcp.WithBoolean("append", mcp.Description("Append rather than truncate the existing file (default false).")),
	), handleFileWrite(st))

	srv.AddTool(mcp.NewTool("ssh_file_edit",
		mcp.WithDescription("Replace text in a remote file in place. old_text must occur exactly once, byte for byte (CRLF included) — otherwise the call fails and changes nothing. Read the file with ssh_file_read first: an edit is refused when this daemon has no record of the file's content or when the file changed since that read. Both the file and the result must fit the 256 KiB (262144 bytes) inline budget. Returns the line the replacement starts on and the new line count."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("old_text", mcp.Required(), mcp.Description("Exact text to replace; must occur exactly once.")),
		mcp.WithString("new_text", mcp.Required(), mcp.Description("Replacement text; may be empty to delete.")),
	), handleFileEdit(st))

	srv.AddTool(mcp.NewTool("ssh_upload",
		mcp.WithDescription("Stream a local file OR directory (where the daemon runs) to the remote host via SFTP. Directories are mirrored recursively; intermediate dirs are created; file permissions are preserved best-effort; symlinks are skipped. This is the right tool for any non-trivial data transfer — it streams over the SSH channel without serializing bytes into the MCP message."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("local_path", mcp.Required(), mcp.Description("Path on the daemon's filesystem; can be a file or a directory.")),
		mcp.WithString("remote_path", mcp.Required(), mcp.Description("Destination path on the remote host. When local_path is a directory, this becomes the remote root for the mirrored tree.")),
	), handleUpload(st))

	srv.AddTool(mcp.NewTool("ssh_download",
		mcp.WithDescription("Stream a remote file OR directory to the local filesystem (where the daemon runs) via SFTP. Directories are mirrored recursively; intermediate dirs are created; symlinks are skipped. Use this instead of ssh_file_read whenever the payload is binary, is not valid UTF-8, or is larger than 256 KiB."),
		mcp.WithString("host", mcp.Required(), mcp.Description("Exposed host alias (see ssh_hosts).")),
		mcp.WithString("remote_path", mcp.Required(), mcp.Description("Path on the remote host; can be a file or a directory.")),
		mcp.WithString("local_path", mcp.Required(), mcp.Description("Destination path on the daemon's filesystem. When remote_path is a directory, this becomes the local root for the mirrored tree.")),
	), handleDownload(st))
}

func handleFileList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		entries, err := s.FileList(path)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(entries)
	}
}

func handleFileStat(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		info, err := s.FileStat(path)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(info)
	}
}

func handleFileRead(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		startLine, maxLines, err := lineWindowArgs(req.GetArguments())
		if err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		// One bounded read of the whole file: over budget or not valid UTF-8
		// is an error, never a shortened body.
		buf, err := s.FileReadBounded(path, inlineBudget)
		if err != nil {
			return resultErr(err)
		}
		if err := checkInlineRead(buf); err != nil {
			return resultErr(err)
		}
		win, err := sliceLines(buf, startLine, maxLines)
		if err != nil {
			return resultErr(err)
		}
		// Record the whole file even when the window was partial: an edit's
		// own read compares against this hash.
		st.Guard.AfterRead(caller(ctx), host, path, buf)
		return st.resultJSON(win)
	}
}

func handleFileWrite(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		data, err := req.RequireString("data")
		if err != nil {
			return resultErr(err)
		}
		appendMode := req.GetBool("append", false)
		// The budget bounds this call's data, not the remote file afterwards:
		// appending a short string to a large file still works.
		if err := checkWriteBudget(len(data)); err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		werr := s.FileWrite(path, []byte(data), appendMode)
		st.Guard.AfterWrite(caller(ctx), host, path, []byte(data), appendMode, werr)
		if werr != nil {
			return resultErr(werr)
		}
		return st.resultJSON(map[string]any{"bytes_written": len(data)})
	}
}

// editResult is what a successful ssh_file_edit returns: where the change
// landed and how big the file is now — never the file itself.
type editResult struct {
	StartLine  int `json:"start_line"`
	TotalLines int `json:"total_lines"`
}

func handleFileEdit(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		oldText, err := req.RequireString("old_text")
		if err != nil {
			return resultErr(err)
		}
		newText, err := req.RequireString("new_text")
		if err != nil {
			return resultErr(err)
		}
		if err := checkEditTexts(oldText, newText); err != nil {
			return resultErr(err)
		}
		client := caller(ctx)
		// Read before edit: no record means this client has not seen the file
		// in this daemon run, and editing blind is exactly the failure mode
		// the guard exists to prevent. This fails before any dial.
		if _, ok := st.Guard.Lookup(client, host, path); !ok {
			return resultErr(errEditNeedsRead)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		src, err := s.FileReadBounded(path, inlineBudget)
		if err != nil {
			return resultErr(err)
		}
		if err := checkInlineRead(src); err != nil {
			return resultErr(err)
		}
		if err := st.Guard.Check(client, host, path, src); err != nil {
			return resultErr(err)
		}
		count, idx := countMatches(src, []byte(oldText))
		switch {
		case count == 0:
			return resultErr(errors.New("old_text not found in the file"))
		case count > 1:
			return resultErr(fmt.Errorf("old_text occurs %d times: add surrounding context so it matches exactly once", count))
		}
		result := applyEdit(src, idx, []byte(oldText), []byte(newText))
		if err := checkEditBudget(len(result)); err != nil {
			return resultErr(err)
		}
		// In place, through the same open-truncate-write path as a write: no
		// temp file, no rename. Owner, mode, hard links and a symlink survive;
		// a connection lost mid-write can leave a partial file.
		if werr := s.FileWrite(path, result, false); werr != nil {
			st.Guard.AfterEdit(client, host, path, nil, werr)
			return resultErr(fmt.Errorf("write failed, a partial file may remain: %w", werr))
		}
		st.Guard.AfterEdit(client, host, path, result, nil)
		return st.resultJSON(editResult{
			StartLine:  startLineAt(result, idx),
			TotalLines: lineCount(result),
		})
	}
}

func handleUpload(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		local, err := req.RequireString("local_path")
		if err != nil {
			return resultErr(err)
		}
		remote, err := req.RequireString("remote_path")
		if err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		n, err := s.Upload(local, remote)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes": n})
	}
}

func handleDownload(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		host, err := targetHost(req)
		if err != nil {
			return resultErr(err)
		}
		if err := checkExposed(st, host); err != nil {
			return resultErr(err)
		}
		remote, err := req.RequireString("remote_path")
		if err != nil {
			return resultErr(err)
		}
		local, err := req.RequireString("local_path")
		if err != nil {
			return resultErr(err)
		}
		s, err := dial(st, host)
		if err != nil {
			return resultErr(err)
		}
		n, err := s.Download(remote, local)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes": n})
	}
}

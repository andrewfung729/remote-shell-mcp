package mcptools

import (
	"fmt"
	"strings"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

// withStatus puts status on the line after body. An empty body is just the
// status line — an empty result has to stay unambiguous. A body that already
// ends in a newline does not grow a blank line.
func withStatus(body, status string) string {
	if body == "" {
		return status
	}
	if strings.HasSuffix(body, "\n") {
		return body + status
	}
	return body + "\n" + status
}

// formatShell is the ssh_shell text result: verbatim output, then one
// bracketed status line. Segments appear only when they apply, in this
// order, joined with " · ". The shell name is omitted for "default".
func formatShell(output, status, cwd, name string, exit *int, truncated, restarted, degraded bool) string {
	var segs []string
	if status == "done" {
		if exit != nil {
			segs = append(segs, fmt.Sprintf("exit %d", *exit))
		}
		if cwd != "" {
			segs = append(segs, "cwd "+cwd)
		}
	} else {
		segs = append(segs, "running — poll to continue")
	}
	if truncated {
		segs = append(segs, "truncated, poll for full output")
	}
	if restarted {
		segs = append(segs, "shell restarted: cwd/env lost")
	}
	if degraded {
		segs = append(segs, "degraded: sh fallback")
	}
	if name != "" && name != "default" {
		segs = append(segs, "shell "+name)
	}
	return withStatus(output, "["+strings.Join(segs, " · ")+"]")
}

// formatLineWindow is the ssh_file_read text result. An empty file is
// "[0 lines]"; a window that stopped early names the next start_line.
func formatLineWindow(w lineWindow) string {
	if w.TotalLines == 0 {
		return "[0 lines]"
	}
	end := w.StartLine + w.Lines - 1
	status := fmt.Sprintf("[lines %d–%d of %d]", w.StartLine, end, w.TotalLines)
	if w.Truncated {
		status = fmt.Sprintf("[lines %d–%d of %d · more: start_line=%d]", w.StartLine, end, w.TotalLines, end+1)
	}
	return withStatus(w.Data, status)
}

// formatFileList is one ls-style line per entry. Directories end in "/".
func formatFileList(entries []sshx.FileEntry) string {
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		if e.IsDir {
			name += "/"
		}
		if e.Target != "" {
			name += " -> " + e.Target
		}
		lines = append(lines, fmt.Sprintf("%s %d %s %s", e.Mode, e.Size, e.ModTime.Format(time.RFC3339), name))
	}
	return strings.Join(lines, "\n")
}

// formatFileStat is one "key: value" line per field the stat actually has.
// target is omitted when the path is not a symlink — same as the old JSON.
func formatFileStat(e sshx.FileEntry) string {
	lines := []string{
		"name: " + e.Name,
		fmt.Sprintf("size: %d", e.Size),
		"mode: " + e.Mode,
		"mod_time: " + e.ModTime.Format(time.RFC3339),
		fmt.Sprintf("is_dir: %t", e.IsDir),
	}
	if e.Target != "" {
		lines = append(lines, "target: "+e.Target)
	}
	return strings.Join(lines, "\n")
}

func formatWrite(path string, n int) string {
	return fmt.Sprintf("wrote %d bytes to %s", n, path)
}

// formatEdit names the lines the replacement occupies and the file's new size.
// A deletion (empty new text) names only the line it started on.
func formatEdit(start, total int, newText string) string {
	end := start
	if n := lineCount([]byte(newText)); n > 1 {
		end = start + n - 1
	}
	if start == end {
		return fmt.Sprintf("edited line %d (file now %d lines)", start, total)
	}
	return fmt.Sprintf("edited lines %d–%d (file now %d lines)", start, end, total)
}

func formatUpload(path string, n int64) string {
	return fmt.Sprintf("uploaded %d bytes to %s", n, path)
}

func formatDownload(path string, n int64) string {
	return fmt.Sprintf("downloaded %d bytes to %s", n, path)
}

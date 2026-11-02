package mcptools

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// This file is the text file channel: the shared inline budget, the
// size-then-UTF-8 gate, and line slicing. Everything here is a pure function
// of bytes and arguments — no session, no sshd.

// inlineBudget is the one cap on text that travels through the model context:
// ssh_file_read returns at most this many bytes of a file, ssh_file_write and
// ssh_file_edit accept at most this many. Anything larger belongs on disk
// (ssh_upload / ssh_download). The remote side reads one byte past it only to
// detect overflow.
const inlineBudget = 256 * 1024 // 262144 bytes

var (
	// errOverReadBudget is returned when the file does not fit the inline
	// read budget. A file that does not fit is an error, never a shortened
	// success.
	errOverReadBudget = fmt.Errorf("file is larger than the %d-byte inline read budget: use ssh_download", inlineBudget)
	// errNotUTF8 is returned when the file fits but cannot be carried as
	// JSON text.
	errNotUTF8 = errors.New("file is not valid UTF-8: use ssh_download")
	// errOverWriteBudget bounds one call's data argument, not the remote file
	// afterwards — appending a short string to a large file still works.
	errOverWriteBudget = fmt.Errorf("data is larger than the %d-byte inline write budget: use ssh_upload", inlineBudget)
	errOverEditBudget  = fmt.Errorf("edit would make the file larger than the %d-byte inline budget: use ssh_upload", inlineBudget)
	errStartLineBad    = errors.New("start_line must be >= 1")
	errMaxLinesBad     = errors.New("max_lines must be >= 0")
)

// lineWindow is the JSON body ssh_file_read returns: a window of lines and
// just enough context to ask for the next one.
type lineWindow struct {
	Data       string `json:"data"`
	StartLine  int    `json:"start_line"`
	Lines      int    `json:"lines"`
	TotalLines int    `json:"total_lines"`
	// Truncated is set only when max_lines stopped before the last line.
	Truncated bool `json:"truncated,omitempty"`
}

// checkInlineRead is the gate every in-context read passes: size first, then
// UTF-8. A file that fails both gets the size error, so the two failures have
// one order.
func checkInlineRead(buf []byte) error {
	if len(buf) > inlineBudget {
		return errOverReadBudget
	}
	if !utf8.Valid(buf) {
		return errNotUTF8
	}
	return nil
}

// checkBudget refuses n bytes with over when it does not fit the inline
// budget. Exactly inlineBudget bytes is allowed.
func checkBudget(n int, over error) error {
	if n > inlineBudget {
		return fmt.Errorf("%w (%d bytes)", over, n)
	}
	return nil
}

// checkWriteBudget bounds the data argument of one write call.
func checkWriteBudget(n int) error { return checkBudget(n, errOverWriteBudget) }

// checkEditBudget bounds the file an edit would produce, so an edit only ever
// yields files that a read can return.
func checkEditBudget(n int) error { return checkBudget(n, errOverEditBudget) }

// lineStarts returns the byte offset where each line of buf begins. Lines are
// split on \n only and keep their terminator: a trailing newline ends the last
// line rather than opening an empty one, CRLF is left alone, and a non-empty
// tail after the last \n is a line. An empty buffer has no lines.
func lineStarts(buf []byte) []int {
	if len(buf) == 0 {
		return nil
	}
	starts := []int{0}
	for i, b := range buf {
		if b == '\n' && i+1 < len(buf) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// sliceLines cuts a line window out of a buffer that already passed
// checkInlineRead. maxLines 0 means "to the end of the file". A start_line
// past the last line is an error — an empty window is never mistaken for an
// empty file.
func sliceLines(buf []byte, startLine, maxLines int) (lineWindow, error) {
	if startLine < 1 {
		return lineWindow{}, errStartLineBad
	}
	if maxLines < 0 {
		return lineWindow{}, errMaxLinesBad
	}
	starts := lineStarts(buf)
	total := len(starts)
	if total == 0 {
		// The empty file has exactly one addressable position: line 1, the
		// (empty) whole file.
		if startLine != 1 {
			return lineWindow{}, fmt.Errorf("start_line %d is past the last line (%d lines)", startLine, total)
		}
		return lineWindow{Data: "", StartLine: 1, Lines: 0, TotalLines: 0}, nil
	}
	if startLine > total {
		return lineWindow{}, fmt.Errorf("start_line %d is past the last line (%d lines)", startLine, total)
	}
	first := startLine - 1
	last := total // exclusive
	truncated := false
	if maxLines > 0 && first+maxLines < total {
		last = first + maxLines
		truncated = true
	}
	from := starts[first]
	to := len(buf)
	if last < total {
		to = starts[last]
	}
	return lineWindow{
		Data:       string(buf[from:to]),
		StartLine:  startLine,
		Lines:      last - first,
		TotalLines: total,
		Truncated:  truncated,
	}, nil
}

// lineWindowArgs reads start_line / max_lines off the raw argument map and
// rejects the removed byte parameters by name. Presence is checked on the raw
// map — a key whose value is 0 or null is still present — so a stale caller
// cannot have its offsets silently reinterpreted as lines. Values are JSON
// numbers with no fractional part; nothing is truncated into a different line.
func lineWindowArgs(args map[string]any) (startLine, maxLines int, err error) {
	for _, key := range []string{"offset", "length"} {
		if _, present := args[key]; present {
			return 0, 0, fmt.Errorf("%q was removed: ssh_file_read takes line numbers (start_line, max_lines), not byte offsets — use ssh_download for byte ranges", key)
		}
	}
	if startLine, err = wholeNumber(args, "start_line", 1); err != nil {
		return 0, 0, err
	}
	if maxLines, err = wholeNumber(args, "max_lines", 0); err != nil {
		return 0, 0, err
	}
	if startLine < 1 {
		return 0, 0, errStartLineBad
	}
	if maxLines < 0 {
		return 0, 0, errMaxLinesBad
	}
	return startLine, maxLines, nil
}

// wholeNumber reads an optional whole JSON number from the raw argument map.
// A key that is absent or null takes the default; anything that is not a
// number, or is fractional, is an error.
func wholeNumber(args map[string]any, key string, def int) (int, error) {
	v, present := args[key]
	if !present || v == nil {
		return def, nil
	}
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, fmt.Errorf("%q must be a whole number (got %T): pass a line number like 1", key, v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, fmt.Errorf("%q must be a whole number (got %v): pass a line number like 1", key, f)
	}
	if f > math.MaxInt32 || f < math.MinInt32 {
		return 0, fmt.Errorf("%q is out of range: pass a line number like 1", key)
	}
	return int(f), nil
}

// lineCount counts the lines of buf the way sliceLines addresses them.
func lineCount(buf []byte) int {
	return len(lineStarts(buf))
}

// startLineAt returns the 1-based line that byte offset idx falls on.
func startLineAt(buf []byte, idx int) int {
	if idx > len(buf) {
		idx = len(buf)
	}
	return 1 + bytes.Count(buf[:idx], []byte{'\n'})
}

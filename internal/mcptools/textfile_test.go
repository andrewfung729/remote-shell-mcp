package mcptools

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Seam 1: pure functions of bytes and arguments. No sshd, no session — the
// line-window and inline-budget rules of the text file channel, the removed
// byte parameters, and the ssh_file_edit matcher.

func TestSliceLines(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		start     int
		max       int
		data      string
		lines     int
		total     int
		truncated bool
	}{
		{name: "single line without newline", in: "a", start: 1, data: "a", lines: 1, total: 1},
		{name: "trailing newline is one line", in: "a\n", start: 1, data: "a\n", lines: 1, total: 1},
		{name: "blank line counts as a line", in: "a\n\n", start: 1, data: "a\n\n", lines: 2, total: 2},
		{name: "final line without newline counts", in: "a\nb", start: 1, data: "a\nb", lines: 2, total: 2},
		{name: "crlf is not normalized", in: "a\r\nb\r\n", start: 1, data: "a\r\nb\r\n", lines: 2, total: 2},
		{name: "only a newline is one line", in: "\n", start: 1, data: "\n", lines: 1, total: 1},
		{name: "empty file at line 1", in: "", start: 1, data: "", lines: 0, total: 0},
		{name: "window from line 2", in: "a\nb\nc\n", start: 2, data: "b\nc\n", lines: 2, total: 3},
		{name: "max_lines cuts short", in: "a\nb\nc\n", start: 1, max: 2, data: "a\nb\n", lines: 2, total: 3, truncated: true},
		{name: "max_lines reaches the end", in: "a\nb\nc\n", start: 1, max: 3, data: "a\nb\nc\n", lines: 3, total: 3},
		{name: "max_lines beyond the end", in: "a\nb\nc\n", start: 2, max: 10, data: "b\nc\n", lines: 2, total: 3},
		{name: "window ending on unterminated line", in: "a\nb", start: 2, data: "b", lines: 1, total: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sliceLines([]byte(tc.in), tc.start, tc.max)
			if err != nil {
				t.Fatalf("sliceLines(%q, %d, %d): %v", tc.in, tc.start, tc.max, err)
			}
			if got.Data != tc.data {
				t.Fatalf("data = %q, want %q", got.Data, tc.data)
			}
			if got.StartLine != tc.start {
				t.Fatalf("start_line = %d, want %d", got.StartLine, tc.start)
			}
			if got.Lines != tc.lines {
				t.Fatalf("lines = %d, want %d", got.Lines, tc.lines)
			}
			if got.TotalLines != tc.total {
				t.Fatalf("total_lines = %d, want %d", got.TotalLines, tc.total)
			}
			if got.Truncated != tc.truncated {
				t.Fatalf("truncated = %v, want %v", got.Truncated, tc.truncated)
			}
		})
	}
}

func TestSliceLinesRejectsBadNumbers(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		start int
		max   int
	}{
		{name: "start_line 0", in: "a\n", start: 0},
		{name: "start_line negative", in: "a\n", start: -3},
		{name: "max_lines negative", in: "a\n", start: 1, max: -1},
		{name: "start_line past the last line", in: "a\nb", start: 3},
		{name: "line 2 of an empty file", in: "", start: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sliceLines([]byte(tc.in), tc.start, tc.max); err == nil {
				t.Fatalf("sliceLines(%q, %d, %d) = nil error", tc.in, tc.start, tc.max)
			}
		})
	}
}

func TestSliceLinesEmptyFileWithMaxLinesSucceeds(t *testing.T) {
	got, err := sliceLines(nil, 1, 5)
	if err != nil {
		t.Fatalf("empty file, max_lines 5: %v", err)
	}
	if got.Lines != 0 || got.Data != "" || got.TotalLines != 0 || got.Truncated {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckInlineReadBudget(t *testing.T) {
	if err := checkInlineRead(bytes.Repeat([]byte("a"), inlineBudget)); err != nil {
		t.Fatalf("%d bytes must be eligible: %v", inlineBudget, err)
	}

	err := checkInlineRead(bytes.Repeat([]byte("a"), inlineBudget+1))
	if !errors.Is(err, errOverReadBudget) {
		t.Fatalf("%d bytes: err = %v, want the read-budget error", inlineBudget+1, err)
	}
	if !strings.Contains(err.Error(), "ssh_download") {
		t.Fatalf("read-budget error must name ssh_download: %v", err)
	}
}

func TestCheckInlineReadUTF8(t *testing.T) {
	err := checkInlineRead([]byte{0xff, 0xfe, 'a'})
	if !errors.Is(err, errNotUTF8) {
		t.Fatalf("invalid UTF-8: err = %v, want the UTF-8 error", err)
	}
	if !strings.Contains(err.Error(), "ssh_download") {
		t.Fatalf("UTF-8 error must name ssh_download: %v", err)
	}
}

// A file that is both over the budget and not valid UTF-8 gets the size
// error: one order, not two.
func TestCheckInlineReadSizeBeforeUTF8(t *testing.T) {
	buf := bytes.Repeat([]byte{0xff}, inlineBudget+1)
	if err := checkInlineRead(buf); !errors.Is(err, errOverReadBudget) {
		t.Fatalf("err = %v, want the read-budget error (size is checked first)", err)
	}
}

func TestCheckWriteBudget(t *testing.T) {
	if err := checkWriteBudget(inlineBudget); err != nil {
		t.Fatalf("%d bytes of data must be allowed: %v", inlineBudget, err)
	}
	err := checkWriteBudget(inlineBudget + 1)
	if !errors.Is(err, errOverWriteBudget) {
		t.Fatalf("err = %v, want the write-budget error", err)
	}
	if !strings.Contains(err.Error(), "ssh_upload") {
		t.Fatalf("write-budget error must name ssh_upload: %v", err)
	}
}

func TestCheckEditBudget(t *testing.T) {
	if err := checkEditBudget(inlineBudget); err != nil {
		t.Fatalf("%d bytes must be allowed: %v", inlineBudget, err)
	}
	err := checkEditBudget(inlineBudget + 1)
	if !errors.Is(err, errOverEditBudget) {
		t.Fatalf("err = %v, want the edit-budget error", err)
	}
	if !strings.Contains(err.Error(), "ssh_upload") {
		t.Fatalf("edit-budget error must name ssh_upload: %v", err)
	}
}

func TestLineWindowArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		start   int
		max     int
		wantErr bool
	}{
		{name: "defaults", args: map[string]any{"host": "h", "path": "/p"}, start: 1, max: 0},
		{name: "explicit", args: map[string]any{"start_line": float64(7), "max_lines": float64(3)}, start: 7, max: 3},
		{name: "start_line 0 is a tool error", args: map[string]any{"start_line": float64(0)}, wantErr: true},
		{name: "max_lines 0 means the whole file", args: map[string]any{"max_lines": float64(0)}, start: 1, max: 0},
		{name: "max_lines negative", args: map[string]any{"max_lines": float64(-2)}, wantErr: true},
		{name: "start_line fractional", args: map[string]any{"start_line": 1.5}, wantErr: true},
		{name: "max_lines fractional", args: map[string]any{"max_lines": 2.5}, wantErr: true},
		{name: "start_line as string", args: map[string]any{"start_line": "3"}, wantErr: true},
		{name: "start_line as bool", args: map[string]any{"start_line": true}, wantErr: true},
		{name: "max_lines as string", args: map[string]any{"max_lines": "3"}, wantErr: true},
		{name: "offset present as 0", args: map[string]any{"offset": float64(0), "start_line": float64(2)}, wantErr: true},
		{name: "offset present as null", args: map[string]any{"offset": nil}, wantErr: true},
		{name: "length present", args: map[string]any{"length": float64(10)}, wantErr: true},
		{name: "length present as null", args: map[string]any{"length": nil}, wantErr: true},
		{name: "offset and length together", args: map[string]any{"offset": float64(1), "length": float64(2)}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start, max, err := lineWindowArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("args %v = (%d, %d), want an error", tc.args, start, max)
				}
				return
			}
			if err != nil {
				t.Fatalf("args %v: %v", tc.args, err)
			}
			if start != tc.start || max != tc.max {
				t.Fatalf("args %v = (%d, %d), want (%d, %d)", tc.args, start, max, tc.start, tc.max)
			}
		})
	}
}

// The rejected byte parameters are named in the error so an old caller
// learns what replaced them.
func TestLineWindowArgsNamesRemovedByteParams(t *testing.T) {
	for _, key := range []string{"offset", "length"} {
		_, _, err := lineWindowArgs(map[string]any{key: float64(0)})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s rejection = %v, want an error naming %q", key, err, key)
		}
	}
}

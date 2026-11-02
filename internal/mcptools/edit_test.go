package mcptools

import (
	"strings"
	"testing"
)

// Seam 1: the exactly-once matcher behind ssh_file_edit — a pure function of
// bytes. Byte-exact, overlapping matches counted, no normalization.

func TestCountMatches(t *testing.T) {
	tests := []struct {
		name   string
		hay    string
		needle string
		count  int
		first  int
	}{
		{name: "no match", hay: "hello\nworld\n", needle: "absent", count: 0, first: -1},
		{name: "exactly one", hay: "a = 1\nb = 2\n", needle: "= 2", count: 1, first: 8},
		{name: "two matches", hay: "x = 1\ny = 1\n", needle: "= 1", count: 2, first: 2},
		{name: "overlapping matches count", hay: "aaa", needle: "aa", count: 2, first: 0},
		{name: "needle is the whole file", hay: "abc", needle: "abc", count: 1, first: 0},
		{name: "haystack shorter than needle", hay: "a", needle: "abc", count: 0, first: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			count, first := countMatches([]byte(tc.hay), []byte(tc.needle))
			if count != tc.count || first != tc.first {
				t.Fatalf("countMatches(%q, %q) = (%d, %d), want (%d, %d)", tc.hay, tc.needle, count, first, tc.count, tc.first)
			}
		})
	}
}

// Matching is byte-exact: CRLF in the file only matches CRLF in old_text.
func TestCountMatchesIsByteExact(t *testing.T) {
	hay := []byte("key = value\r\nnext = 2\r\n")
	if count, _ := countMatches(hay, []byte("key = value\r\n")); count != 1 {
		t.Fatalf("CRLF needle = %d matches, want 1", count)
	}
	if count, _ := countMatches(hay, []byte("key = value\n")); count != 0 {
		t.Fatalf("LF needle matched a CRLF file: %d matches", count)
	}
	if count, _ := countMatches(hay, []byte(" key = value")); count != 0 {
		t.Fatalf("whitespace-insensitive match leaked: %d matches", count)
	}
}

func TestApplyEdit(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		old   string
		new   string
		out   string
		start int
	}{
		{name: "middle replacement", in: "a\nb\nc\n", old: "b", new: "B", out: "a\nB\nc\n", start: 2},
		{name: "first line", in: "a\nb\n", old: "a", new: "A", out: "A\nb\n", start: 1},
		{name: "empty new text deletes", in: "keep\nremove\n", old: "remove\n", new: "", out: "keep\n", start: 2},
		{name: "replacement spanning lines", in: "one\ntwo\nthree\n", old: "one\ntwo", new: "1\n2", out: "1\n2\nthree\n", start: 1},
		{name: "crlf preserved", in: "a\r\nb\r\n", old: "a\r\n", new: "A\r\n", out: "A\r\nb\r\n", start: 1},
		{name: "unterminated last line", in: "a\nb", old: "b", new: "bee", out: "a\nbee", start: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hay := []byte(tc.in)
			count, idx := countMatches(hay, []byte(tc.old))
			if count != 1 {
				t.Fatalf("fixture matches %d times, want 1", count)
			}
			out := applyEdit(hay, idx, []byte(tc.old), []byte(tc.new))
			if string(out) != tc.out {
				t.Fatalf("applyEdit = %q, want %q", out, tc.out)
			}
			if got := startLineAt(out, idx); got != tc.start {
				t.Fatalf("start_line = %d, want %d", got, tc.start)
			}
			// The result must still be a file a read can return.
			if err := checkInlineRead(out); err != nil {
				t.Fatalf("edited buffer no longer readable: %v", err)
			}
		})
	}
}

func TestCheckEditTexts(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{name: "a real change", old: "a", new: "b"},
		{name: "empty new text is allowed", old: "a", new: ""},
		{name: "empty old text", old: "", new: "b", wantErr: "old_text"},
		{name: "no-op", old: "same", new: "same", wantErr: "identical"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkEditTexts(tc.old, tc.new)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkEditTexts(%q, %q) = %v", tc.old, tc.new, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkEditTexts(%q, %q) = %v, want an error mentioning %q", tc.old, tc.new, err, tc.wantErr)
			}
		})
	}
}

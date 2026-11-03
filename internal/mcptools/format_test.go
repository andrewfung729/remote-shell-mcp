package mcptools

import (
	"strings"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func TestFormatShellStatusLine(t *testing.T) {
	exit := 0
	seven := 7
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "done",
			got:  formatShell("hello\n", "done", "/srv/app", "default", &exit, false, false, false),
			want: "hello\n[exit 0 · cwd /srv/app]",
		},
		{
			name: "done no output",
			got:  formatShell("", "done", "/tmp", "default", &seven, false, false, false),
			want: "[exit 7 · cwd /tmp]",
		},
		{
			name: "running",
			got:  formatShell("partial", "running", "", "default", nil, false, false, false),
			want: "partial\n[running — poll to continue]",
		},
		{
			name: "truncated running",
			got:  formatShell("head", "running", "", "default", nil, true, false, false),
			want: "head\n[running — poll to continue · truncated, poll for full output]",
		},
		{
			name: "restarted and degraded and named",
			got:  formatShell("ok\n", "done", "/root", "scratch", &exit, false, true, true),
			want: "ok\n[exit 0 · cwd /root · shell restarted: cwd/env lost · degraded: sh fallback · shell scratch]",
		},
		{
			name: "default omits shell name",
			got:  formatShell("", "done", "/root", "default", &exit, false, true, false),
			want: "[exit 0 · cwd /root · shell restarted: cwd/env lost]",
		},
		{
			name: "reset is only the status line",
			got:  formatShell("", "done", "", "default", nil, false, true, false),
			want: "[shell restarted: cwd/env lost]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q\nwant %q", tc.got, tc.want)
			}
		})
	}
}

func TestFormatLineWindow(t *testing.T) {
	if got := formatLineWindow(lineWindow{}); got != "[0 lines]" {
		t.Fatalf("empty file: %q", got)
	}
	full := formatLineWindow(lineWindow{Data: "a\nb\n", StartLine: 1, Lines: 2, TotalLines: 2})
	if full != "a\nb\n[lines 1–2 of 2]" {
		t.Fatalf("complete: %q", full)
	}
	partial := formatLineWindow(lineWindow{Data: "x\n", StartLine: 1, Lines: 200, TotalLines: 950, Truncated: true})
	if partial != "x\n[lines 1–200 of 950 · more: start_line=201]" {
		t.Fatalf("partial: %q", partial)
	}
	if strings.Contains(partial, "\\n") {
		t.Fatalf("status escaped a newline: %q", partial)
	}
}

func TestFormatSmallResults(t *testing.T) {
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	list := formatFileList([]sshx.FileEntry{
		{Name: "src", Size: 4096, Mode: "drwxr-xr-x", ModTime: when, IsDir: true},
		{Name: "link", Size: 3, Mode: "lrwxrwxrwx", ModTime: when, Target: "src"},
	})
	want := "drwxr-xr-x 4096 2024-01-02T03:04:05Z src/\nlrwxrwxrwx 3 2024-01-02T03:04:05Z link -> src"
	if list != want {
		t.Fatalf("list:\n%s\nwant:\n%s", list, want)
	}
	if formatFileList(nil) != "" {
		t.Fatal("empty dir should be empty text")
	}
	stat := formatFileStat(sshx.FileEntry{Name: "a", Size: 4, Mode: "-rw-r--r--", ModTime: when})
	if stat != "name: a\nsize: 4\nmode: -rw-r--r--\nmod_time: 2024-01-02T03:04:05Z\nis_dir: false" {
		t.Fatalf("stat: %q", stat)
	}
	withTarget := formatFileStat(sshx.FileEntry{Name: "a", Mode: "lrwxrwxrwx", ModTime: when, Target: "b"})
	if !strings.HasSuffix(withTarget, "\ntarget: b") {
		t.Fatalf("stat target: %q", withTarget)
	}
	if formatWrite("/x", 1234) != "wrote 1234 bytes to /x" {
		t.Fatal(formatWrite("/x", 1234))
	}
	if formatEdit(10, 200, "a\nb\nc\nd\ne") != "edited lines 10–14 (file now 200 lines)" {
		t.Fatal(formatEdit(10, 200, "a\nb\nc\nd\ne"))
	}
	if formatEdit(10, 200, "line 1") != "edited line 10 (file now 200 lines)" {
		t.Fatal(formatEdit(10, 200, "line 1"))
	}
	if formatUpload("/r", 9) != "uploaded 9 bytes to /r" || formatDownload("/l", 9) != "downloaded 9 bytes to /l" {
		t.Fatal("transfer sentences")
	}
}

package mcptools

import (
	"path/filepath"
	"testing"
)

// localPath is the daemon-side half of the "~" rule: ssh_upload's local_path
// and ssh_download's local_path are on the daemon's own filesystem, so they
// expand against the daemon user's home.

func TestLocalPathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct{ in, want string }{
		{"~", home},
		{"~/x", home + "/x"},
		{"~/a/b.txt", home + "/a/b.txt"},
		{"~other/x", "~other/x"}, // another user's home is not ours to guess
		{"/abs/x", "/abs/x"},
		{"rel/x", "rel/x"},
		{".", "."},
	}
	for _, c := range cases {
		got, err := localPath(c.in)
		if err != nil {
			t.Fatalf("localPath(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("localPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if home != filepath.Clean(home) {
		t.Fatalf("test home %q is not clean", home)
	}
}

func TestLocalPathWithoutHomeIsAnError(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := localPath("~/x"); err == nil {
		t.Fatal("localPath(~/x) without a home dir = nil error, want a failure")
	}
	// A path that needs no home still works.
	if got, err := localPath("/abs/x"); err != nil || got != "/abs/x" {
		t.Fatalf("localPath(/abs/x) = %q, %v", got, err)
	}
}

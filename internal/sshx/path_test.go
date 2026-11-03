package sshx

import (
	"testing"
)

// Path resolution is pure string work plus one cached home dir, so all of it
// is unit-testable: a Session with home already set needs no connection.

func TestNeedsHome(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"~", true},
		{"~/", true},
		{"~/notes.txt", true},
		{"~/a/b/c", true},
		// Another user's home needs the remote passwd db — not our request.
		{"~viki/x", false},
		{"~viki", false},
		{"", false},
		{"/tmp/x", false},
		{"rel/x", false},
		{"a~/b", false},
		{".", false},
		{"..", false},
	}
	for _, c := range cases {
		if got := NeedsHome(c.in); got != c.want {
			t.Errorf("NeedsHome(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	const home = "/home/viki"
	cases := []struct {
		home, in, want string
	}{
		{home, "~", home},
		{home, "~/", home + "/"},
		{home, "~/notes.txt", home + "/notes.txt"},
		{home, "~/a/b", home + "/a/b"},
		// Trailing slashes on home are tolerated.
		{"/home/viki/", "~/a", "/home/viki/a"},
		// Not "~" forms: untouched.
		{home, "~viki/x", "~viki/x"},
		{home, "/abs", "/abs"},
		{home, "rel/x", "rel/x"},
		{home, ".", "."},
		// Unknown home: no expansion, so a caller can't silently get "~/x".
		{"", "~/a", "~/a"},
		{"", "~", "~"},
	}
	for _, c := range cases {
		if got := ExpandHome(c.home, c.in); got != c.want {
			t.Errorf("ExpandHome(%q, %q) = %q, want %q", c.home, c.in, got, c.want)
		}
	}
}

func TestResolvePath(t *testing.T) {
	// home is already cached, so ResolvePath needs no transport.
	s := &Session{home: "/home/viki"}
	cases := []struct{ in, want string }{
		{"~", "/home/viki"},
		{"~/notes.txt", "/home/viki/notes.txt"},
		{"/tmp/x", "/tmp/x"},
		{"rel/x", "rel/x"},
		{"~viki/x", "~viki/x"},
	}
	for _, c := range cases {
		got, err := s.ResolvePath(c.in)
		if err != nil {
			t.Fatalf("ResolvePath(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ResolvePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

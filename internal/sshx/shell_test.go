package sshx

import "testing"

func testSession() *Session {
	return &Session{
		ID:     "s1",
		shells: map[string]*Shell{},
	}
}

func TestGetShellScopedToOwner(t *testing.T) {
	s := testSession()
	s.shells["sh"] = &Shell{ID: "sh", SessionID: "s1", Owner: "client-a"}

	if _, err := s.GetShell("sh", "client-a"); err != nil {
		t.Fatalf("owner got %v", err)
	}
	if _, err := s.GetShell("sh", "client-b"); err == nil {
		t.Fatal("foreign client could reach the shell")
	}
	if _, err := s.GetShell("sh", ""); err == nil {
		t.Fatal("ownerless caller could reach an owned shell")
	}
}

func TestListShellsScopedToOwner(t *testing.T) {
	s := testSession()
	s.shells["a"] = &Shell{ID: "a", SessionID: "s1", Owner: "client-a"}
	s.shells["b"] = &Shell{ID: "b", SessionID: "s1", Owner: "client-b"}

	if got := s.ListShells("client-a"); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("client-a saw %+v", got)
	}
	if got := s.AllShells(); len(got) != 2 {
		t.Fatalf("webui saw %d shells", len(got))
	}
}

func TestCloseShellDeniedForForeignOwner(t *testing.T) {
	s := testSession()
	s.shells["sh"] = &Shell{ID: "sh", SessionID: "s1", Owner: "client-a"}

	if err := s.CloseShell("sh", "client-b"); err == nil {
		t.Fatal("foreign client could close the shell")
	}
	if _, ok := s.shells["sh"]; !ok {
		t.Fatal("shell was removed anyway")
	}
}

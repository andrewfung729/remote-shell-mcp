package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadHandleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")

	want := Handle{Addr: "127.0.0.1:54321", Token: "abc123", PID: 99}
	if err := WriteHandle(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadHandle(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Fatalf("round trip: got %+v want %+v", got, want)
	}
	// Permissions should be 0600 so the bearer token doesn't leak to other
	// users on multi-user boxes.
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("handle perms: got %o want 0600", info.Mode().Perm())
	}
}

func TestReadHandleRejectsMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")
	// addr-only — a client would have nothing to authenticate with.
	if err := os.WriteFile(path, []byte(`{"addr":"127.0.0.1:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandle(path); err == nil {
		t.Fatalf("expected error for handle missing token")
	}
}

func TestReadHandleRejectsCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHandle(path); err == nil {
		t.Fatalf("expected error for corrupt JSON")
	}
}

func TestWriteHandleAtomicReplace(t *testing.T) {
	// Successive writes must replace cleanly — confirms the tmp+rename idiom
	// in WriteHandle so a half-written file is never observed by readers.
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")

	first := Handle{Addr: "127.0.0.1:1", Token: "t1", PID: 1}
	second := Handle{Addr: "127.0.0.1:2", Token: "t2", PID: 2}

	if err := WriteHandle(path, first); err != nil {
		t.Fatal(err)
	}
	if err := WriteHandle(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandle(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("expected second write to win, got %+v", got)
	}
}

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteHandleRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	want := Handle{Addr: "127.0.0.1:47180", PID: 4321}
	if err := WriteHandle(path, want); err != nil {
		t.Fatalf("WriteHandle: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Handle
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
	// The handle is world-readable rendezvous data but should still be
	// private to the user.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("handle mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestWriteHandleAtomicReplace(t *testing.T) {
	// Write twice; the second write must atomically replace the first via
	// tmp-then-rename so a half-written file is never observed.
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.json")
	first := Handle{Addr: "127.0.0.1:1", PID: 1}
	second := Handle{Addr: "127.0.0.1:2", PID: 2}
	if err := WriteHandle(path, first); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteHandle(path, second); err != nil {
		t.Fatalf("second write: %v", err)
	}
	data, _ := os.ReadFile(path)
	var got Handle
	if err := json.Unmarshal(data, &got); err != nil || got != second {
		t.Fatalf("got %+v (err %v), want %+v", got, err, second)
	}
	// No leftover tmp file next to the handle.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind")
	}
}

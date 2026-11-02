package mcptools

import (
	"errors"
	"testing"
)

// Seam 1: the read-before-edit rules. An in-memory store of the content this
// client last saw, per (client, host, path) — no sshd, no daemon restart.

func TestReadGuardNeedsARecord(t *testing.T) {
	g := &ReadGuard{}
	if _, ok := g.Lookup("client", "host", "/p"); ok {
		t.Fatal("empty guard reported a record")
	}
	if err := g.Check("client", "host", "/p", []byte("x")); !errors.Is(err, errEditNeedsRead) {
		t.Fatalf("Check without a record = %v, want errEditNeedsRead", err)
	}
}

func TestReadGuardRejectsChangedContent(t *testing.T) {
	g := &ReadGuard{}
	g.Record("client", "host", "/p", []byte("hello\n"))
	if err := g.Check("client", "host", "/p", []byte("hello\n")); err != nil {
		t.Fatalf("unchanged content rejected: %v", err)
	}
	err := g.Check("client", "host", "/p", []byte("hello!\n"))
	if !errors.Is(err, errEditStale) {
		t.Fatalf("changed content = %v, want errEditStale", err)
	}
}

// The key is (client, host, path-as-sent): another client, another host, or
// another spelling of the same path has no record and fails loudly.
func TestReadGuardIsKeyedByClientHostAndPath(t *testing.T) {
	g := &ReadGuard{}
	g.Record("client", "host", "/p", []byte("x"))
	for _, tc := range []struct{ name, client, host, path string }{
		{name: "other client", client: "client2", host: "host", path: "/p"},
		{name: "other host", client: "client", host: "host2", path: "/p"},
		{name: "other path spelling", client: "client", host: "host", path: "/p/"},
	} {
		if _, ok := g.Lookup(tc.client, tc.host, tc.path); ok {
			t.Fatalf("%s found a record", tc.name)
		}
	}
}

// A windowed read still records the hash of the whole file, so the edit's own
// read can be compared against it.
func TestReadGuardAfterReadRecordsWholeFile(t *testing.T) {
	whole := []byte("line1\nline2\nline3\n")
	g := &ReadGuard{}
	g.AfterRead("client", "host", "/p", whole)
	if err := g.Check("client", "host", "/p", whole); err != nil {
		t.Fatalf("whole file after a windowed read: %v", err)
	}
	if err := g.Check("client", "host", "/p", []byte("line2\n")); !errors.Is(err, errEditStale) {
		t.Fatalf("windowed content accepted as the file: %v", err)
	}
}

func TestReadGuardAfterWrite(t *testing.T) {
	g := &ReadGuard{}

	g.AfterWrite("client", "host", "/p", []byte("new contents"), false, nil)
	if err := g.Check("client", "host", "/p", []byte("new contents")); err != nil {
		t.Fatalf("truncating write did not record: %v", err)
	}

	// An append leaves the content unknown.
	g.AfterWrite("client", "host", "/p", []byte(" more"), true, nil)
	if _, ok := g.Lookup("client", "host", "/p"); ok {
		t.Fatal("append kept a stale record")
	}

	// A failed write leaves the content unknown too.
	g.Record("client", "host", "/p", []byte("new contents"))
	g.AfterWrite("client", "host", "/p", []byte("other"), false, errors.New("boom"))
	if _, ok := g.Lookup("client", "host", "/p"); ok {
		t.Fatal("failed write kept a record")
	}
}

func TestReadGuardAfterEdit(t *testing.T) {
	g := &ReadGuard{}

	g.AfterEdit("client", "host", "/p", []byte("edited\n"), nil)
	if err := g.Check("client", "host", "/p", []byte("edited\n")); err != nil {
		t.Fatalf("successful edit did not record the result: %v", err)
	}

	// Two edits in a row need no read between them: the record is the result.
	g.AfterEdit("client", "host", "/p", []byte("edited twice\n"), nil)
	if err := g.Check("client", "host", "/p", []byte("edited twice\n")); err != nil {
		t.Fatalf("second edit did not record its result: %v", err)
	}

	g.AfterEdit("client", "host", "/p", []byte("partial"), errors.New("boom"))
	if _, ok := g.Lookup("client", "host", "/p"); ok {
		t.Fatal("failed edit kept a record")
	}
}

func TestReadGuardDrop(t *testing.T) {
	g := &ReadGuard{}
	g.Record("client", "host", "/p", []byte("x"))
	g.Drop("client", "host", "/p")
	if _, ok := g.Lookup("client", "host", "/p"); ok {
		t.Fatal("Drop left a record")
	}
	g.Drop("client", "host", "/p") // idempotent
}

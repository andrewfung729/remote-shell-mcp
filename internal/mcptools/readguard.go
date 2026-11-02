package mcptools

import (
	"crypto/sha256"
	"errors"
	"sync"
)

// This file holds the read-before-edit guard: one in-memory record per
// (MCP client identity, host, path) with the SHA-256 of the bytes this client
// last saw. ssh_file_edit refuses to run blind and refuses to overwrite a
// change it never saw. Records are never persisted and are lost on daemon
// restart; the check compares content, so a record that outlives a session or
// a reconnect is still safe.

// errEditStale refuses an edit whose read is out of date.
var errEditStale = errors.New("file changed since it was last read: call ssh_file_read again")

type readGuardKey struct {
	client string
	host   string
	path   string
}

// ReadGuard is the read-before-edit record store. Its zero value is ready to
// use, and records are never evicted.
type ReadGuard struct {
	mu sync.Mutex
	m  map[readGuardKey][sha256.Size]byte
}

// Lookup returns the recorded hash for this client's view of the file.
func (g *ReadGuard) Lookup(client, host, path string) ([sha256.Size]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	sum, ok := g.m[readGuardKey{client: client, host: host, path: path}]
	return sum, ok
}

// Record remembers content as this client's current view of the file.
func (g *ReadGuard) Record(client, host, path string, content []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[readGuardKey][sha256.Size]byte{}
	}
	g.m[readGuardKey{client: client, host: host, path: path}] = sha256.Sum256(content)
}

// Drop forgets this client's view of the file — used when a write or an edit
// left the content unknown. Dropping a path with no record is a no-op.
func (g *ReadGuard) Drop(client, host, path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, readGuardKey{client: client, host: host, path: path})
}

// Check compares content against the record. No record means the file was
// never read; a different hash means it changed underneath the client. Both
// are refusals to edit.
func (g *ReadGuard) Check(client, host, path string, content []byte) error {
	sum, ok := g.Lookup(client, host, path)
	if !ok {
		return errEditNeedsRead
	}
	if sum != sha256.Sum256(content) {
		return errEditStale
	}
	return nil
}

// AfterRead records a successful read. A windowed read still records the hash
// of the whole file, otherwise the edit's own full read could never match.
func (g *ReadGuard) AfterRead(client, host, path string, whole []byte) {
	g.Record(client, host, path, whole)
}

// AfterWrite applies the write rules: a truncating write records the data it
// put in the file; an append leaves the content unknown; a failed write leaves
// it unknown too. Both of those drop the record.
func (g *ReadGuard) AfterWrite(client, host, path string, data []byte, appendMode bool, err error) {
	if err != nil || appendMode {
		g.Drop(client, host, path)
		return
	}
	g.Record(client, host, path, data)
}

// AfterEdit records the file an edit produced, so two edits in a row need no
// read between them. A failed edit wrote an unknown amount — drop the record.
func (g *ReadGuard) AfterEdit(client, host, path string, result []byte, err error) {
	if err != nil {
		g.Drop(client, host, path)
		return
	}
	g.Record(client, host, path, result)
}

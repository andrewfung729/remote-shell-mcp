package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/mcptest"

	"golang.org/x/crypto/ssh"
)

// connectMessage is the only shape an agent is allowed to see for a dial failure.
func connectMessage(alias, category string) string {
	return fmt.Sprintf("cannot connect to %q (%s). Do not retry, run local ssh/scp/sftp/rsync, or edit ssh config — ask the user to check this host; details are on the WebUI hosts page.", alias, category)
}

func TestConnectionFailureHandOff(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_, _ = c.Write(bytesRepeat(300))
			time.Sleep(5 * time.Second)
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	closed.Close()

	key := writeTestKey(t)
	cfg := fmt.Sprintf("Host banner\n  HostName 127.0.0.1\n  Port %d\n  User alice\n  IdentityFile %s\nHost closed\n  HostName 127.0.0.1\n  Port %d\n  User alice\n  IdentityFile %s\nHost bare\n  HostName 127.0.0.1\n  Port 1\n", port, key, closedPort, key)
	a := startAppConfig(t, cfg, AppOptions{})
	a.login()
	for _, alias := range []string{"banner", "closed", "bare"} {
		a.setExposed(alias, true)
	}
	mc := mcptest.New(t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cases := []struct {
		alias    string
		category string
		raw      string // present on the WebUI, absent from the tool error
	}{
		{"banner", "port does not speak SSH", "overflow reading version string"},
		{"closed", "connection refused", "dial tcp"},
		{"bare", "ssh config incomplete", "doesn't set User"},
	}
	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			text, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": tc.alias, "command": "id"})
			if err == nil {
				t.Fatalf("ssh_shell succeeded: %q", text)
			}
			want := connectMessage(tc.alias, tc.category)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), tc.raw) {
				t.Fatalf("agent saw raw library text %q in %q", tc.raw, err.Error())
			}
			var shown string
			for _, h := range a.hosts().Hosts {
				if h.Alias == tc.alias {
					shown = h.LastConnectError
					if h.LastConnectErrorAt.IsZero() {
						t.Fatal("failure has no time")
					}
				}
			}
			if !strings.Contains(shown, tc.raw) {
				t.Fatalf("WebUI last failure = %q, want it to contain %q", shown, tc.raw)
			}
		})
	}

	// A retry dials again; the failure is not a cached answer.
	if _, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": "banner", "command": "id"}); err == nil {
		t.Fatal("retry succeeded")
	}
	if accepts.Load() < 2 {
		t.Fatalf("banner listener saw %d connections, want at least 2", accepts.Load())
	}

	// Parallel first calls on a cold host share the wrapped failure.
	a2 := startAppConfig(t, fmt.Sprintf("Host closed\n  HostName 127.0.0.1\n  Port %d\n  User alice\n  IdentityFile %s\n", closedPort, key), AppOptions{})
	a2.login()
	a2.setExposed("closed", true)
	const n = 4
	clients := make([]*mcptest.Client, n)
	for i := range clients {
		clients[i] = mcptest.New(t, a2.addr)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, errs[i] = clients[i].CallText(cctx, "ssh_file_list", map[string]any{"host": "closed", "path": "/"})
		}(i)
	}
	close(start)
	wg.Wait()
	want := connectMessage("closed", "connection refused")
	for i, err := range errs {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("parallel call %d = %v, want %q", i, err, want)
		}
	}
}

func bytesRepeat(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'X'
	}
	return b
}

func writeTestKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

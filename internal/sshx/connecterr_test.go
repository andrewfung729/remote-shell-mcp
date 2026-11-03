package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestConnectCategory(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"config", fmt.Errorf("%w: no User", errConfigIncomplete), CatConfigIncomplete},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}, CatDNS},
		{"refused", fmt.Errorf("all addresses failed: %w", refused), CatRefused},
		{"deadline", fmt.Errorf("connect to %q: %w", "h", context.DeadlineExceeded), CatTimeout},
		{"net timeout", &net.OpError{Op: "dial", Err: timeoutErr{}}, CatTimeout},
		{"banner", fmt.Errorf("all addresses failed: ssh: handshake failed: %w", errors.New("ssh: overflow reading version string")), CatNotSSH},
		{"host key", fmt.Errorf("%w: host key verification failed: %w", errHostKeyUntrusted, &knownhosts.KeyError{}), CatHostKey},
		{"auth typed", fmt.Errorf("ssh: handshake failed: %w", ssh.ServerAuthError{}), CatAuth},
		{"auth text", errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [publickey], no supported methods remain"), CatAuth},
		{"generic hides raw", errors.New("something the library invented"), CatFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConnectCategory(tc.err); got != tc.want {
				t.Fatalf("category = %q, want %q (err %v)", got, tc.want, tc.err)
			}
		})
	}
}

// timeoutErr is a net.Error that is a timeout and not a DNS failure.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

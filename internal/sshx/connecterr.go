package sshx

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Failure categories an agent is allowed to see. The raw library error stays
// on the daemon (log + WebUI); the agent only gets one of these phrases.
const (
	CatConfigIncomplete = "ssh config incomplete"
	CatDNS              = "host name does not resolve"
	CatRefused          = "connection refused"
	CatTimeout          = "timed out"
	CatNotSSH           = "port does not speak SSH"
	CatHostKey          = "host key not trusted"
	CatAuth             = "authentication rejected"
	CatFailed           = "connection failed"
)

// errConfigIncomplete marks a dial that never had a usable ssh config entry
// (no Host, no User, a jump without User, no identity). The wrapped text is
// for the WebUI; agents only see CatConfigIncomplete.
var errConfigIncomplete = errors.New(CatConfigIncomplete)

// errHostKeyUntrusted marks a known_hosts rejection or a missing known_hosts
// file. The wrapped text tells the user to ssh once; agents only see CatHostKey.
var errHostKeyUntrusted = errors.New(CatHostKey)

// ConnectCategory maps a dial error to the phrase the agent is shown.
// Typed errors win. Message matching is only for what the SSH library
// returns as a plain string (version-banner overflow, auth rejection).
func ConnectCategory(err error) string {
	if err == nil {
		return CatFailed
	}
	if errors.Is(err, errConfigIncomplete) {
		return CatConfigIncomplete
	}
	var keyErr *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	if errors.Is(err, errHostKeyUntrusted) || errors.As(err, &keyErr) || errors.As(err, &revoked) {
		return CatHostKey
	}
	var authPtr *ssh.ServerAuthError
	var authVal ssh.ServerAuthError
	if errors.As(err, &authPtr) || errors.As(err, &authVal) || strings.Contains(err.Error(), "unable to authenticate") {
		return CatAuth
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() && !dns.IsNotFound {
			return CatTimeout
		}
		return CatDNS
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return CatRefused
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CatTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return CatTimeout
	}
	if strings.Contains(err.Error(), "overflow reading version string") {
		return CatNotSSH
	}
	return CatFailed
}

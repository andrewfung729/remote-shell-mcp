package sshx

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type AuthSpec struct {
	KeyPath       string `json:"key_path,omitempty"`
	KeyPassphrase string `json:"key_passphrase,omitempty"`
	UseAgent      bool   `json:"use_agent,omitempty"`
	// AgentSocket overrides the default $SSH_AUTH_SOCK lookup. Useful when
	// the daemon was started without the env var (e.g. spawned by an MCP
	// host that doesn't propagate user env), or when you want to point at
	// a specific agent like 1Password's (~/Library/.../1password/t/agent.sock)
	// or `gpg-agent --enable-ssh-support`.
	AgentSocket string `json:"agent_socket,omitempty"`
	Password    string `json:"password,omitempty"`
}

// closerList lets callers cleanly release auth-time resources (e.g. the
// ssh-agent unix socket) after ssh.Dial has completed.
type closerList []io.Closer

func (cl closerList) Close() {
	for _, c := range cl {
		_ = c.Close()
	}
}

// Build returns the auth methods plus a Closer that releases anything Methods
// opened (currently just the ssh-agent socket). Always defer cl.Close() in
// the caller, even on error — Build closes its own state on error paths but
// the caller must close on the happy path too.
func (a AuthSpec) Build() ([]ssh.AuthMethod, closerList, error) {
	var methods []ssh.AuthMethod
	var closers closerList

	if a.UseAgent {
		sock := a.AgentSocket
		if sock == "" {
			sock = os.Getenv("SSH_AUTH_SOCK")
		}
		if sock == "" {
			return nil, nil, errors.New("ssh-agent requested but neither agent_socket nor SSH_AUTH_SOCK is set")
		}
		// Allow `~` in the path so callers can pass "~/Library/.../agent.sock"
		// without expanding it themselves.
		if strings.HasPrefix(sock, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				sock = home + sock[1:]
			}
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, nil, fmt.Errorf("dial ssh-agent: %w", err)
		}
		closers = append(closers, conn)
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}

	if a.KeyPath != "" {
		data, err := os.ReadFile(a.KeyPath)
		if err != nil {
			closers.Close()
			// Don't echo the key path back to the MCP client; the file is
			// often in a private user directory and the path itself is
			// reconnaissance. Log the path on the daemon if you need details.
			return nil, nil, fmt.Errorf("read private key: %w", err)
		}
		var signer ssh.Signer
		if a.KeyPassphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(a.KeyPassphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(data)
		}
		if err != nil {
			closers.Close()
			return nil, nil, fmt.Errorf("parse private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if a.Password != "" {
		methods = append(methods, ssh.Password(a.Password))
	}

	if len(methods) == 0 {
		closers.Close()
		return nil, nil, errors.New("no authentication method provided (set key_path, use_agent, or password)")
	}
	return methods, closers, nil
}

// HostKeyAlgorithmsFor returns the host-key algorithm names that the user's
// known_hosts file has entries for, so we can advertise only those. Without
// this, the server may present (say) an RSA key for a host we only have an
// ed25519 entry for, and the knownhosts callback errors with "key mismatch"
// even though the same hostname has the same key the user is happy with
// from their CLI. Returns nil if the file is missing or has no matching
// entries (caller should leave HostKeyAlgorithms unset → Go's defaults).
func HostKeyAlgorithmsFor(knownHostsPath, host string) []string {
	if insecureModeEnabled := host == ""; insecureModeEnabled {
		return nil
	}
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		knownHostsPath = home + "/.ssh/known_hosts"
	}
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	rest := data
	for len(rest) > 0 {
		_, hosts, pubKey, _, next, err := ssh.ParseKnownHosts(rest)
		rest = next
		if err != nil {
			// Bad line — stop trying to recover; if the file is fully
			// unreadable we'd rather fall back to default algos than guess.
			break
		}
		if pubKey == nil {
			continue
		}
		for _, h := range hosts {
			if knownHostsHostMatches(h, host) {
				seen[pubKey.Type()] = struct{}{}
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	return out
}

// knownHostsHostMatches handles the plaintext patterns OpenSSH writes:
//
//	host
//	[host]:port
//	host,otherhost,…
//
// Hashed entries (|1|salt|hash) are skipped — the user's file is plaintext
// in practice. If someone needs hashed-only support we can plumb it later.
func knownHostsHostMatches(pattern, host string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "|") {
		return false // hashed; out of scope for now
	}
	// "[host]:port" — extract inner host.
	if strings.HasPrefix(pattern, "[") {
		if end := strings.IndexByte(pattern, ']'); end > 0 {
			return pattern[1:end] == host
		}
	}
	return pattern == host
}

func HostKeyCallback(insecure bool, knownHostsPath string) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	if knownHostsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locate home dir: %w", err)
		}
		knownHostsPath = home + "/.ssh/known_hosts"
	}
	if _, err := os.Stat(knownHostsPath); err != nil {
		// Mention that a known_hosts file is missing without echoing the
		// (possibly home-directory) path back to the MCP client.
		return nil, fmt.Errorf("known_hosts file is not readable: %w (pass insecure=true to skip host key verification)", err)
	}
	return knownhosts.New(knownHostsPath)
}

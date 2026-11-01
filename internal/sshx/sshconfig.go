package sshx

import (
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// resolveFromSSHConfig fills in missing fields on a ConnectSpec from the
// user's ~/.ssh/config, the same way the `ssh` CLI does. Anything the caller
// set explicitly is preserved; only empty fields are filled. Set
// spec.DisableSSHConfig to skip entirely (useful for tests or fully-explicit
// automation).
//
// Mapping from OpenSSH option → ConnectSpec field:
//
//	Hostname        → addresses[i] (alias replaced with the real host:port)
//	Port            → addresses[i] (only when no explicit port was given)
//	User            → spec.User
//	IdentityFile    → spec.Auth.KeyPath  (only if no other auth is set)
//	IdentityAgent   → spec.Auth.UseAgent + spec.Auth.AgentSocket
//	ProxyJump       → spec.JumpHosts (chained)
func resolveFromSSHConfig(spec *ConnectSpec) {
	if spec.DisableSSHConfig {
		return
	}
	if len(spec.Addresses) == 0 {
		return
	}

	// Treat the first address as an SSH alias. Strip any explicit port the
	// caller supplied so we can ask ssh_config for its Hostname/Port.
	rawFirst := strings.TrimSpace(spec.Addresses[0])
	alias, explicitPort := splitHostPort(rawFirst)
	if alias == "" {
		return
	}

	// Hostname: replace the alias with the real host, keeping the explicit
	// port if one was given.
	hostname := nonEmpty(ssh_config.Get(alias, "Hostname"))
	if hostname != "" && hostname != alias {
		port := explicitPort
		if port == "" {
			port = nonEmpty(ssh_config.Get(alias, "Port"))
			if port == "" {
				port = "22"
			}
		}
		spec.Addresses[0] = net.JoinHostPort(hostname, port)
	} else if explicitPort == "" {
		// No alias remapping, but the caller didn't specify a port and
		// the config has one — apply it.
		if port := nonEmpty(ssh_config.Get(alias, "Port")); port != "" && port != "22" {
			spec.Addresses[0] = net.JoinHostPort(alias, port)
		}
	}

	if spec.User == "" {
		if u := nonEmpty(ssh_config.Get(alias, "User")); u != "" {
			spec.User = u
		}
	}

	// Auth: only fill if no auth method is explicit. Otherwise the caller's
	// explicit choice wins entirely.
	if spec.Auth.KeyPath == "" && !spec.Auth.UseAgent {
		if ag := nonEmpty(ssh_config.Get(alias, "IdentityAgent")); ag != "" {
			spec.Auth.UseAgent = true
			spec.Auth.AgentSocket = expandHome(ag)
		} else if id := nonEmpty(ssh_config.Get(alias, "IdentityFile")); id != "" {
			spec.Auth.KeyPath = expandHome(id)
		}
	}

	// ProxyJump: only fill if no jump chain is explicit.
	if len(spec.JumpHosts) == 0 {
		if pj := nonEmpty(ssh_config.Get(alias, "ProxyJump")); pj != "" {
			for _, hop := range splitCommaList(pj) {
				spec.JumpHosts = append(spec.JumpHosts, parseJumpHopFromConfig(hop))
			}
		}
	}
}

// nonEmpty trims quotes and whitespace from a config value; some entries are
// quoted (e.g. IdentityAgent "~/path with spaces/sock").
func nonEmpty(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	return v
}

func splitHostPort(s string) (host, port string) {
	if h, p, err := net.SplitHostPort(s); err == nil {
		return h, p
	}
	return s, ""
}

func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func splitCommaList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseJumpHopFromConfig parses `[user@]host[:port]` (the ProxyJump format)
// into a JumpHost and recursively resolves any nested config for that hop's
// alias.
func parseJumpHopFromConfig(s string) JumpHost {
	user := ""
	if i := strings.Index(s, "@"); i >= 0 {
		user = s[:i]
		s = s[i+1:]
	}
	host, port := splitHostPort(s)
	if host == "" {
		host = s
	}
	addr := host
	if port != "" {
		addr = net.JoinHostPort(host, port)
	}
	jh := JumpHost{User: user, Addresses: []string{addr}}
	// Resolve the hop's own ssh_config entry (e.g. its IdentityAgent).
	jhSpec := &ConnectSpec{User: user, Addresses: []string{addr}}
	resolveFromSSHConfig(jhSpec)
	jh.User = jhSpec.User
	jh.Addresses = jhSpec.Addresses
	jh.Auth = jhSpec.Auth
	return jh
}

// validatePortInt is a small sanity check used elsewhere; kept here so
// resolveFromSSHConfig's helpers live in one file.
var _ = strconv.Itoa

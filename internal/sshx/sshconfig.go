package sshx

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// errNoSSHConfigEntry is returned by Connect when the first address matches
// no concrete Host in the daemon user's ~/.ssh/config. Callers can't supply
// credentials themselves, so there is nothing to fall back to — fix the
// config (or expose the host via the WebUI).
var errNoSSHConfigEntry = errors.New("no matching Host in ~/.ssh/config: connection details and auth resolve only from ssh_config, and a wildcard or missing entry can't be used")

// resolveFromSSHConfig fills in missing fields on a ConnectSpec from the
// user's ~/.ssh/config, the same way the `ssh` CLI does. It is also the
// *only* source of auth — callers cannot pass credentials. Anything the
// caller set explicitly (user, addresses, jump_hosts) is preserved; only
// empty fields are filled.
//
// The config is read through decodeSSHConfig (the same file path
// ConfiguredHosts uses: $HOME/.ssh/config, Include-aware at the listing
// layer). We do NOT use ssh_config.Get — its global DefaultUserSettings
// resolves the home dir via the passwd entry, not $HOME, so it can't see
// an overridden HOME.
//
//	Hostname        → addresses[i] (alias replaced with the real host:port)
//	Port            → addresses[i] (only when no explicit port was given)
//	User            → spec.User
//	IdentityFile    → spec.Auth.KeyPath
//	IdentityAgent   → spec.Auth.UseAgent + spec.Auth.AgentSocket
//	ProxyJump       → spec.JumpHosts (chained)
func resolveFromSSHConfig(spec *ConnectSpec) {
	if len(spec.Addresses) == 0 {
		return
	}
	rawFirst := strings.TrimSpace(spec.Addresses[0])
	alias, _ := splitHostPort(rawFirst)
	cfg := loadUserSSHConfig()
	if cfg == nil {
		return
	}
	resolveAliasFromConfig(spec, cfg, alias)
}

// loadUserSSHConfig parses the daemon user's ssh config once per call;
// a missing/unreadable file means "no resolution" — callers then fail
// on their own missing-field checks.
func loadUserSSHConfig() *ssh_config.Config {
	path, err := UserSSHConfigPath()
	if err != nil {
		return nil
	}
	cfg, err := decodeSSHConfig(path)
	if err != nil {
		return nil
	}
	return cfg
}

// resolveAliasFromConfig fills spec fields from cfg's entry for alias.
func resolveAliasFromConfig(spec *ConnectSpec, cfg *ssh_config.Config, alias string) {
	rawFirst := strings.TrimSpace(spec.Addresses[0])
	_, explicitPort := splitHostPort(rawFirst)

	// Hostname: replace the alias with the real host, keeping the explicit
	// port if one was given.
	hostname := nonEmpty(cfgGet(cfg, alias, "Hostname"))
	if hostname != "" && hostname != alias {
		port := explicitPort
		if port == "" {
			port = nonEmpty(cfgGet(cfg, alias, "Port"))
			if port == "" {
				port = "22"
			}
		}
		spec.Addresses[0] = net.JoinHostPort(hostname, port)
	} else if explicitPort == "" {
		if port := nonEmpty(cfgGet(cfg, alias, "Port")); port != "" && port != "22" {
			spec.Addresses[0] = net.JoinHostPort(alias, port)
		}
	}

	if spec.User == "" {
		if u := nonEmpty(cfgGet(cfg, alias, "User")); u != "" {
			spec.User = u
		}
	}

	// Auth always comes from ssh_config (IdentityAgent preferred, then
	// IdentityFile); the caller can't pick credentials.
	if ag := nonEmpty(cfgGet(cfg, alias, "IdentityAgent")); ag != "" {
		spec.Auth.UseAgent = true
		spec.Auth.AgentSocket = expandHome(ag)
	} else if id := nonEmpty(cfgGet(cfg, alias, "IdentityFile")); id != "" {
		spec.Auth.KeyPath = expandHome(id)
	}

	// ProxyJump: only fill if no jump chain is explicit.
	if len(spec.JumpHosts) == 0 {
		if pj := nonEmpty(cfgGet(cfg, alias, "ProxyJump")); pj != "" {
			for _, hop := range splitCommaList(pj) {
				spec.JumpHosts = append(spec.JumpHosts, parseJumpHopFromConfig(cfg, hop))
			}
		}
	}
}

// cfgGet wraps Config.Get: kevinburke Config.Get returns (val, err); err
// only means "key not present" so we flatten both to the value.
func cfgGet(cfg *ssh_config.Config, alias, key string) string {
	v, _ := cfg.Get(alias, key)
	return v
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
func parseJumpHopFromConfig(cfg *ssh_config.Config, s string) JumpHost {
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
	resolveAliasFromConfig(jhSpec, cfg, host)
	jh.User = jhSpec.User
	jh.Addresses = jhSpec.Addresses
	jh.Auth = jhSpec.Auth
	return jh
}

// validatePortInt is a small sanity check used elsewhere; kept here so
// resolveFromSSHConfig's helpers live in one file.
var _ = strconv.Itoa

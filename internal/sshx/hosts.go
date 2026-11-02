package sshx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// ConfiguredHost is one concrete Host alias from the daemon user's
// ~/.ssh/config (and its Include files). Wildcard patterns and Match blocks
// are omitted. Key paths are not part of this view.
type ConfiguredHost struct {
	Alias     string `json:"alias"`
	User      string `json:"user,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Port      string `json:"port,omitempty"`
	ProxyJump string `json:"proxy_jump,omitempty"`
}

// ConfiguredHosts reads path the way OpenSSH reads a user config: Include
// globs are followed (absolute, ~/ , or relative to ~/.ssh), at most five
// levels deep. An empty path uses ~/.ssh/config. A missing file is an empty
// list. The first concrete alias wins; user, hostname, port, and ProxyJump
// come from the same first-match rules ssh_config uses.
func ConfiguredHosts(path string) ([]ConfiguredHost, error) {
	if path == "" {
		var err error
		path, err = UserSSHConfigPath()
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return []ConfiguredHost{}, nil
		}
		return nil, err
	}
	root, err := decodeSSHConfig(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []ConfiguredHost
	if err := walkSSHConfig(root, 0, seen, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []ConfiguredHost{}
	}
	return out, nil
}

// hasConfigEntry reports whether alias names a concrete Host in the daemon
// user's ~/.ssh/config (the same set ConfiguredHosts / the WebUI lists).
// Callers can't supply auth, so without an entry there is nothing to
// resolve against — Connect fails rather than guess.
func hasConfigEntry(alias string) bool {
	if alias == "" {
		return false
	}
	hosts, err := ConfiguredHosts("")
	if err != nil {
		return false
	}
	for _, h := range hosts {
		if h.Alias == alias {
			return true
		}
	}
	return false
}

// UserSSHConfigPath is the OpenSSH user config path this daemon reads.
func UserSSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

func decodeSSHConfig(path string) (*ssh_config.Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ssh_config.DecodeBytes(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

const maxSSHIncludeDepth = 5

func walkSSHConfig(cfg *ssh_config.Config, depth int, seen map[string]bool, out *[]ConfiguredHost) error {
	if depth > maxSSHIncludeDepth {
		return fmt.Errorf("ssh config include depth exceeded")
	}
	for _, h := range cfg.Hosts {
		// Match blocks are conditions, not aliases. Their Include nodes are
		// still real files and have to be walked — the parser attaches a
		// top-level Include that follows a Match to that Match.
		if !isMatchBlock(h) {
			for _, pat := range h.Patterns {
				alias := pat.String()
				if !concreteAlias(alias) || seen[alias] {
					continue
				}
				seen[alias] = true
				*out = append(*out, resolveConfiguredHost(cfg, alias))
			}
		}
		for _, n := range h.Nodes {
			for _, glob := range includeGlobs(n) {
				matches, err := expandUserInclude(glob)
				if err != nil {
					return err
				}
				for _, m := range matches {
					sub, err := decodeSSHConfig(m)
					if err != nil {
						return err
					}
					if err := walkSSHConfig(sub, depth+1, seen, out); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func resolveConfiguredHost(cfg *ssh_config.Config, alias string) ConfiguredHost {
	user, _ := cfg.Get(alias, "User")
	hostname, _ := cfg.Get(alias, "HostName")
	if hostname == "" {
		hostname = alias
	}
	port, _ := cfg.Get(alias, "Port")
	if port == "" {
		port = "22"
	}
	jump, _ := cfg.Get(alias, "ProxyJump")
	return ConfiguredHost{
		Alias:     alias,
		User:      user,
		Hostname:  hostname,
		Port:      port,
		ProxyJump: jump,
	}
}

func concreteAlias(alias string) bool {
	if alias == "" || strings.HasPrefix(alias, "!") {
		return false
	}
	return !strings.ContainsAny(alias, "*?")
}

func isMatchBlock(h *ssh_config.Host) bool {
	line := h.String()
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return strings.HasPrefix(strings.TrimSpace(line), "Match")
}

func includeGlobs(n ssh_config.Node) []string {
	inc, ok := n.(*ssh_config.Include)
	if !ok {
		return nil
	}
	s := strings.TrimSpace(inc.String())
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "Include")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "=")
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// expandUserInclude matches OpenSSH user-config Include lookup: absolute
// paths and ~/ stay as given; anything else is relative to ~/.ssh.
func expandUserInclude(directive string) ([]string, error) {
	directive = strings.TrimSpace(directive)
	var path string
	switch {
	case filepath.IsAbs(directive):
		path = directive
	case strings.HasPrefix(directive, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, directive[2:])
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".ssh", directive)
	}
	return filepath.Glob(path)
}

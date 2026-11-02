package mcptools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

// SSHHostsTool lists concrete Host aliases from the daemon user's ssh config.
// It is hidden from agents until the user opts in on the WebUI. The page
// itself always shows the list, through /api/hosts, not through this tool.
const SSHHostsTool = "ssh_hosts"

func RegisterSSHHosts(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool(SSHHostsTool,
		mcp.WithDescription("List the SSH host aliases the user has exposed to agents. Each entry is just an alias name — connection details from the daemon user's ssh config are never returned. Hosts the user left off are omitted. The tool itself is hidden until at least one host is exposed. To list already-open connections use ssh_sessions."),
	), handleSSHHosts(st))
}

func handleSSHHosts(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		hosts, err := sshx.ConfiguredHosts("")
		if err != nil {
			return resultErr(err)
		}
		exposed := exposedOnly(hosts, st.ExposedHosts())
		// Aliases only — user/hostname/port/ProxyJump stay daemon-side so the
		// agent can't reconstruct the target from config details it was never
		// meant to read.
		aliases := make([]string, 0, len(exposed))
		for _, h := range exposed {
			aliases = append(aliases, h.Alias)
		}
		return st.resultJSON(aliases)
	}
}

// exposedOnly keeps hosts whose alias is in the exposed set, in config order.
func exposedOnly(hosts []sshx.ConfiguredHost, exposed []string) []sshx.ConfiguredHost {
	if len(exposed) == 0 {
		return []sshx.ConfiguredHost{}
	}
	on := map[string]bool{}
	for _, a := range exposed {
		on[a] = true
	}
	out := make([]sshx.ConfiguredHost, 0, len(exposed))
	for _, h := range hosts {
		if on[h.Alias] {
			out = append(out, h)
		}
	}
	return out
}

// WithHostsExposure shows SSHHostsTool only when anyExposed is true, and
// strips a stale disabled entry when it is. False is the default: no host
// has been exposed yet.
func WithHostsExposure(disabled []string, anyExposed bool) []string {
	out := make([]string, 0, len(disabled)+1)
	for _, n := range disabled {
		if n == SSHHostsTool {
			continue
		}
		out = append(out, n)
	}
	if !anyExposed {
		out = append(out, SSHHostsTool)
	}
	return out
}

// HostsExposedFrom reports whether SSHHostsTool is currently visible to agents.
func HostsExposedFrom(disabled []string) bool {
	for _, n := range disabled {
		if n == SSHHostsTool {
			return false
		}
	}
	return true
}

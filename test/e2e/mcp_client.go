//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPClient wraps the mcp-go library client over the spec Streamable HTTP
// transport, talking to the daemon's /mcp endpoint — the same path a real MCP
// client takes (there is no stdio launcher). The Call/CallText/CallJSON
// helpers keep test call sites identical to the old /rpc-era client: Call
// returns the raw CallToolResult JSON, CallText unwraps the first text
// content block, CallJSON unmarshals it into a struct.
type MCPClient struct {
	t *testing.T
	c *mcpclient.Client
	tr *transport.StreamableHTTP
}

// NewMCPClientForDaemon connects a streamable-HTTP MCP client to a running
// daemon at http://<addr>/mcp. Loopback binds skip bearer auth, but we pass
// the handle-file token anyway so this client also works against a
// -require-auth daemon, same as a remote client would.
func NewMCPClientForDaemon(t *testing.T, d *Daemon) *MCPClient {
	t.Helper()
	tr, err := transport.NewStreamableHTTP(
		"http://"+d.addr+"/mcp",
		transport.WithHTTPHeaderFunc(func(ctx context.Context) map[string]string {
			// Resolve the token per-request instead of capturing it at
			// connect time: after a daemon restart the handle file holds a
			// fresh token, and a headerFunc picks it up without rebuilding
			// the transport.
			token, err := daemon_ReadHandle(d.handlePath)
			if err != nil || token == "" {
				return nil
			}
			return map[string]string{"Authorization": "Bearer " + token}
		}),
	)
	if err != nil {
		t.Fatalf("new streamable transport: %v", err)
	}
	c := mcpclient.NewClient(tr)
	mc := &MCPClient{t: t, c: c, tr: tr}
	mc.initialize()
	return mc
}

// NewMCPClientForAddr is like NewMCPClientForDaemon but takes an addr +
// handle path directly — for tests that keep the MCP client alive across a
// daemon restart, where the client object must outlive any Daemon handle.
func NewMCPClientForAddr(t *testing.T, addr, handlePath string) *MCPClient {
	t.Helper()
	return NewMCPClientForDaemon(t, &Daemon{addr: addr, handlePath: handlePath})
}

func (c *MCPClient) initialize() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.c.Start(ctx); err != nil {
		c.t.Fatalf("client start: %v", err)
	}
	var req mcp.InitializeRequest
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "e2e", Version: "1"}
	if _, err := c.c.Initialize(ctx, req); err != nil {
		c.t.Fatalf("initialize: %v", err)
	}
}

// Reinitialize performs the MCP initialize handshake again without rebuilding
// the transport. Used by the daemon-restart test: the new daemon keeps the
// client's session id valid (the server's session-id manager is stateless —
// it validates format, not existence), so a re-handshake revives the client
// in place, which is exactly what a reconnecting real-world client does.
func (c *MCPClient) Reinitialize() {
	c.initialize()
}

// SessionID returns the negotiated MCP session id ("" until initialize).
// Only valid while the underlying transport is a StreamableHTTP.
func (c *MCPClient) SessionID() string {
	return c.tr.GetSessionId()
}

// Call invokes a tool and returns the raw CallToolResult as JSON — same
// shape the old /rpc client returned from a tools/call request.
func (c *MCPClient) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	c.t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name = tool
	if args == nil {
		args = map[string]any{}
	}
	req.Params.Arguments = args
	res, err := c.c.CallTool(ctx, req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	return raw, nil
}

func (c *MCPClient) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
	var req mcp.CallToolRequest
	req.Params.Name = tool
	if args == nil {
		args = map[string]any{}
	}
	req.Params.Arguments = args
	res, err := c.c.CallTool(ctx, req)
	if err != nil {
		return "", err
	}
	if len(res.Content) == 0 {
		return "", nil
	}
	var text string
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		text = tc.Text
	}
	if res.IsError {
		return text, fmt.Errorf("tool error: %s", text)
	}
	return text, nil
}

func (c *MCPClient) CallJSON(ctx context.Context, tool string, args map[string]any, out any) error {
	text, err := c.CallText(ctx, tool, args)
	if err != nil {
		return err
	}
	if text == "" || out == nil {
		return nil
	}
	return json.Unmarshal([]byte(text), out)
}

// Close terminates the MCP session (DELETE to /mcp) and releases the
// transport's resources.
func (c *MCPClient) Close() {
	_ = c.c.Close()
}

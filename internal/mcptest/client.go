// Package mcptest provides a test-only MCP client over the spec Streamable
// HTTP transport. It talks to a daemon's /mcp endpoint — the same path a real
// MCP client takes — and is shared by the in-process unit tests and the e2e
// suite, so there is exactly one client implementation to keep honest.
package mcptest

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

// Client wraps the mcp-go library client. Call/CallText/CallJSON keep call
// sites uniform: Call returns the raw CallToolResult JSON, CallText returns
// the first text block as-is, CallJSON unmarshals that text into a struct.
type Client struct {
	t  *testing.T
	c  *mcpclient.Client
	tr *transport.StreamableHTTP
}

// New connects a client to http://<addr>/mcp and completes the initialize
// handshake. The daemon is loopback-only and unauthenticated — no headers.
func New(t *testing.T, addr string) *Client {
	t.Helper()
	tr, err := transport.NewStreamableHTTP("http://" + addr + "/mcp")
	if err != nil {
		t.Fatalf("new streamable transport: %v", err)
	}
	return NewWithTransport(t, tr)
}

// NewWithTransport is New for a caller-built transport — e.g. one created
// with transport.WithContinuousListening so server notifications arrive.
func NewWithTransport(t *testing.T, tr *transport.StreamableHTTP) *Client {
	t.Helper()
	c := &Client{t: t, c: mcpclient.NewClient(tr), tr: tr}
	c.initialize()
	return c
}

func (c *Client) initialize() {
	c.t.Helper()
	// Start takes the client's lifetime context, not the handshake timeout:
	// the continuous-listening goroutine outlives the initialize call (the
	// transport cancels it from Close). Handing it a soon-cancelled context
	// silently kills server->client notifications.
	if err := c.c.Start(context.Background()); err != nil {
		c.t.Fatalf("client start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var req mcp.InitializeRequest
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1"}
	if _, err := c.c.Initialize(ctx, req); err != nil {
		c.t.Fatalf("initialize: %v", err)
	}
}

// ListTools returns the tool names the server currently advertises.
func (c *Client) ListTools(ctx context.Context) ([]string, error) {
	res, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// OutputSchemaTypes returns each tool's output-schema type, or "" when the
// tool declares none. Text-only tools declare none.
func (c *Client) OutputSchemaTypes(ctx context.Context) (map[string]string, error) {
	res, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		out[tool.Name] = tool.OutputSchema.Type
	}
	return out, nil
}

func (c *Client) list(ctx context.Context) (*mcp.ListToolsResult, error) {
	return c.c.ListTools(ctx, mcp.ListToolsRequest{})
}

// OnNotification registers a handler for server notifications such as
// notifications/tools/list_changed.
func (c *Client) OnNotification(fn func(mcp.JSONRPCNotification)) {
	c.c.OnNotification(fn)
}

// CallTool invokes a tool and returns the raw result. Unlike CallText it does
// not turn a tool error result into a Go error, so callers can tell "the tool
// ran and reported an error" from "the tool is not registered".
func (c *Client) CallTool(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	var req mcp.CallToolRequest
	req.Params.Name = tool
	if args == nil {
		args = map[string]any{}
	}
	req.Params.Arguments = args
	return c.c.CallTool(ctx, req)
}

// Call invokes a tool and returns the raw CallToolResult as JSON.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
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

// CallText invokes a tool and returns its first text content block. A tool
// error result is returned as that text plus a non-nil error.
func (c *Client) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
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
	var text string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if res.IsError {
		return text, fmt.Errorf("tool error: %s", text)
	}
	return text, nil
}

// CallJSON calls a tool and unmarshals its text block into out.
func (c *Client) CallJSON(ctx context.Context, tool string, args map[string]any, out any) error {
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
func (c *Client) Close() {
	_ = c.c.Close()
}

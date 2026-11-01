//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// MCPClient is a test MCP client that talks to the daemon's /rpc endpoint
// over plain HTTP. There is no stdio launcher anymore — the daemon is the
// only binary and serves a one-shot JSON-RPC POST endpoint, so a test client
// is just an HTTP POST with a bearer token.
type MCPClient struct {
	t     *testing.T
	url   string
	token string
	hc    *http.Client

	nextID atomic.Int64
	closed atomic.Bool
}

// NewMCPClientForDaemon connects a test client to a running daemon. The
// bearer token is read from the daemon's handle file.
func NewMCPClientForDaemon(t *testing.T, d *Daemon) *MCPClient {
	t.Helper()
	token, err := daemon_ReadHandle(d.handlePath)
	if err != nil {
		t.Fatalf("read daemon handle for token: %v", err)
	}
	c := &MCPClient{
		t:     t,
		url:   "http://" + d.addr + "/rpc",
		token: token,
		hc:    &http.Client{Timeout: 0},
	}
	c.initialize()
	return c
}

// do posts a single JSON-RPC message and returns the raw result. Notifications
// (no id) return nil — /rpc answers them with 204 No Content.
func (c *MCPClient) do(ctx context.Context, payload any) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("client closed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc http %d: %s", resp.StatusCode, string(body))
	}
	var env struct {
		Result json.RawMessage  `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode rpc response: %w (body: %s)", err, string(body))
	}
	if env.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", string(*env.Error))
	}
	return env.Result, nil
}

func (c *MCPClient) initialize() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id := c.nextID.Add(1)
	if _, err := c.do(ctx, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e", "version": "1"},
		},
	}); err != nil {
		c.t.Fatalf("initialize: %v", err)
	}
	_, _ = c.do(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (c *MCPClient) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	c.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	id := c.nextID.Add(1)
	return c.do(ctx, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
}

func (c *MCPClient) CallText(ctx context.Context, tool string, args map[string]any) (string, error) {
	raw, err := c.Call(ctx, tool, args)
	if err != nil {
		return "", err
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", nil
	}
	if result.IsError {
		return result.Content[0].Text, fmt.Errorf("tool error: %s", result.Content[0].Text)
	}
	return result.Content[0].Text, nil
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

func (c *MCPClient) Close() {
	c.closed.Store(true)
}

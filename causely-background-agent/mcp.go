package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type mcpClient struct {
	url    string
	token  string // optional bearer token; empty means no Authorization header
	client *http.Client
	seq    int
}

func newMCPClient(url string, token string) *mcpClient {
	return &mcpClient{url: url, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

type mcpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      int    `json:"id"`
}

type mcpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *mcpClient) post(method string, params any) (json.RawMessage, error) {
	c.seq++
	req := mcpRequest{JSONRPC: "2.0", Method: method, Params: params, ID: c.seq}
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequest("POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp build request %s: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The Causely MCP server (Streamable HTTP transport, per the MCP spec) always
	// wraps responses as SSE ("event: message\ndata: {...}") regardless of what
	// Accept header is sent. Ask for it explicitly so intent is documented;
	// extractSSEData below handles the actual unwrapping either way.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp post %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	payload := extractSSEData(raw)

	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("mcp decode: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("mcp error: %s", result.Error.Message)
	}
	return result.Result, nil
}

// extractSSEData unwraps a Streamable-HTTP SSE response body ("event: message\n
// data: {...}\n\n") down to the last "data:" line's JSON payload. Falls back to
// returning the raw body unchanged if it isn't SSE-framed, so this is safe to
// call unconditionally regardless of how a given MCP server responds.
func extractSSEData(raw []byte) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	var lastData []byte
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			lastData = bytes.TrimSpace(data)
		}
	}
	if lastData != nil {
		return lastData
	}
	return raw
}

// ListTools returns available tools from the Causely MCP server.
func (c *mcpClient) ListTools() ([]mcpToolDef, error) {
	data, err := c.post("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []mcpToolDef `json:"tools"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// CallTool executes a Causely MCP tool and returns the text result.
func (c *mcpClient) CallTool(name string, args map[string]any) (string, error) {
	data, err := c.post("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []mcpContentBlock `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if len(out.Content) > 0 {
		return out.Content[0].Text, nil
	}
	return string(data), nil
}

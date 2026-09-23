package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// mcpProtocolVersion is the MCP spec revision this client speaks during the
// initialize handshake. Bump deliberately, not automatically — a server may
// negotiate down to an older version it supports (see initialize()).
const mcpProtocolVersion = "2025-06-18"

type mcpClient struct {
	url   string
	token string // optional bearer token; empty means no Authorization header
	// clientID/clientSecret, if both set, take precedence over token: they're sent as
	// HTTP Basic credentials instead of a Bearer token. This is for MCP servers (e.g.
	// Causely's own, on tenants with Frontegg auth enabled) that exchange client_id:secret
	// for a short-lived access token themselves and cache it server-side — the caller
	// never has to fetch/refresh a token, just always sends the same static pair.
	clientID     string
	clientSecret string
	client       *http.Client
	// seq is an atomic counter, not a plain int: ensureInitialized explicitly
	// supports concurrent callers once initialized, and JSON-RPC request ids
	// must be unique — a plain c.seq++ is a read-modify-write race under
	// concurrent CallTool/ListTools calls.
	seq atomic.Int64

	// MCP connection lifecycle state (see ensureInitialized). initMu makes
	// initialize+notifications/initialized run to completion exactly once at a
	// time, however many goroutines start calling tools concurrently, but
	// deliberately does NOT cache failure the way sync.Once would — a
	// transient MCP outage during the first call would otherwise permanently
	// poison this client (e.g. the poll loop's one long-lived mcpClient)
	// until process restart. Only success is remembered; a failed attempt is
	// retried on the next call.
	initMu      sync.Mutex
	initialized bool
	sessionMu   sync.Mutex
	sessionID   string // Mcp-Session-Id, if the server assigned one; empty otherwise
}

func newMCPClient(url string, token string) *mcpClient {
	return &mcpClient{url: url, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

// newMCPClientBasicAuth creates a client that authenticates with HTTP Basic
// credentials (clientID:clientSecret) instead of a bearer token — see mcpClient.
func newMCPClientBasicAuth(url, clientID, clientSecret string) *mcpClient {
	return &mcpClient{url: url, clientID: clientID, clientSecret: clientSecret, client: &http.Client{Timeout: 30 * time.Second}}
}

type mcpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	// ID is a pointer so a JSON-RPC *notification* (e.g.
	// notifications/initialized) can omit it entirely, per spec — omitempty on
	// a plain int would instead serialize a real request's id:0 as absent.
	ID *int `json:"id,omitempty"`
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
	return c.postContext(context.Background(), method, params)
}

// postContext sends a JSON-RPC request and returns its result — the low-level
// transport used by every real MCP method. It does NOT itself perform the
// initialize handshake; callers that need it (ListToolsContext,
// CallToolContext) call ensureInitialized first. initialize() and notify()
// call this and buildRequest directly to avoid recursing back into
// ensureInitialized.
func (c *mcpClient) postContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := int(c.seq.Add(1))
	raw, err := c.do(ctx, mcpRequest{JSONRPC: "2.0", Method: method, Params: params, ID: &id})
	if err != nil {
		return nil, err
	}

	var result struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		// Include a prefix of the actual response so a non-JSON body (an auth
		// gateway's plain-text "unauthorized", an HTML error page, a proxy
		// timeout page) is diagnosable from the error alone — confirmed live,
		// a decode failure with only the json error ("invalid character 'u'
		// looking for beginning of value") gave no way to tell what the
		// server actually sent.
		return nil, fmt.Errorf("mcp decode: %w (response: %q)", err, truncate(string(raw), 300))
	}
	if result.Error != nil {
		return nil, fmt.Errorf("mcp error: %s", result.Error.Message)
	}
	return result.Result, nil
}

// notify sends a JSON-RPC *notification* — no id, no result expected (the
// spec has the server reply 202 Accepted with an empty body). Used for
// notifications/initialized, the second half of the MCP handshake.
func (c *mcpClient) notify(ctx context.Context, method string, params any) error {
	_, err := c.do(ctx, mcpRequest{JSONRPC: "2.0", Method: method, Params: params})
	return err
}

// do sends one JSON-RPC message (request or notification) and returns the
// raw (SSE-unwrapped) response body. It attaches the session ID from a prior
// initialize response, if any, and captures one from this response if the
// server sends it — the MCP Streamable HTTP transport lets a server assign
// Mcp-Session-Id on any response, not only initialize's.
func (c *mcpClient) do(ctx context.Context, req mcpRequest) ([]byte, error) {
	body, _ := json.Marshal(req)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp build request %s: %w", req.Method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// The Causely MCP server (Streamable HTTP transport, per the MCP spec) always
	// wraps responses as SSE ("event: message\ndata: {...}") regardless of what
	// Accept header is sent. Ask for it explicitly so intent is documented;
	// extractSSEData below handles the actual unwrapping either way.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid := c.getSessionID(); sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(httpReq.Header))
	switch {
	case c.clientID != "" && c.clientSecret != "":
		basic := base64.StdEncoding.EncodeToString([]byte(c.clientID + ":" + c.clientSecret))
		httpReq.Header.Set("Authorization", "Basic "+basic)
	case c.token != "":
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp post %s: %w", req.Method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// A non-2xx status (auth gateway 401/403, proxy 5xx, etc.) must fail here,
	// not fall through to unmarshal — an empty or generically-shaped error
	// body can otherwise decode into a zero-value "success" (no result, no
	// error field either), silently treating a real failure — including of
	// notifications/initialized, whose success path never checks a result at
	// all — as if the call worked.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp post %s: HTTP %d (response: %q)", req.Method, resp.StatusCode, truncate(string(raw), 300))
	}
	// Only retain a session id from a response we've already confirmed
	// succeeded — capturing it before the status check meant a malformed/
	// partial response (real status non-2xx, but a session id header still
	// present) could poison sessionID with an id the server never actually
	// validated.
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.setSessionID(sid)
	}
	return extractSSEData(raw), nil
}

func (c *mcpClient) getSessionID() string {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	return c.sessionID
}

func (c *mcpClient) setSessionID(id string) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.sessionID = id
}

// ensureInitialized performs the MCP connection lifecycle — initialize, then
// the notifications/initialized notification — once successfully per client,
// however many goroutines start calling tools concurrently. Required by the
// MCP spec for any session-based/stateful server; skipping it happens to work
// against Causely's own permissive endpoint (which tolerates a bare
// tools/list with no prior handshake) but breaks against a compliant server
// that actually enforces it. A failed attempt is NOT cached — see initMu's
// doc comment — so a transient outage is retried on the next call rather
// than permanently disabling this client.
func (c *mcpClient) ensureInitialized(ctx context.Context) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.initialized {
		return nil
	}
	if err := c.initialize(ctx); err != nil {
		// Clear any session id captured during this failed attempt — whether
		// initialize itself failed or notifications/initialized failed after
		// a successful initialize, the handshake as a whole didn't complete,
		// so a retry must start a fresh session negotiation rather than
		// reusing an id from a half-established one.
		c.setSessionID("")
		return err
	}
	c.initialized = true
	return nil
}

func (c *mcpClient) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "causely-background-agent",
			"version": "1.0.0",
		},
	}
	if _, err := c.postContext(ctx, "initialize", params); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp notifications/initialized: %w", err)
	}
	return nil
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
	return c.ListToolsContext(context.Background())
}

func (c *mcpClient) ListToolsContext(ctx context.Context) ([]mcpToolDef, error) {
	if err := c.ensureInitialized(ctx); err != nil {
		return nil, err
	}
	data, err := c.postContext(ctx, "tools/list", nil)
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
	return c.CallToolContext(context.Background(), name, args)
}

// CallToolContext carries the enclosing agent invocation context. This lets
// Beyla correlate its payload-derived MCP span without duplicate SDK spans.
func (c *mcpClient) CallToolContext(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := c.ensureInitialized(ctx); err != nil {
		return "", err
	}
	data, err := c.postContext(ctx, "tools/call", map[string]any{
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

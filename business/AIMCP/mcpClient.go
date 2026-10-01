// Package business (AIMCP) is the MCP (Model Context Protocol) integration: a
// minimal client that speaks MCP over Streamable HTTP to external tool servers,
// plus (in sibling files) the orchestration that introspects those servers and
// registers their tools into the shared AI tool registry.
//
// This file is the transport: JSON-RPC 2.0 (initialize / tools/list /
// tools/call) over a single HTTP endpoint, handling both a direct JSON response
// and an SSE-framed (text/event-stream) response, plus MCP session continuity
// via the Mcp-Session-Id header. No third-party SDK — net/http + encoding/json.
package business

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// protocolVersion is the MCP version we advertise in the initialize handshake.
const protocolVersion = "2025-06-18"

// requestTimeout bounds a single JSON-RPC round trip. MCP tools may do real
// work (API calls), so this is generous but finite.
const requestTimeout = 30 * time.Second

// maxResponseBytes caps how much we read from an MCP server response, so a
// hostile or buggy server can't exhaust memory.
const maxResponseBytes = 4 << 20 // 4 MiB

// McpTool is one tool advertised by an MCP server (tools/list entry). Schema is
// the raw JSON Schema object describing the tool's arguments.
type McpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Annotations are the MCP tool behavior hints (spec: tool annotations). They
	// are UNTRUSTED input from an external server: they feed the host-side
	// classifier (classifyToolRisk) where they may only raise risk, never lower
	// it — generically, for any server.
	Annotations *McpToolAnnotations `json:"annotations,omitempty"`
}

// McpToolAnnotations is the subset of the MCP tool-annotations object we act on.
// All fields are standard MCP tool-annotation hints, so any conformant server
// (not just GitHub) can drive OneCamp's governance the same way.
type McpToolAnnotations struct {
	// Title is a human-readable label for the tool (spec: title).
	Title string `json:"title,omitempty"`
	// ReadOnlyHint: the server claims the tool does not modify its environment.
	// It is a NECESSARY but not sufficient condition for auto-run: the host also
	// requires a credibly read-shaped name with no mutating/destructive token
	// (so delete_file or get_or_create with readOnlyHint=true is still a write).
	// When false/absent the tool is always a write routed through confirmation.
	ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
	// DestructiveHint: the tool may perform irreversible/destructive updates
	// (delete, drop, overwrite). Trusted only in the risk-RAISING direction: a
	// declared destructive tool is high-risk regardless of any contradictory
	// readOnlyHint, kept out of auto-run and routed through human approval.
	DestructiveHint bool `json:"destructiveHint,omitempty"`
	// IdempotentHint: repeated calls with the same args have no additional
	// effect. Informational (surfaced to the model) — a non-idempotent write is
	// riskier to retry.
	IdempotentHint bool `json:"idempotentHint,omitempty"`
	// OpenWorldHint: the tool interacts with an open/external world (the
	// internet) rather than a closed system. Informational.
	OpenWorldHint bool `json:"openWorldHint,omitempty"`
}

// Client talks to one MCP server. Construct via NewClient; not safe for
// concurrent use across goroutines (each call mutates sessionID), so callers
// create one per operation.
type Client struct {
	url        string
	authType   string
	authHeader string
	authSecret string
	http       *http.Client
	sessionID  string
	nextID     int
}

// ErrAuthSecretUnreadable is returned when a server's stored auth secret cannot be decrypted.
//
// Named rather than generic so callers can tell it apart from a transport failure: nothing about
// the remote is wrong, and retrying will not help until an admin re-enters the secret.
var ErrAuthSecretUnreadable = errors.New(
	"mcp server auth secret cannot be decrypted (usually an AI_CONFIG_KEK change); " +
		"re-enter the secret in admin MCP settings")

// NewClient builds a client for a registered server.
//
// REFUSES A SERVER WHOSE SECRET CANNOT BE READ, rather than calling it without one. The model
// leaves AuthSecret empty when the stored blob will not decrypt, and an empty secret on a server
// configured to require one is not a degraded call — it is an unauthenticated call to an external
// endpoint that is about to receive workspace data. If the remote refuses it, the error names the
// wrong cause; if the remote does not refuse it, the auth was never enforced.
func NewClient(s *model.McpServer) (*Client, error) {
	if s.AuthSecretUnreadable {
		return nil, ErrAuthSecretUnreadable
	}
	authHeader := ""
	if s.AuthHeaderName != nil {
		authHeader = *s.AuthHeaderName
	}
	return &Client{
		url:        s.URL,
		authType:   s.AuthType,
		authHeader: authHeader,
		authSecret: s.AuthSecret,
		http:       newMCPHTTPClient(),
	}, nil
}

// newMCPHTTPClient builds the outbound HTTP client used for every MCP call,
// with an SSRF guard. An MCP server URL is admin-supplied and may legitimately
// be an INTERNAL endpoint (a self-hosted MCP server in the same cluster), so we
// deliberately do NOT block private/loopback ranges - that would defeat the
// product, same as the AI provider guard. What we DO block, at dial time (so
// DNS rebinding can't bypass it) and across redirects, is link-local, which
// includes the cloud metadata endpoint (169.254.169.254) - the one target with
// no legitimate use and a real credential-theft risk.
func newMCPHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, fmt.Errorf("mcp dial lookup failed: %w", err)
			}
			// Validate every resolved IP, then dial the validated IP DIRECTLY
			// (not the hostname). Dialing addr would let the dialer re-resolve
			// DNS independently, reopening a rebinding window between our check
			// and the connect; pinning the connection to an IP we just verified
			// closes it.
			var chosen net.IP
			for _, ip := range ips {
				if isBlockedMCPIP(ip) {
					return nil, fmt.Errorf("mcp dial blocked address for host %s: %s", host, ip)
				}
				if chosen == nil {
					chosen = ip
				}
			}
			if chosen == nil {
				return nil, fmt.Errorf("mcp dial: no address found for host %s", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(chosen.String(), port))
		},
	}
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			if ip := net.ParseIP(req.URL.Hostname()); ip != nil && isBlockedMCPIP(ip) {
				return fmt.Errorf("mcp redirect to blocked address: %s", ip)
			}
			return nil
		},
	}
}

// isBlockedMCPIP reports whether an IP must never be reached by an MCP call:
// link-local (which contains the 169.254.169.254 cloud metadata endpoint),
// multicast, and the unspecified address. Private/loopback are intentionally
// allowed so internal/self-hosted MCP servers stay reachable.
func isBlockedMCPIP(ip net.IP) bool {
	return ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// jsonRPCRequest is a JSON-RPC 2.0 request envelope.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response envelope.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

// ListTools performs the initialize handshake then returns the server's tools.
// It is the single entry point used both by the admin "test connection" probe
// and the registry rebuild.
func (c *Client) ListTools(ctx context.Context) ([]McpTool, error) {
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []McpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse tools/list result: %w", err)
	}
	return out.Tools, nil
}

// CallTool invokes a tool by name with the given arguments and returns the
// flattened text content of the result. An MCP "isError" result is surfaced as
// a Go error so the agent runner records it like any other tool failure.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error) {
	if err := c.initialize(ctx); err != nil {
		return "", err
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	raw, err := c.call(ctx, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}
	var b strings.Builder
	for _, part := range res.Content {
		if part.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
	}
	text := strings.TrimSpace(b.String())
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

// initialize runs the MCP handshake once per client, capturing the session id
// and sending the initialized notification the spec requires.
func (c *Client) initialize(ctx context.Context) error {
	if c.sessionID != "" {
		return nil // already initialized
	}
	_, err := c.call(ctx, "initialize", map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "OneCamp", "version": "1.0"},
	})
	if err != nil {
		return err
	}
	// Best-effort initialized notification (no id → no response expected).
	_ = c.notify(ctx, "notifications/initialized")
	if c.sessionID == "" {
		// Some servers don't use sessions; mark initialized so we don't loop.
		c.sessionID = "stateless"
	}
	return nil
}

// call sends a JSON-RPC request and returns its result payload.
func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.nextID++
	reqBody := jsonRPCRequest{JSONRPC: "2.0", ID: c.nextID, Method: method, Params: params}
	resp, err := c.do(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// notify sends a JSON-RPC notification (no id, no response parsing).
func (c *Client) notify(ctx context.Context, method string) error {
	body := map[string]interface{}{"jsonrpc": "2.0", "method": method}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBytes))
	res.Body.Close()
	return nil
}

// do performs one HTTP POST and parses the JSON-RPC response, transparently
// handling a direct JSON body or an SSE (text/event-stream) framing.
func (c *Client) do(ctx context.Context, reqBody jsonRPCRequest) (*jsonRPCResponse, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.applyHeaders(req)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP request failed: %w", err)
	}
	defer res.Body.Close()

	// Capture a session id if the server issued one.
	if sid := res.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("MCP server returned %d: %s", res.StatusCode, strings.TrimSpace(string(snippet)))
	}

	body := io.LimitReader(res.Body, maxResponseBytes)
	contentType := res.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEResponse(body, reqBody.ID)
	}

	var out jsonRPCResponse
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parse MCP response: %w", err)
	}
	return &out, nil
}

// applyHeaders sets the JSON-RPC + auth + session headers on a request.
func (c *Client) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" && c.sessionID != "stateless" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	switch c.authType {
	case model.AuthBearer:
		if c.authSecret != "" {
			req.Header.Set("Authorization", "Bearer "+c.authSecret)
		}
	case model.AuthHeader:
		if c.authHeader != "" && c.authSecret != "" {
			req.Header.Set(c.authHeader, c.authSecret)
		}
	}
}

// parseSSEResponse reads a text/event-stream body and returns the JSON-RPC
// message whose id matches the request (the first matching "data:" payload).
func parseSSEResponse(body io.Reader, wantID int) (*jsonRPCResponse, error) {
	var found *jsonRPCResponse
	err := ai.ForEachSSEFrame(body, maxResponseBytes, func(data []byte) (bool, error) {
		var out jsonRPCResponse
		if uerr := json.Unmarshal(data, &out); uerr != nil {
			return false, nil // not a JSON-RPC frame; skip
		}
		if out.Result == nil && out.Error == nil {
			return false, nil
		}
		// Match the response to our request id (ignore unrelated notifications).
		var id int
		if len(out.ID) > 0 {
			_ = json.Unmarshal(out.ID, &id)
		}
		if id != wantID && len(out.ID) != 0 {
			return false, nil
		}
		found = &out
		return true, nil // stop: the rest of the stream is not ours
	})
	if err != nil {
		return nil, fmt.Errorf("read MCP stream: %w", err)
	}
	if found == nil {
		return nil, fmt.Errorf("no JSON-RPC response found in MCP stream")
	}
	return found, nil
}

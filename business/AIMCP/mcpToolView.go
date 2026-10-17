package business

// Exported, transport-shaped view of what OneCamp ENFORCES for an MCP tool, so
// any caller (the admin API today, any future surface) can ask "what actually
// happens when an agent calls this tool?" without reading the external server's
// self-description.
//
// classifyToolRisk (mcpRegistry.go) stays the single source of truth: the agent
// loop, the protected-branch guard and this view all resolve risk through that
// one function, so tightening a rule relabels every surface at once. The
// provider's own annotations are deliberately NOT part of this view — they are
// untrusted input that may only raise risk, and must never be shown to an admin
// as if they were the enforced rule.
//
// Classification happens HERE, at the API boundary, from the raw tools_cache —
// the cache keeps the provider's untouched truth, so a classifier change
// re-labels every existing server instantly, with no migration and no
// re-introspection.

import (
	"encoding/json"
	"net/url"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// ToolRisk is the enforced risk of one tool. Both flags false means "write,
// routed through approval/autonomy gating" — the fail-closed default.
type ToolRisk struct {
	// ReadOnly means the tool auto-runs inside the agent loop: its result goes
	// straight back to the model with no human in the middle.
	ReadOnly bool `json:"read_only"`
	// Destructive means the write is irreversible/high-risk: never auto-run
	// unattended, and surfaced with a destructive warning on the approval card.
	Destructive bool `json:"destructive"`
}

// ClassifyTool reports the risk OneCamp enforces for one MCP tool. Thin
// exported wrapper over the internal classifier — no rules are duplicated here.
func ClassifyTool(t McpTool) ToolRisk {
	risk := classifyToolRisk(t)
	return ToolRisk{ReadOnly: risk.ReadOnly, Destructive: risk.Destructive}
}

// ToolView is one MCP tool as the admin API returns it: the provider's own
// name/description/schema plus OneCamp's enforced classification (inlined as
// read_only/destructive).
type ToolView struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	ToolRisk
}

// ClassifyTools enriches a tool list (e.g. a live tools/list result) with the
// enforced classification. Never nil, so a JSON response is always an array.
func ClassifyTools(tools []McpTool) []ToolView {
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			ToolRisk:    ClassifyTool(t),
		})
	}
	return out
}

// ServerTools derives a stored server's classified tool list from its raw
// tools_cache. A missing or unparseable cache yields an empty list (the same
// thing the registry does with it).
func ServerTools(s *model.McpServer) []ToolView {
	if s == nil {
		return []ToolView{}
	}
	raw := strings.TrimSpace(s.ToolsCache)
	if raw == "" {
		return []ToolView{}
	}
	var tools []McpTool
	if err := json.Unmarshal([]byte(raw), &tools); err != nil {
		return []ToolView{}
	}
	return ClassifyTools(tools)
}

// ServerView is a stored server as the admin API returns it: every field of the
// row (tools_cache included, still raw) plus a classified tools array. The
// embedded pointer keeps the JSON shape backwards compatible — existing clients
// see exactly the fields they saw before, plus "tools".
type ServerView struct {
	*model.McpServer
	Tools []ToolView `json:"tools"`
	// CanManage says whether the person reading may change, switch, test or
	// remove this server (its creator, or an admin), so the web shows those
	// controls only to them. The server enforces it either way.
	CanManage bool `json:"can_manage"`
}

// NewServerView wraps one server row as the actor sees it; nil in, nil out.
//
// Someone who may not manage the server sees only its host: the rest of an
// address can carry what isn't theirs to read (a key in its query, a private
// path), and so can its last connection error, which quotes the address.
func NewServerView(s *model.McpServer, actor Actor) *ServerView {
	if s == nil {
		return nil
	}
	canManage := s.ManageableBy(actor.UserID, actor.IsAdmin)
	shown := s
	if !canManage {
		c := *s
		c.URL = hostOnly(s.URL)
		if c.LastError != nil {
			msg := "It can't connect right now."
			c.LastError = &msg
		}
		shown = &c
	}
	return &ServerView{McpServer: shown, Tools: ServerTools(s), CanManage: canManage}
}

// hostOnly is an address's scheme and host (with its port), or "" when it
// doesn't parse.
func hostOnly(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// NewServerViews wraps a list of server rows. Never nil.
func NewServerViews(servers []*model.McpServer, actor Actor) []*ServerView {
	out := make([]*ServerView, 0, len(servers))
	for _, s := range servers {
		if v := NewServerView(s, actor); v != nil {
			out = append(out, v)
		}
	}
	return out
}

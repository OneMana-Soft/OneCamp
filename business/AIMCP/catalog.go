package business

// Curated MCP connector catalog.
//
// MCP is OneCamp's design-sanctioned path for external integrations and code
// writes (see the note on the built-in code tools: PRs/commits go through an
// admin-registered MCP server, never a built-in). Registering a server today
// means an admin hand-enters a URL, transport, and auth. This catalog is a
// vetted, in-code list of well-known connectors that pre-fills that flow so an
// admin installs one in a couple of clicks instead of from scratch — the
// OneCamp answer to Claude Tag's connector catalog, but self-hosted and
// vendor-neutral (each entry points at an MCP server the admin runs/authorizes;
// no data flows anywhere until they configure it).
//
// The catalog is a static constant (like the tool registry): adding a connector
// is one struct literal. Endpoints are intentionally left blank — MCP servers
// are deployed/authorized by the operator, so the admin supplies the URL and
// secret; the entry supplies everything else plus guidance.

import (
	"context"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// Connector categories (stable, for grouping in the UI).
const (
	CategoryCode      = "Code"
	CategoryKnowledge = "Knowledge & docs"
	CategoryIssues    = "Issue tracking"
	CategoryMonitor   = "Monitoring"
	CategoryData      = "Data"
	CategoryMessaging = "Messaging"
)

// CatalogEntry is one vetted connector. Its fields map onto ServerInput so the
// FE can prefill the add-server dialog; presentation fields (Category, DocsURL,
// SecretHint, URLPlaceholder) guide the admin through the parts only they can
// provide (the deployed URL + the secret).
type CatalogEntry struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Category       string `json:"category"`
	DocsURL        string `json:"docs_url"`
	Transport      string `json:"transport"`
	AuthType       string `json:"auth_type"`
	AuthHeaderName string `json:"auth_header_name,omitempty"`
	SecretRequired bool   `json:"secret_required"`
	SecretHint     string `json:"secret_hint,omitempty"`
	URLPlaceholder string `json:"url_placeholder,omitempty"`
}

// CatalogEntryStatus is a catalog entry plus whether the workspace already has a
// matching MCP server registered (so the UI shows "Installed").
type CatalogEntryStatus struct {
	CatalogEntry
	Installed bool `json:"installed"`
}

// connectorCatalog is the curated set. Docs point at each connector's canonical
// MCP server so the admin can deploy/authorize it; the umbrella reference is the
// official modelcontextprotocol/servers repo. Endpoints stay blank on purpose.
var connectorCatalog = []CatalogEntry{
	{
		Slug:           "github",
		Name:           "GitHub",
		Description:    "Repository questions, issues, and pull requests — including opening a PR (the vendor-neutral path for code writes). Answers in-thread and drives the fix-to-PR loop.",
		Category:       CategoryCode,
		DocsURL:        "https://github.com/github/github-mcp-server",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A GitHub token (PAT or app token) with the repo scopes you want the agent to use.",
		URLPlaceholder: "https://your-github-mcp-server/mcp",
	},
	{
		Slug:           "notion",
		Name:           "Notion",
		Description:    "Look up policies, runbooks, and prior decisions in Notion and reply with the source; review documents against a checklist.",
		Category:       CategoryKnowledge,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A Notion integration token with access to the pages/databases you want the agent to read.",
		URLPlaceholder: "https://your-notion-mcp-server/mcp",
	},
	{
		Slug:           "linear",
		Name:           "Linear",
		Description:    "Turn threads into Linear issues, track project status, and chase stalled work.",
		Category:       CategoryIssues,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A Linear API key scoped to the team(s) the agent should manage.",
		URLPlaceholder: "https://your-linear-mcp-server/mcp",
	},
	{
		Slug:           "jira",
		Name:           "Jira",
		Description:    "Create and update Jira issues from discussions and report on project state.",
		Category:       CategoryIssues,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A Jira/Atlassian API token for the projects the agent should manage.",
		URLPlaceholder: "https://your-jira-mcp-server/mcp",
	},
	{
		Slug:           "sentry",
		Name:           "Sentry",
		Description:    "Investigate errors and monitor issues — check a project on a schedule and post a first-pass diagnosis before anyone asks.",
		Category:       CategoryMonitor,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A Sentry auth token with read access to the org/projects to monitor.",
		URLPlaceholder: "https://your-sentry-mcp-server/mcp",
	},
	{
		Slug:           "postgres",
		Name:           "PostgreSQL",
		Description:    "Answer data questions by querying a Postgres database (read-only recommended) and returning results the agent can chart.",
		Category:       CategoryData,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthNone,
		SecretRequired: false,
		URLPlaceholder: "https://your-postgres-mcp-server/mcp",
	},
	{
		Slug:           "google-drive",
		Name:           "Google Drive",
		Description:    "Find answers in Google Drive documents and review them against a checklist or policy.",
		Category:       CategoryKnowledge,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "An OAuth token for the Drive scope the agent should read.",
		URLPlaceholder: "https://your-gdrive-mcp-server/mcp",
	},
	{
		Slug:           "slack",
		Name:           "Slack",
		Description:    "Read and post in a connected Slack workspace — bridge conversations and cross-post updates.",
		Category:       CategoryMessaging,
		DocsURL:        "https://github.com/modelcontextprotocol/servers",
		Transport:      model.TransportHTTP,
		AuthType:       model.AuthBearer,
		SecretRequired: true,
		SecretHint:     "A Slack bot token for the workspace/channels the agent should reach.",
		URLPlaceholder: "https://your-slack-mcp-server/mcp",
	},
}

// Catalog returns the curated connectors, each flagged with whether the
// workspace already has a matching MCP server registered.
func Catalog(ctx context.Context) ([]CatalogEntryStatus, error) {
	servers, err := model.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	return mergeCatalogStatus(connectorCatalog, servers), nil
}

// mergeCatalogStatus marks each catalog entry installed when an existing server
// matches it by name (case-insensitive) — the name the install flow prefills —
// so the UI can show "Installed" and steer the admin to manage it instead of
// adding a duplicate. Pure and total.
func mergeCatalogStatus(entries []CatalogEntry, servers []*model.McpServer) []CatalogEntryStatus {
	installed := make(map[string]bool, len(servers))
	for _, s := range servers {
		if s == nil {
			continue
		}
		installed[strings.ToLower(strings.TrimSpace(s.Name))] = true
	}
	out := make([]CatalogEntryStatus, 0, len(entries))
	for _, e := range entries {
		out = append(out, CatalogEntryStatus{
			CatalogEntry: e,
			Installed:    installed[strings.ToLower(strings.TrimSpace(e.Name))],
		})
	}
	return out
}

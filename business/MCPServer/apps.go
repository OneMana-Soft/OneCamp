package business

// MCP Apps: OneCamp's own views inside the assistant that called it.
//
// Since January 2026 an MCP tool can name an interactive view, and Claude,
// ChatGPT and VS Code render it in the conversation instead of a wall of text
// (spec: github.com/modelcontextprotocol/ext-apps, revision 2026-01-26). A
// person who asks Claude "what's on my plate" now sees their tasks as tasks:
// due dates, overdue in red, one click to open each in OneCamp.
//
// The contract, all of it standard MCP plus three conventions:
//   - a tool lists `_meta.ui.resourceUri` pointing at a ui:// resource;
//   - resources/read returns that resource as text/html;profile=mcp-app;
//   - the host renders it sandboxed and pushes the tool result to it over a
//     postMessage JSON-RPC dialect (ui/initialize, ui/notifications/tool-result).
//
// A host without MCP Apps ignores `_meta` and shows the text result as before,
// so this changes nothing for any client that does not opt in.
//
// THE VIEWS FETCH NOTHING. They render only what the host hands them from the
// tool result, which the governed path already authorised, so a view can never
// show more than the text did. With no csp declared, the host applies the
// spec's default (connect-src 'none').

import (
	_ "embed"
	"encoding/json"
)

// AppMimeType is the only content type MCP Apps defines.
const AppMimeType = "text/html;profile=mcp-app"

//go:embed apps/tasks.html
var tasksViewHTML string

// AppView is one ui:// resource and the tools that render with it.
type AppView struct {
	URI         string
	Name        string
	Description string
	HTML        string
	Tools       []string
}

// AppViews are every view this server offers.
var AppViews = []AppView{{
	URI:         "ui://onecamp/tasks",
	Name:        "onecamp_tasks",
	Description: "A list of OneCamp tasks with status, due date and project, each linking to the task",
	HTML:        tasksViewHTML,
	Tools:       []string{"list_tasks", "list_project_tasks"},
}}

// ViewForTool returns the ui:// resource a tool renders with, or "".
func ViewForTool(tool string) string {
	for _, v := range AppViews {
		for _, t := range v.Tools {
			if t == tool {
				return v.URI
			}
		}
	}
	return ""
}

// ViewByURI finds a view by its ui:// URI.
func ViewByURI(uri string) (AppView, bool) {
	for _, v := range AppViews {
		if v.URI == uri {
			return v, true
		}
	}
	return AppView{}, false
}

// StructuredData decodes an executor's MetaStructuredJSON, or nil. A view
// renders this; the text result remains what the model reads.
func StructuredData(meta map[string]string, key string) any {
	raw, ok := meta[key]
	if !ok || raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	return v
}

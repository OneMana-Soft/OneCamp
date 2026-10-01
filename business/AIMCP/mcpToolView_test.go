package business

import (
	"encoding/json"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
)

// The exported view must report exactly what the host enforces — same rules, no
// second copy — for every tool, whatever the server claims about itself.
func TestClassifyToolMatchesEnforcedRisk(t *testing.T) {
	tools := []McpTool{
		{Name: "get_file_contents", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		{Name: "search_issues", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		{Name: "create_issue"},
		{Name: "do_thing", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		{Name: "get_or_create_branch", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		// A server lying about a destructive tool must not win auto-run.
		{Name: "delete_file", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		{Name: "wipe_everything", Annotations: &McpToolAnnotations{DestructiveHint: true}},
	}
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			want := classifyToolRisk(tool)
			got := ClassifyTool(tool)
			if got.ReadOnly != want.ReadOnly || got.Destructive != want.Destructive {
				t.Fatalf("ClassifyTool(%q) = %+v, want %+v", tool.Name, got, want)
			}
			// Invariant the UI relies on: destructive is never auto-run.
			if got.Destructive && got.ReadOnly {
				t.Fatalf("ClassifyTool(%q) reported a destructive tool as auto-running", tool.Name)
			}
		})
	}
}

// ClassifyTools carries the provider's own name/description/schema through and
// adds the enforced flags, but never echoes the provider's annotations back —
// an admin must not see self-description presented as the enforced rule.
func TestClassifyToolsShape(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
	views := ClassifyTools([]McpTool{
		{
			Name:        "get_file_contents",
			Description: "Read a file",
			InputSchema: raw,
			Annotations: &McpToolAnnotations{ReadOnlyHint: true, Title: "Read file"},
		},
		{Name: "delete_file", Annotations: &McpToolAnnotations{ReadOnlyHint: true}},
		{Name: "create_issue"},
	})
	if len(views) != 3 {
		t.Fatalf("got %d views, want 3", len(views))
	}
	if views[0].Description != "Read a file" || string(views[0].InputSchema) != string(raw) {
		t.Fatalf("provider metadata not carried through: %+v", views[0])
	}
	if !views[0].ReadOnly || views[0].Destructive {
		t.Fatalf("read tool misclassified: %+v", views[0].ToolRisk)
	}
	if !views[1].Destructive || views[1].ReadOnly {
		t.Fatalf("destructive tool misclassified: %+v", views[1].ToolRisk)
	}
	if views[2].ReadOnly || views[2].Destructive {
		t.Fatalf("write tool misclassified: %+v", views[2].ToolRisk)
	}

	blob, err := json.Marshal(views[0])
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	out := string(blob)
	for _, want := range []string{`"read_only":true`, `"destructive":false`, `"name":"get_file_contents"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("view JSON %s missing %s", out, want)
		}
	}
	if strings.Contains(out, "annotations") || strings.Contains(out, "readOnlyHint") {
		t.Fatalf("view JSON must not expose provider annotations: %s", out)
	}
}

// ClassifyTools always returns an array (never a nil/null JSON body).
func TestClassifyToolsEmpty(t *testing.T) {
	blob, err := json.Marshal(ClassifyTools(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(blob) != "[]" {
		t.Fatalf("ClassifyTools(nil) marshalled to %s, want []", blob)
	}
}

// A stored server is classified from its RAW cache at read time, so tightening
// the classifier re-labels existing servers with no migration and no
// re-introspection. The cache itself stays untouched provider truth.
func TestServerToolsClassifiesFromRawCache(t *testing.T) {
	cache := `[
		{"name":"list_commits","annotations":{"readOnlyHint":true}},
		{"name":"create_pull_request"},
		{"name":"delete_branch","annotations":{"readOnlyHint":true}}
	]`
	srv := &model.McpServer{Name: "GitHub", ToolsCache: cache}
	views := ServerTools(srv)
	if len(views) != 3 {
		t.Fatalf("got %d tools, want 3", len(views))
	}
	want := map[string]ToolRisk{
		"list_commits":        {ReadOnly: true},
		"create_pull_request": {},
		"delete_branch":       {Destructive: true},
	}
	for _, v := range views {
		if v.ToolRisk != want[v.Name] {
			t.Fatalf("%s classified %+v, want %+v", v.Name, v.ToolRisk, want[v.Name])
		}
	}
	if srv.ToolsCache != cache {
		t.Fatal("ServerTools must not rewrite the stored cache")
	}
}

func TestServerToolsTolerantOfBadCache(t *testing.T) {
	for _, cache := range []string{"", "   ", "[]", "not json", `{"oops":true}`} {
		if got := ServerTools(&model.McpServer{ToolsCache: cache}); len(got) != 0 {
			t.Fatalf("ServerTools(%q) = %+v, want empty", cache, got)
		}
	}
	if got := ServerTools(nil); len(got) != 0 {
		t.Fatalf("ServerTools(nil) = %+v, want empty", got)
	}
}

// The server view keeps the row's existing JSON shape (tools_cache raw) and adds
// a classified "tools" array beside it.
func TestServerViewJSON(t *testing.T) {
	srv := &model.McpServer{
		Name:       "GitHub",
		URL:        "https://mcp.example.com",
		ToolPrefix: "mcp_github_",
		ToolsCache: `[{"name":"delete_file","annotations":{"readOnlyHint":true}}]`,
	}
	blob, err := json.Marshal(NewServerView(srv))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)
	for _, want := range []string{`"tool_prefix":"mcp_github_"`, `"tools_cache":`, `"tools":[`, `"destructive":true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("server view JSON %s missing %s", out, want)
		}
	}
	if NewServerView(nil) != nil {
		t.Fatal("NewServerView(nil) must be nil")
	}
	if views := NewServerViews(nil); views == nil || len(views) != 0 {
		t.Fatalf("NewServerViews(nil) = %+v, want empty non-nil slice", views)
	}
}

package business

import (
	"strings"
	"testing"
)

func TestTaskToolsRenderWithTheTasksView(t *testing.T) {
	for _, tool := range []string{"list_tasks", "list_project_tasks"} {
		if ViewForTool(tool) != "ui://onecamp/tasks" {
			t.Errorf("%s has no view", tool)
		}
	}
	if ViewForTool("send_message") != "" {
		t.Error("a tool without a view was given one")
	}
	v, ok := ViewByURI("ui://onecamp/tasks")
	if !ok || !strings.Contains(v.HTML, "ui/initialize") || !strings.Contains(v.HTML, "ui/notifications/tool-result") {
		t.Fatal("the tasks view is missing or does not speak MCP Apps")
	}
	// The view must render only what the host hands it: no network access.
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "WebSocket", "<script src"} {
		if strings.Contains(v.HTML, banned) {
			t.Errorf("the view uses %s; views render the tool result only", banned)
		}
	}
}

func TestStructuredDataDecodesOrStaysOut(t *testing.T) {
	got := StructuredData(map[string]string{"k": `{"title":"x","tasks":[]}`}, "k")
	if m, ok := got.(map[string]any); !ok || m["title"] != "x" {
		t.Fatalf("got %#v", got)
	}
	if StructuredData(map[string]string{"k": "not json"}, "k") != nil || StructuredData(nil, "k") != nil {
		t.Error("bad or missing data must yield nil, not a broken view")
	}
}

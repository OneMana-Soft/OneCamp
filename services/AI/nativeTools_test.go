package ai

import (
	"strings"
	"testing"
)

func TestToolCallToAction(t *testing.T) {
	// Typical: string args pass through.
	a := ToolCallToAction(ToolCall{
		ID:        "call_1",
		Name:      "create_task",
		Arguments: `{"task_name":"Ship it","project_uuid":"p-123","priority":"high"}`,
	})
	if a.ToolName != "create_task" {
		t.Fatalf("tool name = %q", a.ToolName)
	}
	if a.Params["task_name"] != "Ship it" || a.Params["project_uuid"] != "p-123" || a.Params["priority"] != "high" {
		t.Fatalf("params not mapped: %+v", a.Params)
	}

	// Non-string values are coerced (numbers without trailing .0, booleans).
	a = ToolCallToAction(ToolCall{Name: "summarize_channel", Arguments: `{"count":50,"deep":true}`})
	if a.Params["count"] != "50" {
		t.Fatalf("number coercion = %q, want 50", a.Params["count"])
	}
	if a.Params["deep"] != "true" {
		t.Fatalf("bool coercion = %q, want true", a.Params["deep"])
	}

	// Malformed arguments must not panic and yield empty params (validation
	// then rejects the call).
	a = ToolCallToAction(ToolCall{Name: "x", Arguments: "{not json"})
	if len(a.Params) != 0 {
		t.Fatalf("malformed args should give empty params, got %+v", a.Params)
	}
}

func TestToolSpecsForRun(t *testing.T) {
	specs := ToolSpecsForRun([]string{"create_task", "list_tasks"}, "create a task")
	if len(specs) == 0 {
		t.Fatalf("expected specs")
	}
	var found *ToolSpec
	for i := range specs {
		if specs[i].Name == "create_task" {
			found = &specs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("create_task spec missing")
	}
	// Schema must be a JSON-Schema object with properties incl. required fields.
	if found.Parameters["type"] != "object" {
		t.Fatalf("schema type = %v", found.Parameters["type"])
	}
	props, ok := found.Parameters["properties"].(map[string]interface{})
	if !ok || props["task_name"] == nil {
		t.Fatalf("properties missing task_name: %+v", found.Parameters)
	}
	req, ok := found.Parameters["required"].([]string)
	if !ok || !containsStr(req, "task_name") {
		t.Fatalf("required missing task_name: %+v", found.Parameters["required"])
	}
	// Empty allow-list yields no specs.
	if s := ToolSpecsForRun(nil, "anything"); s != nil {
		t.Fatalf("expected nil for empty names, got %+v", s)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

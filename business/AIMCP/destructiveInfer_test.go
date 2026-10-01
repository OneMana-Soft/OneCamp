package business

import (
	"strings"
	"testing"
)

// inferDestructiveByName is the fallback that flags irreversible MCP writes by
// NAME when a server omits the destructiveHint annotation. It must catch common
// destructive verbs across camelCase/snake/kebab/dot naming, catch force-push
// style overwrites, and NOT false-positive on read/list tools or safe writes.
func TestInferDestructiveByName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Destructive across naming styles.
		{"delete_file", true},
		{"deleteFile", true},
		{"delete-branch", true},
		{"drop_table", true},
		{"removeMember", true},
		{"purge_cache", true},
		{"truncate_table", true},
		{"repo.destroy", true},
		{"force_push", true},
		{"forcePush", true},
		{"force_reset", true},

		// Not destructive: reads, safe writes, and near-miss tokens.
		{"get_file_contents", false},
		{"list_deleted_items", false}, // token "deleted", not "delete"
		{"create_issue", false},
		{"push_files", false}, // covered separately by the branch guard
		{"create_or_update_file", false},
		{"search_repositories", false},
		{"add_comment", false},
		{"undelete_record", false}, // token "undelete", not "delete"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferDestructiveByName(tc.name); got != tc.want {
				t.Fatalf("inferDestructiveByName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestInferDestructiveByNameEnvExtension(t *testing.T) {
	t.Setenv("AI_DESTRUCTIVE_TOOL_KEYWORDS", "archive, close")
	if !inferDestructiveByName("archive_project") {
		t.Fatal("expected custom keyword 'archive' to be treated as destructive")
	}
	if !inferDestructiveByName("closeAccount") {
		t.Fatal("expected custom keyword 'close' to be treated as destructive")
	}
	// Built-ins still apply alongside the custom set.
	if !inferDestructiveByName("delete_file") {
		t.Fatal("built-in verbs must remain destructive when env extends the set")
	}
}

// classifyToolRisk resolves risk host-side: a provider annotation may only
// RAISE risk. A tool may auto-run (ReadOnly) only when the server claims
// read-only AND the name is credibly read-shaped with no mutating token;
// everything else — including unknown/ambiguous names — fails closed to the
// approval gate, and destructive evidence (hint or name) always wins.
func TestClassifyToolRisk(t *testing.T) {
	readOnly := &McpToolAnnotations{ReadOnlyHint: true}
	destructive := &McpToolAnnotations{DestructiveHint: true}
	readOnlyAndDestructive := &McpToolAnnotations{ReadOnlyHint: true, DestructiveHint: true}

	cases := []struct {
		label           string
		tool            McpTool
		wantReadOnly    bool
		wantDestructive bool
	}{
		// Honest reads may auto-run, across every naming style.
		{"snake read", McpTool{Name: "get_file_contents", Annotations: readOnly}, true, false},
		{"camel read", McpTool{Name: "listCommits", Annotations: readOnly}, true, false},
		{"kebab read", McpTool{Name: "search-repositories", Annotations: readOnly}, true, false},
		{"dot read", McpTool{Name: "repos.list", Annotations: readOnly}, true, false},
		{"read with near-miss token", McpTool{Name: "list_deleted_items", Annotations: readOnly}, true, false},

		// Dishonest readOnlyHint on a destructive tool: destructive wins.
		{"dishonest hint on delete_file", McpTool{Name: "delete_file", Annotations: readOnly}, false, true},
		{"dishonest hint on camel delete", McpTool{Name: "deleteRepository", Annotations: readOnly}, false, true},
		{"dishonest hint on dot destroy", McpTool{Name: "repo.destroy", Annotations: readOnly}, false, true},
		{"dishonest hint on force push", McpTool{Name: "force_push", Annotations: readOnly}, false, true},
		{"both hints set", McpTool{Name: "get_thing", Annotations: readOnlyAndDestructive}, false, true},

		// Dishonest readOnlyHint on a non-destructive mutation: write, approval.
		{"dishonest hint on push_files", McpTool{Name: "push_files", Annotations: readOnly}, false, false},
		{"compound get_or_create", McpTool{Name: "get_or_create", Annotations: readOnly}, false, false},
		{"compound listAndUpdate", McpTool{Name: "listAndUpdate", Annotations: readOnly}, false, false},
		{"compound search.and.replace", McpTool{Name: "search.and.replace", Annotations: readOnly}, false, false},
		{"read verb with exec token", McpTool{Name: "execute_query", Annotations: readOnly}, false, false},
		{"create_or_update_file", McpTool{Name: "create_or_update_file", Annotations: readOnly}, false, false},

		// Unknown / ambiguous names fail closed even with the read-only claim.
		{"unknown verb", McpTool{Name: "do_thing", Annotations: readOnly}, false, false},
		{"opaque name", McpTool{Name: "foo.bar", Annotations: readOnly}, false, false},
		{"empty name", McpTool{Name: "", Annotations: readOnly}, false, false},

		// Provider-declared write stays a write even with a read-shaped name.
		{"no annotations at all", McpTool{Name: "get_file_contents"}, false, false},
		{"explicit write hint", McpTool{Name: "list_issues", Annotations: &McpToolAnnotations{}}, false, false},
		{"declared destructive read-shaped name", McpTool{Name: "get_snapshot", Annotations: destructive}, false, true},

		// Plain writes with no annotations behave as before.
		{"create_issue", McpTool{Name: "create_issue"}, false, false},
		{"delete_file no hints", McpTool{Name: "delete_file"}, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got := classifyToolRisk(tc.tool)
			if got.ReadOnly != tc.wantReadOnly || got.Destructive != tc.wantDestructive {
				t.Fatalf("classifyToolRisk(%q) = {ReadOnly:%v Destructive:%v}, want {ReadOnly:%v Destructive:%v}",
					tc.tool.Name, got.ReadOnly, got.Destructive, tc.wantReadOnly, tc.wantDestructive)
			}
			// Invariant: a destructive tool is never auto-runnable.
			if got.Destructive && got.ReadOnly {
				t.Fatalf("classifyToolRisk(%q) marked a destructive tool read-only", tc.tool.Name)
			}
		})
	}
}

// The env extension must also deny auto-run, not only raise destructiveness:
// an operator adding a custom mutation keyword covers dishonest hints too.
func TestClassifyToolRiskEnvExtension(t *testing.T) {
	t.Setenv("AI_DESTRUCTIVE_TOOL_KEYWORDS", "archive, wipeout")
	risk := classifyToolRisk(McpTool{Name: "get_archive_status", Annotations: &McpToolAnnotations{ReadOnlyHint: true}})
	if risk.ReadOnly || !risk.Destructive {
		t.Fatalf("custom keyword must force a destructive write, got %+v", risk)
	}
	// Read-shaped names unrelated to the custom keywords still auto-run.
	safe := classifyToolRisk(McpTool{Name: "list_commits", Annotations: &McpToolAnnotations{ReadOnlyHint: true}})
	if !safe.ReadOnly || safe.Destructive {
		t.Fatalf("list_commits should remain an auto-runnable read, got %+v", safe)
	}
}

// mcpToolDescription must keep warning the model about a destructive tool, and
// stay quiet for reads/plain writes.
func TestMcpToolDescriptionDestructiveWarning(t *testing.T) {
	tool := McpTool{Name: "delete_file", Description: "Delete a file"}
	risk := classifyToolRisk(tool)
	desc := mcpToolDescription("GitHub", tool, risk.Destructive)
	if !strings.Contains(desc, "[GitHub]") {
		t.Fatalf("description should name the source server: %q", desc)
	}
	if !strings.Contains(desc, "destructive") {
		t.Fatalf("destructive tools must carry the warning: %q", desc)
	}
	readTool := McpTool{Name: "get_file_contents", Description: "Read a file", Annotations: &McpToolAnnotations{ReadOnlyHint: true}}
	readDesc := mcpToolDescription("GitHub", readTool, classifyToolRisk(readTool).Destructive)
	if strings.Contains(readDesc, "destructive") {
		t.Fatalf("a read must not be described as destructive: %q", readDesc)
	}
}

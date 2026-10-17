package ai

import (
	"context"
	"errors"
	"testing"
)

// Every tool from outside the workspace is refused where it runs when the
// rule says so, whichever path reached it (the runner, a knowledge read, the
// served MCP surface), and nothing is sent.
func TestAnOutsideToolIsRefusedWhereItRuns(t *testing.T) {
	refusal := errors.New("not on the demo")
	oldGuard := outsideReachGuard
	t.Cleanup(func() { outsideReachGuard = oldGuard; SetDynamicTools(nil, nil) })

	called := 0
	SetDynamicTools([]ToolDef{{Name: "mcp_github_push_files"}}, map[string]ToolExecutor{
		"mcp_github_push_files": func(context.Context, ProposedAction, string) (string, map[string]string, error) {
			called++
			return "pushed", nil, nil
		},
	})
	if !IsOutsideTool("mcp_github_push_files") || IsOutsideTool("send_message") {
		t.Fatal("an MCP tool is outside; a OneCamp tool is not")
	}
	exec, ok := GetExecutor("mcp_github_push_files")
	if !ok {
		t.Fatal("no executor")
	}

	RegisterOutsideReachGuard(func(_ context.Context, acting string) error {
		if acting == "visitor" {
			return refusal
		}
		return nil
	})
	if _, _, err := exec(context.Background(), ProposedAction{ToolName: "mcp_github_push_files"}, "visitor"); err != refusal {
		t.Fatalf("got %v; want the refusal", err)
	}
	if called != 0 {
		t.Fatal("the outside server was called")
	}
	if out, _, err := exec(context.Background(), ProposedAction{ToolName: "mcp_github_push_files"}, "admin"); err != nil || out != "pushed" || called != 1 {
		t.Fatalf("allowed run: %q %v (called %d)", out, err, called)
	}
	if OutsideReachRefused(context.Background(), "visitor") != refusal {
		t.Error("OutsideReachRefused must give the rule's answer")
	}
	RegisterOutsideReachGuard(nil)
	if OutsideReachRefused(context.Background(), "visitor") != nil {
		t.Error("no rule installed allows")
	}
}

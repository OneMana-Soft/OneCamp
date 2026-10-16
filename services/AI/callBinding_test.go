package ai

import (
	"context"
	"testing"
)

// An approved call is held to where its tools sent it when it was proposed, so
// the fingerprint has to change with exactly that: the server behind a tool and
// the server's address. Nothing else may move it, or a plan would be refused for
// listing its steps in another order.
func TestCallBindingFollowsWhereAToolSendsItsCall(t *testing.T) {
	t.Cleanup(func() { SetDynamicTools(nil, nil) })
	noop := func(context.Context, ProposedAction, string) (string, map[string]string, error) { return "", nil, nil }
	register := func(destination string) {
		SetDynamicTools(
			[]ToolDef{{Name: "tracker_create_issue", Destination: destination}, {Name: "tracker_close_issue", Destination: destination}},
			map[string]ToolExecutor{"tracker_create_issue": noop, "tracker_close_issue": noop})
	}
	const own = "call_binding_test_own_tool"
	Executors[own] = noop
	t.Cleanup(func() { delete(Executors, own) })

	register("server-1\x00https://tracker.example/mcp")
	if got := CallBinding(own); got != "" {
		t.Fatalf("OneCamp's own tools send nothing elsewhere, so they bind nothing; got %q", got)
	}
	first := CallBinding("tracker_create_issue")
	if first == "" {
		t.Fatal("a tool from an MCP server must be bound to where it sends its call")
	}
	if got := CallBinding(own, "tracker_create_issue", "tracker_create_issue"); got != first {
		t.Error("an own tool beside it, or the same tool twice, must not change the binding")
	}
	if CallBinding("tracker_close_issue") == first {
		t.Error("another tool is another binding")
	}
	if CallBinding("tracker_create_issue", "tracker_close_issue") != CallBinding("tracker_close_issue", "tracker_create_issue") {
		t.Error("a plan's steps bind the same in any order")
	}

	register("server-1\x00https://elsewhere.example/mcp")
	if CallBinding("tracker_create_issue") == first {
		t.Error("re-pointing the tool's server must change the binding")
	}
	register("server-2\x00https://tracker.example/mcp")
	if CallBinding("tracker_create_issue") == first {
		t.Error("the same name under another server must change the binding")
	}
	SetDynamicTools(nil, nil)
	if gone := CallBinding("tracker_create_issue"); gone == "" || gone == first {
		t.Errorf("a tool that is gone still binds, to nothing it could reach; got %q", gone)
	}
}

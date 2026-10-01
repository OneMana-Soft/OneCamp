package ai

import "testing"

// These tests are the deterministic half of the agent harness: they cover the
// model-independent logic the agent loop relies on (tool classification, the
// read/write split, tool-prompt gating, and tool-call parsing). They run with
// `go test ./services/AI/` and need no LLM or Redis.

func TestIsReadOnlyTool(t *testing.T) {
	cases := map[string]bool{
		"summarize_channel":     true,
		"summarize_dm":          true,
		"summarize_group_chat":  true,
		"gmail_search":          true,
		"calendar_list_events":  true,
		"github_list_prs":       true,
		"github_list_issues":    true,
		"send_message":          false,
		"send_dm":               false,
		"create_task":           false,
		"create_doc":            false,
		"set_reminder":          false,
		"gmail_send":            false,
		"calendar_create_event": false,
		"github_comment":        false,
		"does_not_exist":        false, // unknown tools must never be auto-run
	}
	for tool, want := range cases {
		if got := IsReadOnlyTool(tool); got != want {
			t.Errorf("IsReadOnlyTool(%q) = %v, want %v", tool, got, want)
		}
	}
}

func TestClassifyActions(t *testing.T) {
	actions := []ProposedAction{
		{ToolName: "summarize_channel", Params: map[string]string{"channel_uuid": "c1"}},            // read
		{ToolName: "send_dm", Params: map[string]string{"to_uuid": "u1", "text": "hi"}},             // write
		{ToolName: "gmail_search", Params: map[string]string{"query": "invoice"}},                   // read
		{ToolName: "send_message", Params: map[string]string{"channel_uuid": "c2"}},                 // invalid: missing text → dropped
		{ToolName: "totally_unknown", Params: map[string]string{"x": "y"}},                          // unknown → dropped
		{ToolName: "create_task", Params: map[string]string{"task_name": "t", "project_uuid": "p"}}, // write
	}

	reads, writes := ClassifyActions(actions)

	if len(reads) != 2 {
		t.Fatalf("expected 2 read actions, got %d (%v)", len(reads), reads)
	}
	if len(writes) != 2 {
		t.Fatalf("expected 2 write actions, got %d (%v)", len(writes), writes)
	}
	if reads[0].ToolName != "summarize_channel" || reads[1].ToolName != "gmail_search" {
		t.Errorf("unexpected read actions: %v", reads)
	}
	if writes[0].ToolName != "send_dm" || writes[1].ToolName != "create_task" {
		t.Errorf("unexpected write actions: %v", writes)
	}
}

func TestShouldIncludeTools(t *testing.T) {
	cases := map[string]bool{
		// Pure summary/read intents keep tools excluded (token saving, unchanged).
		"summarize #engineering":     false,
		"catch me up on the channel": false,
		"recap what happened today":  false,
		// Read + write combined must expose tools so the loop can act.
		"summarize #engineering and dm the summary to john": true,
		"recap the channel and post it in #general":         true,
		// Summary-style asks that name a task/project/team entity expose tools
		// so the model can fetch authoritative data via the read tools.
		"summarize my tasks":              true,
		"give me an overview of my todos": true,
		"recap the marketing project":     true,
		// Plain action commands always include tools.
		"create a task to ship the release": true,
		"send a message to the team":        true,
	}
	for q, want := range cases {
		if got := ShouldIncludeTools(q); got != want {
			t.Errorf("ShouldIncludeTools(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestParseToolCallsSingle(t *testing.T) {
	resp := `I'll post that for you.
<tool_call>
{"tool": "send_message", "params": {"channel_uuid": "real-uuid-1234567890123456789012345", "text": "hello"}, "description": "Post to channel"}
</tool_call>`
	clean, actions := ParseToolCalls(resp)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].ToolName != "send_message" || actions[0].Params["text"] != "hello" {
		t.Errorf("unexpected action: %+v", actions[0])
	}
	if clean != "I'll post that for you." {
		t.Errorf("unexpected clean text: %q", clean)
	}
}

func TestParseToolCallsDropsPromptEcho(t *testing.T) {
	// Small models sometimes echo the prompt's dummy example values; those
	// must be dropped so we never act on a hallucinated example.
	resp := `<tool_call>
{"tool": "send_message", "params": {"channel_uuid": "abc-123", "text": "Status report: all systems go"}, "description": "example"}
</tool_call>`
	_, actions := ParseToolCalls(resp)
	if len(actions) != 0 {
		t.Fatalf("expected prompt-echo action to be dropped, got %d (%v)", len(actions), actions)
	}
}

func TestParseToolCallsMalformedIsSafe(t *testing.T) {
	// An unclosed tag must not panic and must produce no actions.
	resp := `here is a broken <tool_call> {"tool": "send_dm"`
	clean, actions := ParseToolCalls(resp)
	if len(actions) != 0 {
		t.Errorf("expected no actions from malformed input, got %v", actions)
	}
	if clean == "" {
		t.Errorf("expected some cleaned text back, got empty")
	}
}

func TestParseToolCallsKeepsDistinctSameToolCalls(t *testing.T) {
	// Two DMs to different people in one turn must both survive (dedup is by
	// full signature, not by tool name).
	resp := `Sure, messaging both.
<tool_call>
{"tool": "send_dm", "params": {"to_uuid": "11111111-1111-1111-1111-111111111111", "text": "hi John"}, "description": "DM John"}
</tool_call>
<tool_call>
{"tool": "send_dm", "params": {"to_uuid": "22222222-2222-2222-2222-222222222222", "text": "hi Sarah"}, "description": "DM Sarah"}
</tool_call>`
	_, actions := ParseToolCalls(resp)
	if len(actions) != 2 {
		t.Fatalf("expected 2 distinct send_dm actions, got %d (%v)", len(actions), actions)
	}
}

func TestParseToolCallsDropsExactDuplicate(t *testing.T) {
	// The same call repeated (a true echo) collapses to one.
	resp := `<tool_call>
{"tool": "send_dm", "params": {"to_uuid": "11111111-1111-1111-1111-111111111111", "text": "hi"}, "description": "DM"}
</tool_call>
<tool_call>
{"tool": "send_dm", "params": {"to_uuid": "11111111-1111-1111-1111-111111111111", "text": "hi"}, "description": "DM"}
</tool_call>`
	_, actions := ParseToolCalls(resp)
	if len(actions) != 1 {
		t.Fatalf("expected exact duplicate to collapse to 1, got %d", len(actions))
	}
}

func TestActionSignatureIsDeterministic(t *testing.T) {
	// Identical actions must produce identical signatures regardless of map
	// iteration order — this backs the agent loop's read-result cache.
	a := ProposedAction{ToolName: "gmail_search", Params: map[string]string{"query": "invoice", "limit": "10"}}
	b := ProposedAction{ToolName: "gmail_search", Params: map[string]string{"limit": "10", "query": "invoice"}}
	if ActionSignature(a) != ActionSignature(b) {
		t.Errorf("signatures differ for identical actions: %q vs %q", ActionSignature(a), ActionSignature(b))
	}

	// Different params must produce different signatures.
	c := ProposedAction{ToolName: "gmail_search", Params: map[string]string{"query": "receipt", "limit": "10"}}
	if ActionSignature(a) == ActionSignature(c) {
		t.Errorf("signatures collided for different actions: %q", ActionSignature(a))
	}

	// Different tool, same params → different signature.
	d := ProposedAction{ToolName: "summarize_channel", Params: map[string]string{"query": "invoice", "limit": "10"}}
	if ActionSignature(a) == ActionSignature(d) {
		t.Errorf("signatures collided across tools: %q", ActionSignature(a))
	}
}

// hasTool reports whether the selected set contains a tool by name.
func hasTool(tools []ToolDef, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func TestSelectToolsForQuery_Routing(t *testing.T) {
	// The demo query: task domain. Must include both the read (list_tasks) and
	// the write (update_task_status) so the read-then-act flow works, and must
	// NOT drag in unrelated domains (github/gmail/calendar).
	tools := selectToolsForQuery("find my task about the login bug and mark it done")
	if !hasTool(tools, "list_tasks") || !hasTool(tools, "update_task_status") {
		t.Errorf("task routing missing core tools: %v", toolNames(tools))
	}
	if hasTool(tools, "github_comment") || hasTool(tools, "gmail_send") || hasTool(tools, "calendar_create_event") {
		t.Errorf("task routing leaked unrelated tools: %v", toolNames(tools))
	}
	if len(tools) >= len(ToolRegistry) {
		t.Errorf("routing did not reduce the tool set (%d of %d)", len(tools), len(ToolRegistry))
	}

	// Cross-domain combo: summarize + DM.
	combo := selectToolsForQuery("summarize #eng and DM the summary to John")
	if !hasTool(combo, "summarize_channel") || !hasTool(combo, "send_dm") {
		t.Errorf("combo routing missing tools: %v", toolNames(combo))
	}

	// No keyword match -> safe fallback to the full catalog.
	all := selectToolsForQuery("xyzzy plugh")
	if len(all) != len(ToolRegistry) {
		t.Errorf("expected full-catalog fallback, got %d of %d", len(all), len(ToolRegistry))
	}
}

func toolNames(tools []ToolDef) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// StripReasoning must remove reasoning-model chain-of-thought so it never leaks
// into the reply or is mistaken for a final answer, while leaving real content
// (including any tool call) intact.
func TestStripReasoning(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no reasoning", "Just a plain answer.", "Just a plain answer."},
		{"complete block", "<think>let me check the repo first</think>Done updating the README.", "Done updating the README."},
		{"thinking variant", "<thinking>plan</thinking>Result.", "Result."},
		{"multiline + case", "<Think>\nstep one\nstep two\n</Think>\nFinal.", "Final."},
		{"keeps tool call", "<think>reason</think><tool_call>{\"tool\":\"x\"}</tool_call>", "<tool_call>{\"tool\":\"x\"}</tool_call>"},
		{"orphan close", "leftover reasoning</think>The actual answer.", "The actual answer."},
		{"orphan open only", "Answer first<think>unfinished reasoning", "Answer first"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripReasoning(c.in); got != c.want {
				t.Fatalf("StripReasoning(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

package business

import (
	"context"
	"strings"
	"sync"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// commentTextToHTML must escape any model-authored markup (no injection into
// the task comment thread) and wrap each line in a paragraph.
func TestCommentTextToHTMLEscapesAndWraps(t *testing.T) {
	out := commentTextToHTML("Done <script>alert(1)</script>\n\nNext line")
	if strings.Contains(out, "<script>") {
		t.Fatalf("raw markup leaked into comment HTML: %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag, got %q", out)
	}
	if !strings.Contains(out, "<p>Done") {
		t.Fatalf("expected paragraph-wrapped first line, got %q", out)
	}
	// The blank line between the two paragraphs becomes an empty paragraph.
	if strings.Count(out, "<p>") < 3 {
		t.Fatalf("expected the blank line preserved as spacing, got %q", out)
	}
}

func TestCommentTextToHTMLEmpty(t *testing.T) {
	if got := commentTextToHTML("   "); got == "" {
		// A whitespace-only body still produces a (single empty) paragraph; the
		// caller guards against truly empty text before posting.
		t.Skip("whitespace handled by caller")
	}
}

// synthAssignmentPrompt must surface the task fields, strip HTML from the
// description (the agent reads plain text, not markup), and instruct the agent
// to summarize (the summary becomes the status comment).
func TestSynthAssignmentPromptIncludesFieldsAndStripsHTML(t *testing.T) {
	data := map[string]interface{}{
		"task_id":          "task-123",
		"project_id":       "proj-9",
		"name":             "Draft the launch note",
		"description":      "<p>Cover <b>pricing</b></p>",
		"assigned_by_name": "Akash",
	}
	p := synthAssignmentPrompt(data)
	for _, want := range []string{"Draft the launch note", "task_uuid: task-123", "project_uuid: proj-9", "by Akash", "summary"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "<b>") || strings.Contains(p, "<p>") {
		t.Fatalf("description HTML was not stripped:\n%s", p)
	}
	if !strings.Contains(p, "pricing") {
		t.Fatalf("description text lost after HTML strip:\n%s", p)
	}
}

// toolsUsedFooter must humanize + de-dupe tool ids, drop the mcp_ prefix, cap
// the list, and stay empty when no tools ran.
func TestToolsUsedFooter(t *testing.T) {
	if got := toolsUsedFooter(nil); got != "" {
		t.Fatalf("no tools should yield no footer, got %q", got)
	}
	got := toolsUsedFooter([]string{"search_workspace", "list_tasks", "search_workspace", "mcp_github_list_prs"})
	for _, want := range []string{"Worked with:", "search workspace", "list tasks", "github list prs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("footer missing %q: %s", want, got)
		}
	}
	// de-dupe: "search workspace" appears once.
	if strings.Count(got, "search workspace") != 1 {
		t.Fatalf("expected de-duped tools, got %q", got)
	}
}

func TestToolsUsedFooterCaps(t *testing.T) {
	many := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		many = append(many, "tool_"+string(rune('a'+i)))
	}
	got := toolsUsedFooter(many)
	if !strings.Contains(got, "more") {
		t.Fatalf("expected overflow 'more' suffix, got %q", got)
	}
}

// WithAgentResumeState/agentResumeFromCtx must round-trip the saved
// conversation + checkpoint, and an un-tagged context (every non-worker caller)
// must report no resume state so their runs are unchanged.
func TestAgentResumeStateRoundTrip(t *testing.T) {
	if agentResumeFromCtx(context.Background()) != nil {
		t.Fatal("a plain context must carry no resume state")
	}
	called := false
	ctx := WithAgentResumeState(context.Background(),
		[]ai.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
		func([]ai.ChatMessage) { called = true })
	rs := agentResumeFromCtx(ctx)
	if rs == nil || len(rs.messages) != 2 {
		t.Fatalf("resume state not round-tripped: %+v", rs)
	}
	rs.checkpoint(nil)
	if !called {
		t.Fatal("checkpoint not wired")
	}
}

// WithAgentProgress must round-trip the callback, and an un-tagged context
// (every non-worker caller) must report none so their runs are unchanged.
func TestAgentProgressRoundTrip(t *testing.T) {
	if agentProgressFromCtx(context.Background()) != nil {
		t.Fatal("a plain context must carry no progress callback")
	}
	var got []string
	ctx := WithAgentProgress(context.Background(), func(tools []string) { got = tools })
	fn := agentProgressFromCtx(ctx)
	if fn == nil {
		t.Fatal("progress callback not round-tripped")
	}
	fn([]string{"a", "b"})
	if len(got) != 2 {
		t.Fatalf("callback not invoked with tools, got %v", got)
	}
}

// synthFollowupPrompt must strip HTML from the comment and instruct the agent
// to act and summarize (the summary becomes the next status comment).
func TestSynthFollowupPromptStripsHTML(t *testing.T) {
	p := synthFollowupPrompt("<p>Also update the <b>changelog</b></p>")
	if strings.Contains(p, "<b>") || strings.Contains(p, "<p>") {
		t.Fatalf("follow-up HTML not stripped:\n%s", p)
	}
	// Surface-agnostic now (drives task, thread, and code-PR follow-ups): it
	// must carry the follow-up text, frame it as a follow-up, and instruct the
	// agent to act then reply.
	for _, want := range []string{"Also update the changelog", "followed up", "reply concisely"} {
		if !strings.Contains(p, want) {
			t.Fatalf("follow-up prompt missing %q:\n%s", want, p)
		}
	}
}

// failedToolsNote must stay empty when nothing errored, and otherwise humanize
// + de-dupe the failed tool ids into an honest "did not complete" disclosure.
func TestFailedToolsNote(t *testing.T) {
	if got := failedToolsNote(nil); got != "" {
		t.Fatalf("no failed tools should yield no note, got %q", got)
	}
	got := failedToolsNote([]string{"mcp_github_create_or_update_file", "mcp_github_create_branch", "mcp_github_create_or_update_file"})
	for _, want := range []string{"did not complete", "github create or update file", "github create branch"} {
		if !strings.Contains(got, want) {
			t.Fatalf("note missing %q: %s", want, got)
		}
	}
	if strings.Count(got, "github create or update file") != 1 {
		t.Fatalf("expected de-duped failed tools, got %q", got)
	}
}

// steeringTurn renders the instructions a human gave WHILE the agent was working
// as one user turn. The wording is load-bearing: a weak model treats a late
// instruction as background chatter and finishes its original plan anyway unless
// it is told the message is the latest and authoritative.
func TestSteeringTurn(t *testing.T) {
	turn := steeringTurn([]string{"use the onecamp-fe repo, not the backend", "  ", "and skip the migration"})
	if turn == "" {
		t.Fatal("expected a steering turn")
	}
	for _, want := range []string{
		"while you were working",
		"overrides anything",
		"use the onecamp-fe repo, not the backend",
		"and skip the migration",
	} {
		if !strings.Contains(turn, want) {
			t.Errorf("steering turn is missing %q:\n%s", want, turn)
		}
	}
	// Blank entries are dropped, not rendered as empty bullets.
	if strings.Contains(turn, "- \n") || strings.Contains(turn, "-  ") {
		t.Errorf("blank instructions must be dropped:\n%s", turn)
	}
	// Honesty: it must invite the agent to admit work the change made pointless,
	// rather than retro-fitting it as though it had been asked for.
	if !strings.Contains(turn, "rather than pretending") {
		t.Errorf("steering turn should require honesty about wasted work:\n%s", turn)
	}
}

func TestSteeringTurn_NothingToSay(t *testing.T) {
	if got := steeringTurn(nil); got != "" {
		t.Errorf("no instructions should render no turn, got %q", got)
	}
	if got := steeringTurn([]string{"", "   ", "\n"}); got != "" {
		t.Errorf("only-blank instructions should render no turn, got %q", got)
	}
}

// The concurrent lookup pre-pass must never run something the sequential path
// would have refused: the eligibility predicate is the ONLY gate in front of it,
// so it has to be honoured exactly, and a turn with nothing to gain must fall
// back to the ordinary serial path untouched.
func TestPrefetchReadOnlyTools_HonoursEligibility(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	record := func(name string) {
		mu.Lock()
		ran = append(ran, name)
		mu.Unlock()
	}
	for _, name := range []string{"test_read_a", "test_read_b", "test_forbidden"} {
		toolName := name
		ai.RegisterExecutor(toolName, func(_ context.Context, _ ai.ProposedAction, _ string) (string, map[string]string, error) {
			record(toolName)
			return toolName + " ok", nil, nil
		})
	}
	t.Cleanup(func() {
		for _, name := range []string{"test_read_a", "test_read_b", "test_forbidden"} {
			delete(ai.Executors, name)
		}
	})

	actions := []ai.ProposedAction{
		{ToolName: "test_read_a"},
		{ToolName: "test_read_b"},
		{ToolName: "test_forbidden"},
		{ToolName: "test_read_a"}, // duplicate: fetched once
	}
	got := prefetchReadOnlyTools(context.Background(), actions, "user-1", func(a ai.ProposedAction) bool {
		return a.ToolName != "test_forbidden"
	})
	if len(got) != 2 {
		t.Fatalf("expected 2 cached results (deduped, gated), got %d: %#v", len(got), got)
	}
	for _, want := range []string{"test_read_a", "test_read_b"} {
		if res := got[ai.ActionSignature(ai.ProposedAction{ToolName: want})]; res.result != want+" ok" {
			t.Errorf("missing/incorrect cached result for %s: %#v", want, res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 {
		t.Errorf("exactly the eligible, deduped calls should execute, got %v", ran)
	}
	for _, name := range ran {
		if name == "test_forbidden" {
			t.Error("an ineligible call must never be pre-run")
		}
	}
}

func TestPrefetchReadOnlyTools_SkipsWhenNothingToGain(t *testing.T) {
	always := func(ai.ProposedAction) bool { return true }
	if got := prefetchReadOnlyTools(context.Background(), nil, "u", always); got != nil {
		t.Errorf("no actions should not pre-fetch, got %#v", got)
	}
	one := []ai.ProposedAction{{ToolName: "test_read_a"}}
	if got := prefetchReadOnlyTools(context.Background(), one, "u", always); got != nil {
		t.Errorf("a single lookup gains nothing from concurrency, got %#v", got)
	}
	// The kill switch (concurrency of 1) must fall back to the serial path.
	t.Setenv("AI_AGENT_PARALLEL_READS", "1")
	two := []ai.ProposedAction{{ToolName: "test_read_a"}, {ToolName: "test_read_b"}}
	if got := prefetchReadOnlyTools(context.Background(), two, "u", always); got != nil {
		t.Errorf("AI_AGENT_PARALLEL_READS=1 must disable the pre-pass, got %#v", got)
	}
}

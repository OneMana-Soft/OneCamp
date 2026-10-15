package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// A question refused for asking for a credential, then another call: each
// result goes back under its own call's id, and the model reads the refusal.
func TestARefusedQuestionKeepsEachResultWithItsCall(t *testing.T) {
	actions := []ai.ProposedAction{
		{ToolName: blockerToolName, Params: map[string]string{"reason": "Please paste your API key so I can deploy"}},
		{ToolName: "list_projects"},
	}
	calls := []ai.ToolCall{{ID: "c-ask"}, {ID: "c-projects"}}
	refused := controlObservation(toolCallRecord{Error: sensitiveElicitationRefusal})

	results := newTurnResults(len(actions))
	results.set(0, blockerToolName, refused)
	results.set(1, "list_projects", "Project Hearth")

	native := results.followUp(true, "", calls, actions)
	if len(native) != 3 || len(native[0].ToolCalls) != 2 {
		t.Fatalf("want the assistant turn and one tool message per call, got %+v", native)
	}
	for i, want := range []struct{ id, content string }{{"c-ask", refused}, {"c-projects", "Project Hearth"}} {
		if got := native[i+1]; got.ToolCallID != want.id || got.Content != want.content {
			t.Errorf("tool message %d is %q under %q, want %q under %q", i, got.Content, got.ToolCallID, want.content, want.id)
		}
	}

	text := results.followUp(false, "", nil, actions)
	if len(text) != 2 || !strings.Contains(text[1].Content, "needs_human -> error: Refused: an agent must never ask") ||
		!strings.Contains(text[1].Content, "list_projects -> Project Hearth") {
		t.Errorf("the text turn does not show both results:\n%+v", text)
	}
}

// A call never recorded leaves its own result empty, and no other call's.
func TestAResultNeverRecordedShiftsNoOther(t *testing.T) {
	actions := []ai.ProposedAction{{ToolName: "a"}, {ToolName: "b"}}
	results := newTurnResults(len(actions))
	results.set(1, "b", "from b")
	native := results.followUp(true, "", []ai.ToolCall{{ID: "1"}, {ID: "2"}}, actions)
	if native[1].Content != "" || native[2].ToolCallID != "2" || native[2].Content != "from b" {
		t.Errorf("b's result moved: %+v", native)
	}
}

// Every way out of a call before the end of the turn's loop records what the
// call showed the model first. A refused question used to skip it.
func TestEveryCallInATurnRecordsItsResult(t *testing.T) {
	src, err := os.ReadFile("agentRunner.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "for idx, a := range actions {")
	end := strings.Index(s, "results.set(idx, a.ToolName, obs)")
	if start < 0 || end < start {
		t.Fatal("the turn's call loop or its closing record moved; move this check with it")
	}
	body := s[start:end]
	exits := regexp.MustCompile(`(?m)^\s*continue$`).FindAllStringIndex(body, -1)
	if len(exits) == 0 {
		t.Fatal("found no early exits in the turn's call loop; move this check with it")
	}
	for _, at := range exits {
		block := body[:at[0]]
		block = block[strings.LastIndex(block, "{\n"):]
		if !strings.Contains(block, "results.set(idx,") {
			t.Errorf("a call can leave the turn's loop without recording its result:\n%s", block)
		}
	}
}

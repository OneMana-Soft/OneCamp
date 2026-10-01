package business

import (
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

func TestChartCapabilityPrompt(t *testing.T) {
	p := ChartCapabilityPrompt()
	for _, must := range []string{"## Charts", "```chart", "\"type\"", "bar, line, area, pie", "never invent data"} {
		if !strings.Contains(p, must) {
			t.Errorf("chart prompt missing %q", must)
		}
	}
}

func TestAskAISystemPromptIncludesChartsOnlyWithTools(t *testing.T) {
	// Charts ride on the tool-enabled Q&A path. When there are no registered
	// executors, or tools are off, the chart block must not appear (it renders
	// only where the assistant can gather data).
	if len(ai.Executors) == 0 {
		if strings.Contains(AskAISystemPrompt(true), "```chart") {
			t.Fatal("no executors registered: chart block should be absent")
		}
		if strings.Contains(AskAISystemPromptForQuery(true, "chart tasks"), "```chart") {
			t.Fatal("no executors registered: chart block should be absent (routed)")
		}
	}
	// Tools OFF must never carry the chart block regardless of registry state.
	if strings.Contains(AskAISystemPrompt(false), "```chart") {
		t.Error("withTools=false must not include the chart block")
	}
	if strings.Contains(AskAISystemPromptForQuery(false, "chart tasks"), "```chart") {
		t.Error("withTools=false (routed) must not include the chart block")
	}
}

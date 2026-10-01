package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestParseRunTranscript(t *testing.T) {
	// Two steps: web_search (executed), create_task (executed), create_task again
	// (skipped -> not counted), send_dm (errored -> not counted). Distinct tools
	// preserve first-seen order.
	js := `[
		{"tool_calls":[{"tool":"web_search","result":"ok"}]},
		{"tool_calls":[
			{"tool":"create_task","result":"made"},
			{"tool":"create_task","skipped":"duplicate"},
			{"tool":"send_dm","error":"no access"}
		]}
	]`
	tools, actions := parseRunTranscript(js)
	wantTools := []string{"web_search", "create_task", "send_dm"}
	if len(tools) != len(wantTools) {
		t.Fatalf("tools = %v, want %v", tools, wantTools)
	}
	for i, w := range wantTools {
		if tools[i] != w {
			t.Fatalf("tool[%d] = %q, want %q", i, tools[i], w)
		}
	}
	// web_search + first create_task executed; the skipped + errored ones don't count.
	if actions != 2 {
		t.Fatalf("actionCount = %d, want 2", actions)
	}
}

func TestParseRunTranscriptEmptyOrBad(t *testing.T) {
	for _, in := range []string{"", "null", "   ", "not json", "{}"} {
		tools, actions := parseRunTranscript(in)
		if len(tools) != 0 || actions != 0 {
			t.Fatalf("parseRunTranscript(%q) = (%v,%d), want empty", in, tools, actions)
		}
	}
}

func TestRunSummaryPrefersResult(t *testing.T) {
	res := "Posted the weekly digest to #engineering."
	r := &model.AgentRunActivity{}
	r.Status = model.RunSucceeded
	r.Result = &res
	if got := runSummary(r, []string{"send_message"}); got != res {
		t.Fatalf("summary = %q, want result text", got)
	}
}

func TestRunSummaryFallsBackToTools(t *testing.T) {
	r := &model.AgentRunActivity{}
	r.Status = model.RunSucceeded // no result
	got := runSummary(r, []string{"create_task", "send_dm"})
	if got != "Used create task, send dm" {
		t.Fatalf("summary = %q", got)
	}
}

func TestRunSummaryStatusFallback(t *testing.T) {
	r := &model.AgentRunActivity{}
	r.Status = model.RunFailed
	if got := runSummary(r, nil); got != "Run failed" {
		t.Fatalf("summary = %q, want 'Run failed'", got)
	}
}

func TestTrimOneLineActivity(t *testing.T) {
	if got := trimOneLineActivity("  a\n\tb   c ", 100); got != "a b c" {
		t.Fatalf("got %q", got)
	}
	long := trimOneLineActivity("aaaaaaaaaa", 4)
	if len([]rune(long)) != 5 || long[len(long)-len("…"):] != "…" { // 4 chars + ellipsis
		t.Fatalf("expected truncation with ellipsis, got %q", long)
	}
}

// A result with a chart in it is summarised by naming the chart, not by the
// first line of its JSON.
func TestRunSummaryNamesAChart(t *testing.T) {
	res := "Trend below.\n```chart\n{\"type\":\"line\",\"title\":\"Open tickets\",\"labels\":[\"a\"],\"series\":[{\"name\":\"n\",\"values\":[1]}]}\n```"
	r := &model.AgentRunActivity{}
	r.Status = model.RunSucceeded
	r.Result = &res
	if got := runSummary(r, nil); got != "Trend below. [chart: Open tickets]" {
		t.Fatalf("summary = %q", got)
	}
}

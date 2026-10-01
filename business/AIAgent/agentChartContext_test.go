package business

import (
	"strings"
	"testing"
)

func TestAgentHasChartData(t *testing.T) {
	cases := []struct {
		name  string
		tools []string
		want  bool
	}{
		{"nil", nil, false},
		{"empty", []string{}, false},
		{"no data tools", []string{"create_task", "send_message"}, false},
		{"query_table", []string{"send_message", "query_table"}, true},
		{"table read", []string{"read_table"}, true},
		{"list tasks", []string{"list_tasks"}, true},
		{"code repo summary", []string{"repo_summary"}, true},
		{"connector gmail", []string{"gmail_search"}, true},
		{"trims whitespace", []string{" list_commits "}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentHasChartData(c.tools); got != c.want {
				t.Fatalf("agentHasChartData(%v) = %v, want %v", c.tools, got, c.want)
			}
		})
	}
}

func TestBuildChartCapabilityPrompt(t *testing.T) {
	// No data tool => no chart instruction (keeps a conversational agent lean).
	if got := buildChartCapabilityPrompt([]string{"send_message"}); got != "" {
		t.Fatalf("expected empty prompt for non-data agent, got %q", got)
	}

	p := buildChartCapabilityPrompt([]string{"query_table"})
	if p == "" {
		t.Fatal("expected a chart prompt for a data-capable agent")
	}
	for _, must := range []string{"```chart", "\"type\"", "bar, line, area, pie", "never invent data", "query_table"} {
		if !strings.Contains(p, must) {
			t.Errorf("chart prompt missing %q", must)
		}
	}
}

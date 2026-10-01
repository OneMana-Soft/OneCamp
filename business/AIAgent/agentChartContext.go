package business

// agentChartContext.go — teaches a data-capable agent that it may VISUALIZE the
// numbers it gathers by emitting a fenced ```chart block. This is now rendered
// on EVERY surface an agent posts to: the assistant panel (MarkdownMessage) and
// channel / thread / DM / group-chat messages (the BotPost stream converts a
// ```chart block into an inline chart embed node). So, unlike before, the
// capability is worth advertising to the autonomous runner too.
//
// Pure prompt contribution — the backend never parses the block from the model,
// it only recognizes it downstream when rendering — so it stays fully
// LLM/provider-agnostic. Injected only when the agent has a data-bearing read
// tool on its allow-list, so a purely conversational agent spends no prompt
// budget on a capability it can't meaningfully use.

import "strings"

// chartDataTools is the set of read/list tools whose results are countable or
// aggregatable — i.e. worth charting. query_table is the strongest signal (it
// aggregates and returns a ready chart), but any data-bearing reader qualifies.
var chartDataTools = map[string]bool{
	"query_table":          true,
	"read_table":           true,
	"list_tables":          true,
	"list_tasks":           true,
	"list_project_tasks":   true,
	"list_projects":        true,
	"list_commits":         true,
	"list_recent_changes":  true,
	"repo_summary":         true,
	"github_list_prs":      true,
	"github_list_issues":   true,
	"gmail_search":         true,
	"calendar_list_events": true,
	"search_workspace":     true,
}

// agentHasChartData reports whether any enabled tool produces data worth
// charting. Pure and allocation-light so it is trivially unit-testable.
func agentHasChartData(enabledTools []string) bool {
	for _, t := range enabledTools {
		if chartDataTools[strings.TrimSpace(t)] {
			return true
		}
	}
	return false
}

// buildChartCapabilityPrompt returns the system-prompt snippet advertising the
// chart block, or "" when the agent has no data-bearing tool. The wording is
// the steering surface, so it is unit-tested directly.
func buildChartCapabilityPrompt(enabledTools []string) string {
	if !agentHasChartData(enabledTools) {
		return ""
	}
	return "\n\nWhen your answer is better shown as a chart (a trend over time, a breakdown by " +
		"category, a comparison of counts), you MAY render one inline by adding a fenced code block " +
		"tagged `chart` whose body is a compact JSON spec:\n" +
		"```chart\n" +
		"{\"type\":\"bar\",\"title\":\"Optional title\",\"labels\":[\"Jan\",\"Feb\"],\"series\":[{\"name\":\"Revenue\",\"values\":[10,20]}]}\n" +
		"```\n" +
		"Rules: type is one of bar, line, area, pie (pie uses a single series). `labels` are the x-axis " +
		"categories; each series `values` array lines up with `labels` position-by-position. Use ONLY real " +
		"numbers you obtained from your tools — never invent data to fill a chart. If you used query_table, " +
		"it already returns a ready-to-use chart block you can include verbatim. Keep it small and still give " +
		"a short written takeaway alongside it. Omit the chart when a sentence or a short list says it better."
}

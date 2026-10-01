package business

// What the runner tells a model about drawing.

import (
	"strings"

	aiBusiness "github.com/akashc777/OneCamp/business/AI"
)

// chartCapabilityFor is the chart prompt for a run, or nothing.
//
// Advertised only when the run has somewhere to get numbers from: a tool it
// can call, or knowledge it was grounded in. A model with neither can only
// chart what it made up, and the prompt's own first rule is that it must not.
// The same rule the assistant panel applies, for the same reason.
func chartCapabilityFor(enabledTools []string, knowledge string) string {
	if len(enabledTools) == 0 && strings.TrimSpace(knowledge) == "" {
		return ""
	}
	return aiBusiness.ChartCapabilityPrompt()
}

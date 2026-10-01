package business

// The agent run ledger honours the workspace retention window.
//
// Registered rather than called. The sweep that applies retention lives in the
// audit package, which both editions ship; this ledger lives in the AI packages,
// which only one does. Announcing from init means linking this package is what
// puts these records under retention, and the AI-free build, which does not link
// it and has no runs to clear, registers nothing.

import (
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func init() {
	helpers.RegisterRetentionSweeper("agent run",
		"the rows stay, so run counts, acceptance and token totals are unchanged",
		model.RedactRunsOlderThan)
}

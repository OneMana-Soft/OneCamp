package business

// Actions whose outcome is unknown, contributed to the evidence pack.
//
// THIS SECTION IS THE COMPLETENESS CLAIM, and it is the reason the pack's first
// limit could be narrowed. Every effecting tool call now writes its intent to
// durable storage BEFORE it is attempted, and fails closed if it cannot, so an
// effecting agent action cannot have occurred without a record of it existing.
//
// What remains uncertain is not WHETHER an action was recorded but whether a
// recorded one took effect: if the process stops between the intent and the
// outcome, the row stays open. Those rows are listed here rather than described
// in a caveat, because "these four, at these times, by this agent" is something
// a reviewer can act on and "the log may be incomplete" is not.
//
// An empty section is the good case and reads as one: every action that was
// started was also concluded.
//
// Registered rather than called, like the run ledger beside it: the pack is
// assembled in the audit package that both editions ship, and this belongs to
// the AI packages that only one does.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	actionLog "github.com/akashc777/OneCamp/models/postgres/AgentActionLog"
)

func init() {
	helpers.RegisterEvidenceContributor(helpers.EvidenceSection{
		Name: "agent_actions_unresolved",
		Describe: "Agent actions that were recorded as about to happen and never concluded, because the process " +
			"stopped between the two. Every effecting action is recorded before it is attempted, so one cannot have " +
			"happened without appearing in the log; these are the ones whose EFFECT is unknown rather than unrecorded. " +
			"An empty list means every action started in this window was also concluded.",
		Collect: func(ctx context.Context, from, to time.Time) (any, error) {
			return actionLog.ListUnresolved(ctx, from, to, 500)
		},
	})
}

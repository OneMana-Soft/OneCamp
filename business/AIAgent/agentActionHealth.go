package business

// Are there agent actions that started and never finished?
//
// WHAT AN OPEN ROW MEANS. Every effecting agent action writes its intent before
// it is attempted and closes it afterwards. A row still open long after it was
// written means the process stopped between the two: the action may or may not
// have taken effect, and nobody knows which. That is exactly the residual
// uncertainty the evidence pack now lists rather than hedges, and it is worth an
// operator seeing WITHOUT having to export a pack.
//
// A few, briefly, are normal: a run in flight has open rows by definition. The
// window is what separates "happening now" from "nobody is coming back for it".
//
// WHAT IT DOES NOT PROVE. That the actions which DID close did the right thing.
// This is about actions whose fate is unknown, not actions that went wrong.

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	actionLog "github.com/akashc777/OneCamp/models/postgres/AgentActionLog"
)

// stuckAfter is how long an open intent has to sit before it stops being "a run
// in progress" and starts being "a process that did not come back". Generous:
// a single tool call against a slow MCP server can legitimately take a while.
const stuckAfter = 15 * time.Minute

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "agent-actions",
		Kind: helpers.CheckKindBehaviour,
		Describe: "No agent action has been left with its outcome unknown. An open record means the process " +
			"stopped between recording an action and recording its result, so that action may or may not have " +
			"taken effect. It does not prove the actions that completed did the right thing.",
		Probe: func(ctx context.Context) error {
			// The exact count and the true oldest. Taking the last element of
			// ListUnresolved would give neither: it pages newest-first, so a
			// long backlog would be reported as a handful of recent ones.
			n, oldest, err := actionLog.SummariseUnresolved(ctx, time.Now().Add(-stuckAfter))
			if err != nil {
				return fmt.Errorf("could not read the agent action log: %w", err)
			}
			if n > 0 && oldest != nil {
				return fmt.Errorf("%d agent action(s) were recorded as about to happen and never concluded, "+
					"the oldest at %s (tool: %s); their effect is unknown and they are listed in the evidence pack",
					n, oldest.IntentAt.UTC().Format(time.RFC3339), oldest.ToolName)
			}
			return nil
		},
	})
}

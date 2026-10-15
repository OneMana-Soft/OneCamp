package business

// A turn that has to ask someone something asks before any of it runs.
//
// A run pauses for a person in two ways: a needs_human call puts a question to
// them, and a remember or create_routine whose words are not the asker's own is
// put to the asker first (askToKeep). Either pause ends the run part way through
// a turn, and a turn reaches the saved conversation only once all of it has run
// (the runner appends it, then checkpoints). So the run that resumes on the
// answer starts from before that turn, and the model asks for its calls again.
// Whatever the turn had done by the time it paused, it did a second time. Asked
// for a routine and then for a line to keep, a durable job set the routine up
// on the yes to the first question, paused on the second, and set the routine
// up again on the yes to that. The dedupe set did not catch it: it is rebuilt
// from the saved conversation, and the memory and routine tools never read it.
//
// So the runner looks through a turn for the call it will pause on before it
// runs any of the turn (pauseFirst), and pauses there. Nothing the turn asked
// for has happened when the person is asked; once they answer, the turn is
// asked for again and runs once.

import (
	"context"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// turnPause is the call a turn pauses on: a needs_human question (elic), or a
// question to put to the asker before something is kept (rec.Confirm).
type turnPause struct {
	rec  toolCallRecord
	elic *Elicitation
}

// pauseFirst finds the first call in a turn that will pause the run. Each call
// is decided as the runner's loop decides it, and none is run. ok is false when
// the turn has nothing to ask.
func pauseFirst(ctx context.Context, agent *model.AiAgent, actions []ai.ProposedAction) (p turnPause, ok bool) {
	sc := agentRunScopeFromCtx(ctx)
	for _, a := range actions {
		rec := toolCallRecord{Tool: a.ToolName, Params: a.Params}
		switch a.ToolName {
		case blockerToolName:
			elic := newElicitation(a.Params["reason"], a.Params["options"])
			// The loop answers these with a tool error, and asks nobody.
			if internalIDElicitation(elic.Question) || sensitiveElicitation(elic.Question) {
				continue
			}
			return turnPause{rec: rec, elic: &elic}, true
		case rememberToolName:
			if sc.hasScope() {
				mayRemember(ctx, agent, a, &rec)
			}
		case createRoutineToolName:
			if sc.hasScope() {
				mayCreateRoutine(ctx, agent, sc, a, &rec)
			}
		}
		if rec.Confirm != "" {
			return turnPause{rec: rec}, true
		}
	}
	return turnPause{}, false
}

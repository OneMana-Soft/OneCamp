package business

// Translating what the runner knows into what the audit log records.
//
// The runner speaks in trigger sources ("manual", "mention", "schedule",
// "event", "ambient", "task_assignment", "dm", "eval"), which are its own
// vocabulary for where a prompt came from. The audit log asks a narrower
// question with a fixed answer set: was anybody there. This is the one place
// the two are mapped, so a new trigger source has to be classified here or the
// row is written without an answer rather than with a wrong one.

import (
	"context"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// initiatorForTrigger classifies a synchronous run by how it was launched.
//
// hop is the delegation depth. Any hop above zero means another agent asked
// for this, and that outranks the trigger source: a mention written by a bot
// user arrives through the same path as one a person typed, and the person's
// absence is the whole fact worth recording.
func initiatorForTrigger(triggerSource string, hop int) auditBusiness.Initiator {
	if hop > 0 {
		return auditBusiness.InitiatorHandoff
	}
	switch triggerSource {
	case model.TriggerSchedule:
		return auditBusiness.InitiatorSchedule
	case model.TriggerEvent, triggerSourceAmbient:
		return auditBusiness.InitiatorEvent
	case "eval":
		return auditBusiness.InitiatorEval
	}
	// manual, mention, dm, task_assignment, and anything not yet classified:
	// somebody was in the room. The default is the one that does not hide an
	// unattended run, because the unattended kinds are all named above.
	return auditBusiness.InitiatorPerson
}

// initiatorForTask classifies a durable run from what the queue preserved.
//
// The worker starts every durable run with the same trigger source, so the
// launch vocabulary is gone by the time it runs; what survives on the task row
// is the delegation hop, which is the one thing that changes the answer.
func initiatorForTask(t *model.AgentTask) auditBusiness.Initiator {
	if t == nil {
		return auditBusiness.InitiatorPerson
	}
	return initiatorForTrigger(t.SourceType, t.DelegationHop)
}

// withRunInitiator puts the answer on the context for the run, unless the
// caller already did: a dispatcher that knows the hop sets it before the runner
// sees the trigger source, and its answer wins.
func withRunInitiator(ctx context.Context, triggerSource string) context.Context {
	if _, already := auditBusiness.InitiatorFromCtx(ctx); already {
		return ctx
	}
	return auditBusiness.WithInitiator(ctx, initiatorForTrigger(triggerSource, 0))
}

package business

import (
	"context"
	"testing"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// The one place the runner's launch vocabulary becomes the audit log's answer.

func TestEveryTriggerSourceHasAnAnswer(t *testing.T) {
	want := map[string]auditBusiness.Initiator{
		"manual":              auditBusiness.InitiatorPerson,
		model.TriggerMention:  auditBusiness.InitiatorPerson,
		"dm":                  auditBusiness.InitiatorPerson,
		"task_assignment":     auditBusiness.InitiatorPerson,
		model.TriggerSchedule: auditBusiness.InitiatorSchedule,
		model.TriggerEvent:    auditBusiness.InitiatorEvent,
		triggerSourceAmbient:  auditBusiness.InitiatorEvent,
		"eval":                auditBusiness.InitiatorEval,
	}
	for src, kind := range want {
		if got := initiatorForTrigger(src, 0); got != kind {
			t.Errorf("trigger %q classified as %s, want %s", src, got, kind)
		}
	}
}

func TestAHopOutranksTheTrigger(t *testing.T) {
	// A mention written by a bot user arrives through the same path as one a
	// person typed. The hop is the only thing that says the person was absent.
	if got := initiatorForTrigger(model.TriggerMention, 1); got != auditBusiness.InitiatorHandoff {
		t.Errorf("a delegated mention classified as %s, want handoff", got)
	}
	if got := initiatorForTrigger("task_assignment", 2); got != auditBusiness.InitiatorHandoff {
		t.Errorf("a delegated durable run classified as %s, want handoff", got)
	}
}

func TestADurableRunIsClassifiedFromWhatTheQueueKept(t *testing.T) {
	// The worker starts every durable run with the same trigger source, so the
	// launch vocabulary is gone; the delegation hop on the task row is what
	// survives, and it is the one thing that changes the answer.
	if got := initiatorForTask(&model.AgentTask{SourceType: "channel_post", DelegationHop: 0}); got != auditBusiness.InitiatorPerson {
		t.Errorf("a plain durable run classified as %s, want person", got)
	}
	if got := initiatorForTask(&model.AgentTask{SourceType: "channel_post", DelegationHop: 1}); got != auditBusiness.InitiatorHandoff {
		t.Errorf("a delegated durable run classified as %s, want handoff", got)
	}
	if got := initiatorForTask(nil); got != auditBusiness.InitiatorPerson {
		t.Errorf("a nil task classified as %s", got)
	}
}

func TestADispatcherThatKnewMoreIsNotOverruled(t *testing.T) {
	// The mention dispatcher sets handoff when it sees a hop. The runner then
	// sees trigger "mention" and must not replace that with person.
	ctx := auditBusiness.WithInitiator(context.Background(), auditBusiness.InitiatorHandoff)
	ctx = withRunInitiator(ctx, model.TriggerMention)
	got, _ := auditBusiness.InitiatorFromCtx(ctx)
	if got != auditBusiness.InitiatorHandoff {
		t.Errorf("the runner overwrote the dispatcher's handoff with %s", got)
	}
}

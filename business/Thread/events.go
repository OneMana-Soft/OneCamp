package business

// Every thread change is a Resource Event (I14), emitted through the bus A6
// builds; until it lands the bus keeps nothing. Live updates to open tabs
// (MQTT), notifications and webhooks are the bus's to fan out, so threads
// publish nothing of their own. An event carries ids, never a comment's text.
//
// For A6: Subjects are the people taking part (author, repliers, people
// mentioned; never someone who only read it) and anyone newly mentioned,
// without whoever acted. A thread's comments on a doc also go through the doc
// comment path, which tells the doc's people today (W0-3); when A6 notifies
// per thread from these events, that doc-wide notice for a threaded comment
// should go, or people hear twice.

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// The kinds of thread event. Never renamed once shipped.
const (
	ThreadCreated    resourceEventBusiness.Kind = "thread.created"
	ThreadReplied    resourceEventBusiness.Kind = "thread.replied"
	ThreadResolved   resourceEventBusiness.Kind = "thread.resolved"
	ThreadReopened   resourceEventBusiness.Kind = "thread.reopened"
	ThreadReanchored resourceEventBusiness.Kind = "thread.reanchored"
)

func (s *service) emit(ctx context.Context, kind resourceEventBusiness.Kind, t Thread, who accessBusiness.Subject, subjects []string, commentID string) {
	data := map[string]string{
		"thread_id":   t.ID,
		"anchor_kind": string(t.Anchor.Kind),
		"status":      string(t.Status),
	}
	if commentID != "" {
		data["comment_id"] = commentID
	}
	resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
		Kind: kind, ResourceKind: t.ResourceKind, ResourceUUID: t.ResourceUUID,
		Actor:    resourceEventBusiness.Actor{UserUUID: who.UserUUID, AgentUUID: who.AgentUUID},
		At:       s.now(),
		Subjects: subjects,
		Data:     data,
	})
}

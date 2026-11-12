package business

// Telling a person that an agent is waiting on them.
//
// An approval card reaches an open OneCamp tab over MQTT, and Home lists open
// approvals first. Neither reaches someone who is not looking, which is the
// usual case for work an agent does in the background, and the whole point of
// agents that keep going after you close the app (Meta's Muse, Grok Bot, or
// ChatGPT over MCP). Without a push the agent waits out its TTL and the work
// silently does not happen. So a new approval also goes to the person's
// devices. Firebase only shows it when OneCamp is not in focus, so someone
// already looking at the card is not told twice.
//
// The push names the approval (type_id), so a click opens that request in the
// Inbox rather than a page to search, and says an agent sent it (sender_kind),
// so the toast wears the agent square. Its agent's name is the one the
// approval was given when it was made (nameAgents).

import (
	"context"
	"strings"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

const approvalPushBodyMax = 140

// A seam, so the rule is tested without Firebase. To their devices and the
// desktop app (business/Notification Deliver), named by the approval's own tag
// so two never stand for each other.
var sendApprovalPush = func(ctx context.Context, userUUID string, data map[string]string) error {
	notificationBusiness.Deliver(ctx, []string{userUUID}, data, data[firebaseInit.FIREBASE_PUSH_DATA_TAG])
	return nil
}

// approvalPushData is what the notification says. Pure.
func approvalPushData(a *pendingModels.PendingAction, agentName string) map[string]string {
	who := strings.TrimSpace(agentName)
	if who == "" {
		who = "OneCamp AI"
	}
	body := strings.Join(strings.Fields(a.Description), " ")
	if body == "" {
		body = "Open OneCamp to approve or deny it."
	}
	if r := []rune(body); len(r) > approvalPushBodyMax {
		body = strings.TrimSpace(string(r[:approvalPushBodyMax-1])) + "…"
	}
	return map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:  firebaseInit.FIREBASE_PUSH_DATA_TYPE_APPROVAL,
		firebaseInit.FIREBASE_PUSH_DATA_TITLE: who + " needs your approval",
		firebaseInit.FIREBASE_PUSH_DATA_BODY:  body,
		firebaseInit.FIREBASE_PUSH_DATA_TAG:   "approval_" + a.Id.String(),
		// The approval itself, so the click opens it (For you's row).
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID: a.Id.String(),
		// OneCamp AI and agents both wear the agent square.
		firebaseInit.FIREBASE_PUSH_DATA_SENDER_KIND: firebaseInit.FIREBASE_PUSH_SENDER_AGENT,
	}
}

// notifyApprovalNeeded pushes a new approval to the person it waits on. It
// runs detached: a slow or failing push must never hold up, or fail, the
// proposal itself.
func notifyApprovalNeeded(a *pendingModels.PendingAction) {
	if a == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.LogErrorWithContext(context.Background(), "business/AI/notifyApprovalNeeded panic: %v", r)
			}
		}()
		ctx := context.Background()
		data := approvalPushData(a, a.AgentName)
		if err := sendApprovalPush(ctx, a.RequestedBy.String(), data); err != nil {
			helpers.LogErrorWithContext(ctx, "business/AI/notifyApprovalNeeded push failed err: %v", err)
		}
	}()
}

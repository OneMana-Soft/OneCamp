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

import (
	"context"
	"strings"

	userFCMTokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

const approvalPushBodyMax = 140

// Seams, so the rule is tested without Firebase or a database.
var (
	approvalAgentName = func(ctx context.Context, a *pendingModels.PendingAction) string {
		if a.AgentID == nil {
			return ""
		}
		ag, err := agentModel.GetAgentByID(ctx, *a.AgentID)
		if err != nil || ag == nil {
			return ""
		}
		return ag.Name
	}
	sendApprovalPush = func(ctx context.Context, userUUID string, data map[string]string) error {
		tokens, err := userFCMTokenBusiness.GetFCMTokenByUserId(ctx, userUUID)
		if err != nil || len(tokens) == 0 {
			return err
		}
		return firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens)
	}
)

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
		data := approvalPushData(a, approvalAgentName(ctx, a))
		if err := sendApprovalPush(ctx, a.RequestedBy.String(), data); err != nil {
			helpers.LogErrorWithContext(ctx, "business/AI/notifyApprovalNeeded push failed err: %v", err)
		}
	}()
}

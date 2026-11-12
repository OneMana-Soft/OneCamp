package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
	"github.com/google/uuid"
)

func TestResolvedMsgCarriesOutcome(t *testing.T) {
	result := "Created task #42"
	a := &pendingModels.PendingAction{
		Id:          uuid.New(),
		RequestedBy: uuid.New(),
		SurfaceType: pendingModels.SurfaceChannel,
		SurfaceID:   "chan-1",
		ToolName:    "create_task",
		Description: "Create a task",
		Status:      pendingModels.StatusExecuted,
		Result:      &result,
	}
	msg := resolvedMsg(a)
	if msg.Action != "resolved" {
		t.Fatalf("expected action=resolved, got %q", msg.Action)
	}
	if msg.Status != pendingModels.StatusExecuted || msg.Result != result {
		t.Fatalf("outcome not carried: %+v", msg)
	}
	if msg.Error != "" {
		t.Fatalf("expected empty error on success, got %q", msg.Error)
	}
}

func TestResolvedMsgCarriesError(t *testing.T) {
	errText := "Action failed: permission denied"
	a := &pendingModels.PendingAction{
		Id:       uuid.New(),
		ToolName: "create_doc",
		Status:   pendingModels.StatusFailed,
		Error:    &errText,
	}
	msg := resolvedMsg(a)
	if msg.Status != pendingModels.StatusFailed || msg.Error != errText {
		t.Fatalf("error outcome not carried: %+v", msg)
	}
}

// CreatePendingAction must reject an empty tool name before touching the DB, so
// a malformed proposal never becomes a card.
func TestCreatePendingActionRejectsEmptyTool(t *testing.T) {
	_, err := CreatePendingAction(context.Background(), uuid.New(), pendingModels.SurfaceChannel, "c1", "   ", nil, "desc", "")
	if err == nil || !strings.Contains(err.Error(), "tool_name is required") {
		t.Fatalf("expected tool_name validation error, got %v", err)
	}
}

// The person deciding sees who is asking ("Release Captain wants to…"), on
// every open approval, from one read of the agents however long the list is.
func TestOpenActionsNameTheirAgent(t *testing.T) {
	savedList, savedNames := listOpenActions, agentNamesByID
	t.Cleanup(func() { listOpenActions, agentNamesByID = savedList, savedNames })

	captain, gone := uuid.New(), uuid.New()
	me := uuid.New()
	listOpenActions = func(_ context.Context, user uuid.UUID) ([]*pendingModels.PendingAction, error) {
		if user != me {
			t.Fatalf("listed for %s, want the caller %s", user, me)
		}
		return []*pendingModels.PendingAction{
			{Id: uuid.New(), ToolName: "gmail_send", AgentID: &captain},
			{Id: uuid.New(), ToolName: "create_task"},
			{Id: uuid.New(), ToolName: "create_task", AgentID: &captain},
			{Id: uuid.New(), ToolName: "create_doc", AgentID: &gone},
		}, nil
	}
	reads := 0
	agentNamesByID = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		reads++
		if len(ids) != 2 {
			t.Fatalf("asked for %d ids, want the 2 distinct agents once each", len(ids))
		}
		return map[uuid.UUID]string{captain: "Release Captain"}, nil
	}

	actions, err := ListOpenPendingActions(context.Background(), me)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("%d reads of the agents, want 1 for the page", reads)
	}
	want := []string{"Release Captain", "", "Release Captain", ""}
	for i, a := range actions {
		if a.AgentName != want[i] {
			t.Fatalf("action %d named %q, want %q", i, a.AgentName, want[i])
		}
	}
	if !actions[0].Destructive {
		t.Fatal("naming must not drop the risk flag an email carries")
	}
}

// Names are a courtesy: a failed read still lists every approval (unnamed,
// shown as OneCamp AI), and a page with no agent reads nothing.
func TestOpenActionsListWhenNamesCannotBeRead(t *testing.T) {
	savedList, savedNames := listOpenActions, agentNamesByID
	t.Cleanup(func() { listOpenActions, agentNamesByID = savedList, savedNames })

	agent := uuid.New()
	listOpenActions = func(context.Context, uuid.UUID) ([]*pendingModels.PendingAction, error) {
		return []*pendingModels.PendingAction{{Id: uuid.New(), ToolName: "create_task", AgentID: &agent}}, nil
	}
	agentNamesByID = func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		return nil, errors.New("database is resting")
	}
	actions, err := ListOpenPendingActions(context.Background(), uuid.New())
	if err != nil || len(actions) != 1 || actions[0].AgentName != "" {
		t.Fatalf("got %d actions, err %v; a failed name read must not hide an approval", len(actions), err)
	}

	listOpenActions = func(context.Context, uuid.UUID) ([]*pendingModels.PendingAction, error) {
		return []*pendingModels.PendingAction{{Id: uuid.New(), ToolName: "create_task"}}, nil
	}
	agentNamesByID = func(context.Context, []uuid.UUID) (map[uuid.UUID]string, error) {
		t.Fatal("no agent on the page, so no read")
		return nil, nil
	}
	if _, err := ListOpenPendingActions(context.Background(), uuid.New()); err != nil {
		t.Fatal(err)
	}
}

// A card that arrives live says who asks and until when, as the listed one
// does, so it never has to be read again to be drawn.
func TestCreatedMsgNamesTheAgentAndTheDeadline(t *testing.T) {
	expires := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	a := &pendingModels.PendingAction{
		Id:          uuid.New(),
		SurfaceType: pendingModels.SurfaceAssistant,
		ToolName:    "gmail_send",
		Description: "Email the release notes",
		Status:      pendingModels.StatusPending,
		AgentName:   "Release Captain",
		ExpiresAt:   expires,
		CreatedAt:   expires.Add(-24 * time.Hour),
	}
	enrichRisk(a)
	msg := createdMsg(a)
	if msg.Action != "created" || msg.ID != a.Id.String() || msg.ToolName != "gmail_send" || !msg.Destructive {
		t.Fatalf("created message lost what it carried: %+v", msg)
	}
	if msg.AgentName != "Release Captain" {
		t.Fatalf("agent_name = %q", msg.AgentName)
	}
	if msg.ExpiresAt != "2026-11-10T09:30:00Z" || msg.CreatedAt != "2026-11-09T09:30:00Z" {
		t.Fatalf("expires_at = %q, created_at = %q", msg.ExpiresAt, msg.CreatedAt)
	}
}

// A new approval, one action or an agent's plan, reaches its person named:
// the card that arrives live says who asks and until when, the push says who
// asks, and the plan is pushed too (V-862). A retry of the same proposal
// redraws the card and pushes nothing.
func TestNewApprovalsAreNamedLiveAndPushed(t *testing.T) {
	savedCreate, savedNames, savedPublish, savedSend := createActionRow, agentNamesByID, publishPendingAction, sendApprovalPush
	t.Cleanup(func() {
		createActionRow, agentNamesByID, publishPendingAction, sendApprovalPush = savedCreate, savedNames, savedPublish, savedSend
	})

	owner, captain := uuid.New(), uuid.New()
	fresh := true
	createActionRow = func(_ context.Context, requestedBy uuid.UUID, surfaceType, surfaceID, toolName string, params map[string]string, description, _ string, expiresAt time.Time, attr pendingModels.Attribution, _ pendingModels.Binding) (*pendingModels.PendingAction, error) {
		return &pendingModels.PendingAction{
			Id: uuid.New(), RequestedBy: requestedBy, SurfaceType: surfaceType, SurfaceID: surfaceID,
			ToolName: toolName, Params: params, Description: description, Status: pendingModels.StatusPending,
			ExpiresAt: expiresAt, CreatedAt: time.Now(), AgentID: attr.AgentID, Fresh: fresh,
		}, nil
	}
	agentNamesByID = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		return map[uuid.UUID]string{captain: "Release Captain"}, nil
	}
	var live []*mqttStruct.MqttPendingAction
	publishPendingAction = func(user string, msg *mqttStruct.MqttPendingAction) {
		if user != owner.String() {
			t.Errorf("card sent to %s, want the requester", user)
		}
		live = append(live, msg)
	}
	pushed := make(chan map[string]string, 4)
	sendApprovalPush = func(_ context.Context, _ string, data map[string]string) error {
		pushed <- data
		return nil
	}

	by := pendingModels.Attribution{AgentID: &captain}
	params := map[string]string{"task_uuid": "Q4-12", "status": "done"}
	create := map[string]func() (*pendingModels.PendingAction, error){
		"action": func() (*pendingModels.PendingAction, error) {
			return CreatePendingAction(context.Background(), owner, "agent", captain.String(), "update_task_status", params, "Mark Q4-12 done", "", by)
		},
		"plan": func() (*pendingModels.PendingAction, error) {
			return CreatePlanAction(context.Background(), owner, "agent", captain.String(),
				[]PlanStep{{ToolName: "update_task_status", Params: params, Description: "Mark Q4-12 done"}},
				BuildPlanSummary("Release Captain", []PlanStep{{Description: "Mark Q4-12 done"}}), by)
		},
	}
	for _, kind := range []string{"action", "plan"} {
		live = nil
		fresh = true
		a, err := create[kind]()
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if a.AgentName != "Release Captain" {
			t.Fatalf("%s returned named %q", kind, a.AgentName)
		}
		if len(live) != 1 || live[0].Action != "created" || live[0].ID != a.Id.String() {
			t.Fatalf("%s: live cards %+v, want one created card", kind, live)
		}
		if live[0].AgentName != "Release Captain" || live[0].ExpiresAt != a.ExpiresAt.Format(time.RFC3339) || live[0].ExpiresAt == "" {
			t.Fatalf("%s: live card agent_name %q, expires_at %q", kind, live[0].AgentName, live[0].ExpiresAt)
		}
		select {
		case d := <-pushed:
			if d[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != "Release Captain needs your approval" || d[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != a.Id.String() {
				t.Fatalf("%s: push %v", kind, d)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no push was sent", kind)
		}

		fresh = false
		if _, err := create[kind](); err != nil {
			t.Fatal(err)
		}
		if len(live) != 2 {
			t.Fatalf("%s: a retry must still redraw the card", kind)
		}
		select {
		case d := <-pushed:
			t.Fatalf("%s: a retry was pushed again: %v", kind, d)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

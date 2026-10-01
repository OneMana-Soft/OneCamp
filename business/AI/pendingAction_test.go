package business

import (
	"context"
	"strings"
	"testing"

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

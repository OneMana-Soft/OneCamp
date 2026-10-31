package business

import (
	"context"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// A follow-up to the A4 cost fix (CR-9): a run that a batch's task.created
// started kept the batch's open tasks (helpers.WithOpenTasks), so for as long
// as it ran, what it said of those tasks skipped their restriction read,
// though one may have been restricted since.
func TestAnAgentRunKeepsNoBatchsOpenTasks(t *testing.T) {
	made := uuid.New()
	batch := helpers.WithOpenTasks(context.Background(), made)
	for _, trigger := range []string{model.TriggerEvent, model.TriggerMention, model.TriggerSchedule, model.TriggerManual} {
		if helpers.TasksOpen(agentRunContext(batch, trigger, "ravi"), made.String()) {
			t.Errorf("a run by %s still marks the batch's task open", trigger)
		}
	}
}

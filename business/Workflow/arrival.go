package business

// Arriving work (linear-21 H2): task_arrived rules hear Triage's task.arrived
// (work that came to wait in a project's triage), may test the door it came
// in by (the source condition), decide it through the triage queue as the
// rule's owner (the triage action), and give it to, or tell, whoever the
// project's rota has on duty (on_duty).

import (
	"context"
	"errors"
	"fmt"

	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
)

// Seams for tests.
var (
	// decideAs is the one decision path (its audit and the requester's
	// notice), asked as the rule's owner, who needs to be able to change
	// the task.
	decideAs = triageBusiness.Triage.Decide
	// onDutyIn is who the project's rota has on duty now.
	onDutyIn = triageBusiness.OnDuty
)

var errNobodyOnDuty = errors.New("nobody is on triage duty: set the project's rota")

// onDutyNow is who is on the run's project's triage duty.
func onDutyNow(ctx context.Context, rc *runCtx) (string, error) {
	who, err := onDutyIn(ctx, rc.projectID())
	if err != nil {
		return "", err
	}
	if who == "" {
		return "", errNobodyOnDuty
	}
	return who, nil
}

// arrivalOnly refuses deciding, or testing the door, in a rule that doesn't
// hear arriving work: there is nothing waiting to decide, and no door.
// validate runs it, so every door (workspace and project, create, update,
// drafts) refuses alike.
func arrivalOnly(trigger string, conds Conditions, branches ...[]WorkflowAction) error {
	if trigger == workflowModel.TriggerTaskArrived {
		return nil
	}
	for _, c := range conds.Items {
		if c.Field == "source" {
			return fmt.Errorf("only a rule on arriving work tests the door it came in by")
		}
	}
	for _, actions := range branches {
		for _, a := range actions {
			if a.Type == ActionTriage {
				return fmt.Errorf("only a rule on arriving work accepts or declines it")
			}
		}
	}
	return nil
}

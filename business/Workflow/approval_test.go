package business

import (
	"context"
	"encoding/json"
	"testing"

	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// An approval's rule (package 9 R2b) says how the request must end, or any
// ending; a workspace's own workflows can't start on one.
func TestAnApprovalRuleSaysHowTheRequestMustEnd(t *testing.T) {
	newStore(t)
	var saved []workflowModel.Fields
	oldCount, oldCreate, oldGet, oldOwner := countRules, createRule, getRule, setOwner
	t.Cleanup(func() { countRules, createRule, getRule, setOwner = oldCount, oldCreate, oldGet, oldOwner })
	countRules = func(context.Context, uuid.UUID) (int, error) { return 0, nil }
	createRule = func(_ context.Context, _ uuid.UUID, f workflowModel.Fields) (uuid.UUID, error) {
		saved = append(saved, f)
		return uuid.New(), nil
	}
	getRule = func(context.Context, uuid.UUID) (*workflowModel.Workflow, error) {
		return &workflowModel.Workflow{}, nil
	}
	who := &userModels.UserInfo{}
	rule := func(cfg map[string]interface{}) WorkflowInput {
		return WorkflowInput{Name: "Approved", TriggerType: workflowModel.TriggerApprovalDecided, TriggerConfig: cfg,
			Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "low"}}}
	}
	project := uuid.New()
	for _, c := range []struct {
		cfg  map[string]interface{}
		want string
	}{{map[string]interface{}{"outcome": "approved"}, "approved"}, {map[string]interface{}{"outcome": "any"}, ""}, {map[string]interface{}{}, ""}} {
		saved = nil
		if _, err := CreateProjectRule(context.Background(), who, project, rule(c.cfg)); err != nil {
			t.Fatalf("%v: %v", c.cfg, err)
		}
		var cfg map[string]interface{}
		_ = json.Unmarshal([]byte(saved[0].TriggerConfigJSON), &cfg)
		if got, _ := cfg["outcome"].(string); got != c.want {
			t.Errorf("%v saved %s", c.cfg, saved[0].TriggerConfigJSON)
		}
	}
	if _, err := CreateProjectRule(context.Background(), who, project, rule(map[string]interface{}{"outcome": "maybe"})); err == nil {
		t.Error("an outcome that isn't one was saved")
	}
}

// It runs on the ending it names, and its steps can say who answered, how,
// and what they said.
func TestAnApprovalRuleRunsOnItsEndingWithItsWords(t *testing.T) {
	cw, err := compile(&workflowModel.Workflow{Id: uuid.New(), Name: "Approved", CreatedBy: uuid.New(), IsActive: true,
		TriggerType: workflowModel.TriggerApprovalDecided, TriggerConfig: `{"outcome":"changes"}`, MatchType: "any",
		Actions: `[{"type":"add_comment","text":"x"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if !cw.wants(workflowModel.TriggerApprovalDecided, map[string]interface{}{"outcome": "changes"}, taskStatusBusiness.Move{}) {
		t.Error("a rule skipped its own ending")
	}
	if cw.wants(workflowModel.TriggerApprovalDecided, map[string]interface{}{"outcome": "approved"}, taskStatusBusiness.Move{}) {
		t.Error("a rule ran on another ending")
	}
	vars := taskEventVars(map[string]interface{}{"task_id": "t1", "task_name": "New logo", "outcome": "changes", "approver": "Dana (client)", "comment": "Bigger, please"})
	if got := applyVars("{approver} {outcome} on {task}: {comment}", vars); got != "Dana (client) asked for changes on New logo: Bigger, please" {
		t.Errorf("text %q", got)
	}
}

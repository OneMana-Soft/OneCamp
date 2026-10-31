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

// A form rule names one of its project's forms: another project's, or none,
// isn't saved, and what it keeps of its trigger is the form and the zone.
func TestAFormRuleNamesOneOfItsProjectsForms(t *testing.T) {
	newStore(t)
	var saved []workflowModel.Fields
	oldCount, oldCreate, oldGet, oldOwner, oldForm := countRules, createRule, getRule, setOwner, formIn
	t.Cleanup(func() {
		countRules, createRule, getRule, setOwner, formIn = oldCount, oldCreate, oldGet, oldOwner, oldForm
	})
	countRules = func(context.Context, uuid.UUID) (int, error) { return 0, nil }
	createRule = func(_ context.Context, _ uuid.UUID, f workflowModel.Fields) (uuid.UUID, error) {
		saved = append(saved, f)
		return uuid.New(), nil
	}
	getRule = func(context.Context, uuid.UUID) (*workflowModel.Workflow, error) {
		return &workflowModel.Workflow{}, nil
	}
	project, mine, theirs := uuid.New(), uuid.New(), uuid.New()
	formIn = func(_ context.Context, p, f uuid.UUID) (bool, error) { return p == project && f == mine, nil }
	who := &userModels.UserInfo{}
	rule := func(cfg map[string]interface{}) WorkflowInput {
		return WorkflowInput{Name: "Requests", TriggerType: workflowModel.TriggerFormSubmitted, TriggerConfig: cfg,
			Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}
	}
	if _, err := CreateProjectRule(context.Background(), who, project, rule(map[string]interface{}{"form_id": mine.String(), "tz": "Asia/Kolkata", "junk": 1})); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	_ = json.Unmarshal([]byte(saved[0].TriggerConfigJSON), &cfg)
	if cfg["form_id"] != mine.String() || cfg["tz"] != "Asia/Kolkata" || len(cfg) != 2 {
		t.Errorf("saved %s", saved[0].TriggerConfigJSON)
	}
	for _, bad := range []map[string]interface{}{{"form_id": theirs.String()}, {"form_id": "nope"}, {}} {
		if _, err := CreateProjectRule(context.Background(), who, project, rule(bad)); err == nil {
			t.Errorf("a form rule on %v was saved", bad)
		}
	}
}

// A form rule runs on its own form's submissions only, and its steps can say
// the answers: {answer:<question id>}, {form}, and {project}, which a sent
// form names as a task's events do.
func TestAFormRuleRunsOnItsFormWithItsAnswers(t *testing.T) {
	form := uuid.NewString()
	cw, err := compile(&workflowModel.Workflow{Id: uuid.New(), Name: "Requests", CreatedBy: uuid.New(), IsActive: true,
		TriggerType: workflowModel.TriggerFormSubmitted, TriggerConfig: `{"form_id":"` + form + `"}`, MatchType: "any",
		Actions: `[{"type":"add_comment","text":"x"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if !cw.wants(workflowModel.TriggerFormSubmitted, map[string]interface{}{"form_id": form}, taskStatusBusiness.Move{}) {
		t.Error("a rule skipped its own form")
	}
	if cw.wants(workflowModel.TriggerFormSubmitted, map[string]interface{}{"form_id": uuid.NewString()}, taskStatusBusiness.Move{}) {
		t.Error("a rule ran on another form")
	}
	vars := taskEventVars(map[string]interface{}{
		"task_id": "t1", "name": "New logo", "form_title": "Design requests", "project_name": "Q4 launch",
		"answers": map[string]interface{}{"team": "Design", "seats": "3"},
	})
	if vars["answer:team"] != "Design" || vars["answer:seats"] != "3" || vars["form"] != "Design requests" || vars["task"] != "New logo" {
		t.Errorf("vars %v", vars)
	}
	if got := applyVars("{form} in {project}: {answer:team} wants {task}", vars); got != "Design requests in Q4 launch: Design wants New logo" {
		t.Errorf("text %q", got)
	}
}

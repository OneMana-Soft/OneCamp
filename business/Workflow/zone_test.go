package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

func zoneFor(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no zone data for %s", name)
	}
	return loc
}

// compiled is a saved rule as the engine loads it.
func compiled(t *testing.T, cfg, conds, then, otherwise string) *compiledWorkflow {
	t.Helper()
	cw, err := compile(&workflowModel.Workflow{Id: uuid.New(), Name: "Due", CreatedBy: uuid.New(), IsActive: true,
		TriggerType: workflowModel.TriggerTaskCreated, TriggerConfig: cfg, MatchType: "any",
		Conditions: conds, Actions: then, ElseActions: otherwise})
	if err != nil {
		t.Fatal(err)
	}
	return cw
}

// A due date is a day: the web writes the midnight it starts, in the zone of
// whoever set it. A rule saved in Kolkata reads "overdue" as the task list
// does, once that day has passed there, so a task due today is still due
// today at 21:00, not overdue since midnight.
func TestARuleReadsDueDatesAsDaysInItsZone(t *testing.T) {
	kolkata := zoneFor(t, "Asia/Kolkata")
	f := newStore(t)
	oldClock := clock
	t.Cleanup(func() { clock = oldClock })
	clock = func() time.Time { return time.Date(2026, 10, 20, 21, 0, 0, 0, kolkata) }
	today := time.Date(2026, 10, 20, 0, 0, 0, 0, kolkata)
	f.task.DueDate = &today
	cw := compiled(t, `{"tz":"Asia/Kolkata"}`, `{"match":"all","items":[{"field":"due","op":"overdue"}]}`,
		`[{"type":"set_priority","priority":"high"}]`, `[{"type":"set_priority","priority":"medium"}]`)
	runWorkflow(context.Background(), cw, f.event("today"))
	if f.task.Priority != "medium" {
		t.Errorf("a task due today was taken as overdue at 21:00 in its zone: priority %q", f.task.Priority)
	}

	yesterday := today.AddDate(0, 0, -1)
	f.task.DueDate, f.task.Priority = &yesterday, "low"
	runWorkflow(context.Background(), cw, f.event("yesterday"))
	if f.task.Priority != "high" {
		t.Errorf("a task due yesterday wasn't overdue: priority %q", f.task.Priority)
	}

	soon := compiled(t, `{"tz":"Asia/Kolkata"}`, `{"match":"all","items":[{"field":"due","op":"within_days","value":"0"}]}`,
		`[{"type":"set_priority","priority":"high"}]`, `[]`)
	f.task.DueDate, f.task.Priority = &today, "low"
	runWorkflow(context.Background(), soon, f.event("within"))
	if f.task.Priority != "high" {
		t.Errorf("a task due today isn't due within 0 days at 21:00: priority %q", f.task.Priority)
	}
}

// "Due in N days" writes that day's midnight in the rule's zone, as the web's
// date picker writes one, so the day it shows is the day the rule meant.
func TestDueInDaysIsThatDaysMidnightInTheRulesZone(t *testing.T) {
	ny := zoneFor(t, "America/New_York")
	f := newStore(t)
	oldClock := clock
	t.Cleanup(func() { clock = oldClock })
	clock = func() time.Time { return time.Date(2026, 10, 20, 21, 30, 0, 0, ny) } // 01:30 UTC on the 21st
	cw := compiled(t, `{"tz":"America/New_York"}`, `{}`, `[{"type":"set_due","due_in_days":2}]`, `[]`)
	runWorkflow(context.Background(), cw, f.event("due"))
	want := time.Date(2026, 10, 22, 0, 0, 0, 0, ny).UTC().Format(time.RFC3339)
	if len(f.ops) != 1 || !strings.Contains(f.ops[0], want) {
		t.Errorf("ops %v, want the 22nd's midnight in New York, %s", f.ops, want)
	}
}

// A rule keeps the zone it was saved in, whatever starts it; a zone OneCamp
// doesn't know is refused.
func TestARuleKeepsTheZoneItWasSavedIn(t *testing.T) {
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
	project := uuid.New()
	for _, in := range []WorkflowInput{
		{Name: "Made", TriggerType: workflowModel.TriggerTaskCreated, TriggerConfig: map[string]interface{}{"tz": "Asia/Kolkata"},
			Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}},
		{Name: "Changed", TriggerType: workflowModel.TriggerTaskFieldChanged, TriggerConfig: map[string]interface{}{"tz": "Asia/Kolkata", "fields": []interface{}{"priority"}},
			Actions: []WorkflowAction{{Type: ActionSetDue, DueInDays: days(2)}}},
	} {
		if _, err := CreateProjectRule(context.Background(), who, project, in); err != nil {
			t.Fatalf("%s: %v", in.Name, err)
		}
	}
	for _, f := range saved {
		var cfg map[string]interface{}
		_ = json.Unmarshal([]byte(f.TriggerConfigJSON), &cfg)
		if cfg["tz"] != "Asia/Kolkata" {
			t.Errorf("%s saved %s, without its zone", f.Name, f.TriggerConfigJSON)
		}
	}
	bad := WorkflowInput{Name: "Bad", TriggerType: workflowModel.TriggerTaskCreated, TriggerConfig: map[string]interface{}{"tz": "Mars/Olympus"},
		Actions: []WorkflowAction{{Type: ActionSetPriority, Priority: "high"}}}
	if _, err := CreateProjectRule(context.Background(), who, project, bad); err == nil {
		t.Error("a zone OneCamp doesn't know was saved")
	}
}

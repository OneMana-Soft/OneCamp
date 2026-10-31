package business

import (
	"context"
	"strings"
	"testing"
	"time"

	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func TestConditionsDecideOnTheTaskAsItIsNow(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	in2 := now.Add(48 * time.Hour)
	past := now.Add(-24 * time.Hour)
	maya := "11111111-1111-4111-8111-111111111111"
	task := taskFacts{Priority: "high", Category: "inProgress", CustomID: "qa-id", Assignee: maya,
		Creator: maya, Name: "Fix the SSO login", Tags: []string{"Frontend", "needs review"}, Due: &in2}
	cases := []struct {
		c    Condition
		want bool
	}{
		{Condition{Field: "priority", Op: "is", Value: "high"}, true},
		{Condition{Field: "priority", Op: "is_not", Value: "high"}, false},
		{Condition{Field: "status", Op: "is", Value: "inProgress"}, true},
		{Condition{Field: "status", Op: "is", Value: "qa-id"}, true},
		{Condition{Field: "status", Op: "is_not", Value: "done"}, true},
		{Condition{Field: "assignee", Op: "is", Value: maya}, true},
		{Condition{Field: "assignee", Op: "is_empty"}, false},
		{Condition{Field: "assignee", Op: "is_set"}, true},
		{Condition{Field: "tags", Op: "has", Value: "frontend"}, true},
		{Condition{Field: "tags", Op: "has_not", Value: "backend"}, true},
		{Condition{Field: "tags", Op: "is_empty"}, false},
		{Condition{Field: "name", Op: "contains", Value: "sso"}, true},
		{Condition{Field: "name", Op: "not_contains", Value: "sso"}, false},
		{Condition{Field: "due", Op: "within_days", Value: "3"}, true},
		{Condition{Field: "due", Op: "within_days", Value: "1"}, false},
		{Condition{Field: "due", Op: "overdue"}, false},
		{Condition{Field: "due", Op: "is_empty"}, false},
		{Condition{Field: "creator", Op: "is", Value: maya}, true},
	}
	for _, c := range cases {
		if got := c.c.holds(task, now, nil); got != c.want {
			t.Errorf("%s %s %q: %v, want %v", c.c.Field, c.c.Op, c.c.Value, got, c.want)
		}
	}
	late := task
	late.Due = &past
	if !(Condition{Field: "due", Op: "overdue"}).holds(late, now, nil) {
		t.Error("a task due yesterday is overdue")
	}
	done := late
	done.Category = "done"
	if (Condition{Field: "due", Op: "overdue"}).holds(done, now, nil) {
		t.Error("a finished task is never overdue")
	}
}

// A due date is a day (the web writes the midnight it starts, in the zone of
// whoever set it), and "overdue" means its day has passed in the rule's
// zone, as the task list's Overdue group says: a task due today is due
// today, not overdue, however late in the day.
func TestDueDatesAreDaysInTheRulesZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no zone data")
	}
	now := time.Date(2026, 10, 20, 21, 0, 0, 0, kolkata)
	day := func(d int) *time.Time {
		v := time.Date(2026, 10, 20+d, 0, 0, 0, 0, kolkata)
		return &v
	}
	overdue := Condition{Field: "due", Op: "overdue"}
	within := func(n string) Condition { return Condition{Field: "due", Op: "within_days", Value: n} }
	cases := []struct {
		name string
		c    Condition
		due  *time.Time
		want bool
	}{
		{"due today is not overdue at 21:00", overdue, day(0), false},
		{"due yesterday is overdue", overdue, day(-1), true},
		{"due today is within 0 days", within("0"), day(0), true},
		{"due tomorrow is within 1 day", within("1"), day(1), true},
		{"due in 2 days is not within 1 day", within("1"), day(2), false},
		{"due yesterday is not within 3 days", within("3"), day(-1), false},
	}
	for _, c := range cases {
		task := taskFacts{Category: "todo", Due: c.due}
		if got := c.c.holds(task, now, kolkata); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAllAndAnyAndNone(t *testing.T) {
	now := time.Now()
	task := taskFacts{Priority: "low", Category: "todo"}
	high := Condition{Field: "priority", Op: "is", Value: "high"}
	todo := Condition{Field: "status", Op: "is", Value: "todo"}
	if !(Conditions{}).hold(task, now, nil) {
		t.Error("no conditions always hold")
	}
	if (Conditions{Match: "all", Items: []Condition{high, todo}}).hold(task, now, nil) {
		t.Error("all: one fails")
	}
	if !(Conditions{Match: "any", Items: []Condition{high, todo}}).hold(task, now, nil) {
		t.Error("any: one holds")
	}
}

func TestConditionsAreCheckedWhenSaved(t *testing.T) {
	bad := []Conditions{
		{Items: []Condition{{Field: "colour", Op: "is", Value: "red"}}},
		{Items: []Condition{{Field: "priority", Op: "contains", Value: "h"}}},
		{Items: []Condition{{Field: "priority", Op: "is", Value: "someday"}}},
		{Items: []Condition{{Field: "due", Op: "within_days", Value: "soon"}}},
		{Items: []Condition{{Field: "assignee", Op: "is", Value: "maya"}}},
		{Items: []Condition{{Field: "name", Op: "contains", Value: ""}}},
		{Match: "most", Items: []Condition{{Field: "priority", Op: "is", Value: "low"}}},
		{Items: make([]Condition, MaxConditions+1)},
	}
	for i, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("%d: %+v saved", i, c)
		}
	}
	ok := Conditions{Match: "ANY", Items: []Condition{{Field: "tags", Op: "has", Value: "  Frontend "}}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Match != "any" || ok.Items[0].Value != "Frontend" {
		t.Errorf("not normalised: %+v", ok)
	}
	if err := (&Conditions{Items: []Condition{{Field: "due", Op: "within_days", Value: "400"}}}).validate(); err == nil || !strings.Contains(err.Error(), "365") {
		t.Errorf("a year at most: %v", err)
	}
}

// Every ready-made rule saves as it stands (statuses resolved later, against
// the project), and draws in a camp hue.
func TestTheReadyMadeRulesAreRulesThatSave(t *testing.T) {
	hues := map[string]bool{"sky": true, "moss": true, "sun": true, "dusk": true, "berry": true, "lake": true}
	seen := map[string]bool{}
	check := func(tpl RuleTemplate) {
		if seen[tpl.Key] || !(hues[tpl.Hue] || tpl.Hue == "agent" && tpl.NeedsAI) {
			t.Errorf("%s: key repeated or hue %q", tpl.Key, tpl.Hue)
		}
		seen[tpl.Key] = true
		cfg := map[string]interface{}{}
		for k, v := range tpl.Trigger {
			cfg[k] = v
		}
		in := WorkflowInput{Name: tpl.Name, TriggerType: tpl.TriggerType, TriggerConfig: cfg, ProjectID: uuid.NewString(),
			Conditions: tpl.Conditions, Actions: append([]WorkflowAction(nil), tpl.Actions...), ElseActions: append([]WorkflowAction(nil), tpl.ElseActions...)}
		for i := range in.Actions {
			if in.Actions[i].Type == ActionCreateTask {
				in.Actions[i].ProjectID = in.ProjectID
			}
		}
		if _, err := validate(&in); err != nil {
			t.Errorf("%s: %v", tpl.Key, err)
		}
	}
	withAgentHooks(t, nil, nil)
	for _, tpl := range Templates() {
		if tpl.NeedsAI {
			t.Errorf("%s needs AI but is offered without it", tpl.Key)
		}
		check(tpl)
	}
	withAgentHooks(t, func(context.Context, *userModels.UserInfo, AgentQuestion) (AgentAnswer, error) {
		return AgentAnswer{}, nil
	}, nil)
	seen = map[string]bool{}
	for _, tpl := range Templates() {
		check(tpl)
	}
	if len(Templates()) != len(RuleTemplates) || len(RuleTemplates) < 15 {
		t.Errorf("%d ready-made rules with AI, of %d", len(Templates()), len(RuleTemplates))
	}
}

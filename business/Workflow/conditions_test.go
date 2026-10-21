package business

import (
	"strings"
	"testing"
	"time"
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
	for _, tpl := range RuleTemplates {
		if seen[tpl.Key] || !hues[tpl.Hue] {
			t.Errorf("%s: key repeated or hue %q", tpl.Key, tpl.Hue)
		}
		seen[tpl.Key] = true
		in := WorkflowInput{Name: tpl.Name, TriggerType: tpl.TriggerType, TriggerConfig: map[string]interface{}{},
			Conditions: tpl.Conditions, Actions: append([]WorkflowAction(nil), tpl.Actions...), ElseActions: append([]WorkflowAction(nil), tpl.ElseActions...)}
		if _, err := validate(&in); err != nil {
			t.Errorf("%s: %v", tpl.Key, err)
		}
	}
	if len(RuleTemplates) < 10 {
		t.Errorf("%d ready-made rules", len(RuleTemplates))
	}
}

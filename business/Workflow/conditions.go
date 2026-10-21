package business

// Conditions: the "If" of a rule. A rule's conditions are read against the
// task as it is when the rule runs (read as the rule's owner), not against
// the event, so "when a task moves to Done, if it is high priority" asks the
// task. No conditions always hold.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// MaxConditions bounds a rule's conditions.
const MaxConditions = 10

// Condition is one test of a task: a field, how it is compared, and with
// what (a priority, a status key or id, a person's id, a tag, words, days).
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value,omitempty"`
}

// Conditions are a rule's tests, all of which (or any of which) must hold.
type Conditions struct {
	Match string      `json:"match,omitempty"` // "all" (default) or "any"
	Items []Condition `json:"items,omitempty"`
}

// taskFacts is what conditions read of a task.
type taskFacts struct {
	Priority string
	Category string // the built-in status the task counts as
	CustomID string // its own status's id, "" for none
	Assignee string // a person's id, "" for nobody
	Creator  string
	Name     string
	Tags     []string
	Due      *time.Time
}

// factsOf reads a task's facts.
func factsOf(t *dgraphStruct.DgraphTask) taskFacts {
	if t == nil {
		return taskFacts{}
	}
	s := taskStatusBusiness.Of(t)
	f := taskFacts{Priority: t.Priority, Category: s.Category, CustomID: s.CustomID, Name: t.Name}
	if t.Assignee != nil {
		f.Assignee = t.Assignee.Uuid
	}
	if t.CreatedBy != nil {
		f.Creator = t.CreatedBy.Uuid
	}
	if t.Label != nil {
		f.Tags = helpers.Tags(*t.Label)
	}
	if t.DueDate != nil && t.DueDate.Year() > 1970 {
		d := *t.DueDate
		f.Due = &d
	}
	return f
}

// The ops each field takes.
var fieldOps = map[string][]string{
	"priority": {"is", "is_not"},
	"status":   {"is", "is_not"},
	"assignee": {"is", "is_not", "is_empty", "is_set"},
	"tags":     {"has", "has_not", "is_empty"},
	"name":     {"contains", "not_contains"},
	"due":      {"is_empty", "is_set", "within_days", "overdue"},
	"creator":  {"is", "is_not"},
}

// valueless ops take no value.
var valueless = []string{"is_empty", "is_set", "overdue"}

// validate checks conditions as they are saved and tidies them.
func (c *Conditions) validate() error {
	c.Match = strings.ToLower(strings.TrimSpace(c.Match))
	switch c.Match {
	case "":
		c.Match = "all"
	case "all", "any":
	default:
		return fmt.Errorf("conditions match all or any")
	}
	if len(c.Items) > MaxConditions {
		return fmt.Errorf("a rule has at most %d conditions", MaxConditions)
	}
	for i := range c.Items {
		it := &c.Items[i]
		it.Field = strings.TrimSpace(it.Field)
		it.Op = strings.TrimSpace(it.Op)
		it.Value = strings.TrimSpace(it.Value)
		ops, ok := fieldOps[it.Field]
		if !ok {
			return fmt.Errorf("a condition can't test %q", it.Field)
		}
		if !slices.Contains(ops, it.Op) {
			return fmt.Errorf("%s can't be tested with %q", it.Field, it.Op)
		}
		if slices.Contains(valueless, it.Op) {
			it.Value = ""
			continue
		}
		if it.Value == "" {
			return fmt.Errorf("the %s condition needs a value", it.Field)
		}
		if len(it.Value) > 200 {
			return fmt.Errorf("a condition's value is at most 200 characters")
		}
		switch it.Field {
		case "priority":
			if !taskPriority.Is(it.Value) {
				return fmt.Errorf("a priority is urgent, high, medium or low")
			}
		case "assignee", "creator":
			if _, err := uuid.Parse(it.Value); err != nil {
				return fmt.Errorf("pick a person for the %s condition", it.Field)
			}
		case "due":
			n, err := strconv.Atoi(it.Value)
			if err != nil || n < 0 {
				return fmt.Errorf("due within a number of days")
			}
			if n > 365 {
				return fmt.Errorf("due within at most 365 days")
			}
		}
	}
	return nil
}

// hold says whether the conditions hold for a task now, its due date read
// as a day in loc (the rule's zone; nil is UTC).
func (c Conditions) hold(t taskFacts, now time.Time, loc *time.Location) bool {
	if len(c.Items) == 0 {
		return true
	}
	any := c.Match == "any"
	for _, it := range c.Items {
		ok := it.holds(t, now, loc)
		if any && ok {
			return true
		}
		if !any && !ok {
			return false
		}
	}
	return !any
}

// holds says whether one condition holds for a task now. A due date is a
// day: "overdue" once its day has passed in loc, as the task list's Overdue
// group says, and "within N days" from today to N days on.
func (c Condition) holds(t taskFacts, now time.Time, loc *time.Location) bool {
	switch c.Field {
	case "priority":
		return is(c.Op, t.Priority == c.Value)
	case "status":
		// A built-in key matches the category (a project's own statuses that
		// count as it included); an id matches that status only.
		match := t.CustomID == c.Value
		if taskStatusBusiness.IsBuiltIn(c.Value) {
			match = t.Category == c.Value
		}
		return is(c.Op, match)
	case "assignee":
		switch c.Op {
		case "is_empty":
			return t.Assignee == ""
		case "is_set":
			return t.Assignee != ""
		}
		return is(c.Op, t.Assignee == c.Value)
	case "creator":
		return is(c.Op, t.Creator == c.Value)
	case "tags":
		if c.Op == "is_empty" {
			return len(t.Tags) == 0
		}
		has := slices.ContainsFunc(t.Tags, func(tag string) bool { return strings.EqualFold(tag, c.Value) })
		return has == (c.Op == "has")
	case "name":
		has := strings.Contains(strings.ToLower(t.Name), strings.ToLower(c.Value))
		return has == (c.Op == "contains")
	case "due":
		switch c.Op {
		case "is_empty":
			return t.Due == nil
		case "is_set":
			return t.Due != nil
		case "overdue":
			return t.Due != nil && calendarDays(now, *t.Due, loc) < 0 && !taskStatusBusiness.IsClosed(t.Category)
		case "within_days":
			n, _ := strconv.Atoi(c.Value)
			if t.Due == nil {
				return false
			}
			d := calendarDays(now, *t.Due, loc)
			return d >= 0 && d <= n
		}
	}
	return false
}

// is answers a match for "is" and its opposite for "is_not".
func is(op string, match bool) bool {
	if op == "is_not" {
		return !match
	}
	return match
}

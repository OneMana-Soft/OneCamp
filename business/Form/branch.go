package business

// Branching: a question, or a whole section, shows only when an earlier
// answer says so ("show if Team is Design"). The server decides what was
// shown from the answers themselves, never from what the page says it
// showed: a hidden question is never required, and an answer to one is
// dropped. lib/forms/branch.ts is the page's twin, and both run the same
// cases.

import (
	"fmt"
	"slices"
	"strings"
)

// ShowIf is when a question or a section shows: an earlier question's answer
// is, isn't, or is any of some choices, or is answered at all.
type ShowIf struct {
	Question string   `json:"question"`
	Op       string   `json:"op"`
	Values   []string `json:"values,omitempty"`
}

// The ways a ShowIf reads an answer.
const (
	ShowIs       = "is"
	ShowIsNot    = "is_not"
	ShowAnyOf    = "any_of"
	ShowAnswered = "answered"
)

// checkShowIf says what's wrong with item i's condition, given the items
// before it, or "". A condition reads an earlier question only, so a form
// can't loop; "is", "is not" and "any of" read a choice or a tick, and
// "answered" reads any question. Pure.
func checkShowIf(c *ShowIf, before []Field, n int) string {
	var on *Field
	for j := range before {
		if before[j].Id == c.Question && before[j].Type != Section {
			on = &before[j]
		}
	}
	if on == nil {
		return fmt.Sprintf("Item %d shows on an answer that has to come before it.", n)
	}
	choices := on.Options
	if on.Type == Checkbox {
		choices = []string{"Yes", "No"}
	}
	switch c.Op {
	case ShowAnswered:
		c.Values = nil
		return ""
	case ShowIs, ShowIsNot, ShowAnyOf:
		if on.Type != Select && on.Type != MultiSelect && on.Type != Checkbox {
			return fmt.Sprintf("Item %d can only show when “%s” is answered: it has no choices.", n, on.Label)
		}
	default:
		return fmt.Sprintf("Item %d shows on a condition this form can't read.", n)
	}
	vals := make([]string, 0, len(c.Values))
	for _, v := range c.Values {
		v = tidy(v)
		if v == "" || slices.Contains(vals, v) {
			continue
		}
		if !slices.Contains(choices, v) {
			return fmt.Sprintf("Item %d shows on “%s”, which isn't a choice of “%s”.", n, v, on.Label)
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 || (c.Op != ShowAnyOf && len(vals) > 1) {
		return fmt.Sprintf("Item %d needs the choice it shows on.", n)
	}
	c.Values = vals
	return ""
}

// holds says whether a condition holds for the answers given so far: a
// question not shown, or left blank, is unanswered. A multi-select is the
// choice when it has it among its choices. Pure.
func holds(c *ShowIf, got map[string]Answer) bool {
	if c == nil {
		return true
	}
	a, answered := got[c.Question]
	has := func(v string) bool {
		if !answered {
			return false
		}
		if len(a.Values) > 0 {
			return slices.Contains(a.Values, v)
		}
		return a.Value == v
	}
	switch c.Op {
	case ShowAnswered:
		return answered && strings.TrimSpace(a.Value) != ""
	case ShowIs:
		return len(c.Values) == 1 && has(c.Values[0])
	case ShowIsNot:
		return len(c.Values) == 1 && !has(c.Values[0])
	case ShowAnyOf:
		return slices.ContainsFunc(c.Values, has)
	}
	return false
}

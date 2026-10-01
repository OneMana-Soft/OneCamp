package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestRuleTargetTreatsNoChangeAsNone(t *testing.T) {
	rules := map[string]string{"a": "done", "b": "_none", "c": "  ", "d": " inReview "}
	for trigger, want := range map[string]string{"a": "done", "b": "", "c": "", "d": "inReview", "missing": ""} {
		if got := ruleTarget(rules, trigger); got != want {
			t.Errorf("%s: got %q, want %q", trigger, got, want)
		}
	}
	if ruleTarget(nil, "a") != "" {
		t.Error("no rules is no target")
	}
}

// Built-in keys resolve without the database, which is all this can check
// here; names, labels and custom statuses are in the integration test.
func TestNormalizeAutomationRulesKeepsKeysAndDropsNoChange(t *testing.T) {
	got, err := NormalizeAutomationRules(context.Background(), uuid.New(), map[string]string{
		"pr_merged": "done", "pr_opened": "_none", "issue_opened": "", "approved": " inReview ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["pr_merged"] != "done" || got["approved"] != "inReview" {
		t.Fatalf("%v", got)
	}
}

func TestStatusFromIssueOnlyActsOnOpenOrClosed(t *testing.T) {
	cases := []struct{ state, reason, current, want string }{
		{"open", "", "inProgress", ""},
		{"open", "", "inReview", ""},
		{"open", "", "todo", ""},
		{"open", "", "done", "todo"},
		{"open", "", "canceled", "todo"},
		{"closed", "completed", "inProgress", "done"},
		{"closed", "not_planned", "inReview", "canceled"},
		{"closed", "completed", "canceled", ""},
		{"closed", "not_planned", "done", ""},
	}
	for _, c := range cases {
		if got := statusFromIssue(c.state, c.reason, c.current); got != c.want {
			t.Errorf("%s/%s with task %s: got %q, want %q", c.state, c.reason, c.current, got, c.want)
		}
	}
}

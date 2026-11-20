package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// An issue opened on a linked repository waits in triage only where the link
// sets no status for a new issue: a rule that does says where the team wants
// it, so the task is the team's work at once (linear-21 H1).
func TestAnIssueWaitsInTriageOnlyWhereNoRuleSetsItsStatus(t *testing.T) {
	link := func(rules string) *githubLinkModel.GitHubLink {
		if rules == "" {
			return &githubLinkModel.GitHubLink{}
		}
		return &githubLinkModel.GitHubLink{AutomationRules: &rules}
	}
	todo := dgraphStruct.TASK_STATUS_TODO
	cases := []struct {
		name   string
		rules  string
		status string
		waits  bool
	}{
		{"no rules", "", todo, true},
		{"rules for other events", `{"pr_merged":"done"}`, todo, true},
		{"no change on a new issue", `{"issue_opened":"_none"}`, todo, true},
		{"rules that can't be read", `{"issue_opened":`, todo, true},
		{"a status for a new issue", `{"issue_opened":"inProgress"}`, "inProgress", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, waits := issueOpened(link(c.rules))
			if status != c.status || waits != c.waits {
				t.Errorf("status %q waits %v, want %q %v", status, waits, c.status, c.waits)
			}
		})
	}
}

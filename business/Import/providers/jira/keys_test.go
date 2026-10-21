package jira

import (
	"encoding/json"
	"testing"
)

// I1: an issue keeps its key (ENG-7), which the import gives the task.
func TestAnIssueKeepsItsKey(t *testing.T) {
	var iss jiraIssue
	if err := json.Unmarshal([]byte(`{"key":"ENG-7","fields":{"summary":"S","status":{"name":"To Do"},"issuetype":{"name":"Bug"}}}`), &iss); err != nil {
		t.Fatal(err)
	}
	if got := buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{}).SourceKey; got != "ENG-7" {
		t.Fatalf("source key %q", got)
	}
}

package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// I1: a Jira issue's original estimate (seconds) comes across in minutes.
func TestAnIssueKeepsItsEstimate(t *testing.T) {
	if !strings.Contains(issueFields, "timeoriginalestimate") {
		t.Fatalf("issues aren't asked for their estimate: %s", issueFields)
	}
	var iss jiraIssue
	if err := json.Unmarshal([]byte(`{"key":"ENG-1","fields":{"summary":"Ship","status":{"name":"To Do"},"timeoriginalestimate":7200}}`), &iss); err != nil {
		t.Fatal(err)
	}
	if got := buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{}).EstimateMinutes; got != 120 {
		t.Fatalf("estimate %d minutes, want 120", got)
	}
}

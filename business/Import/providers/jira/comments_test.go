package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// I1: a Jira issue carries its comment total; the import asks for comments
// only when there are some (it asked for none: CommentCount was never set).
func TestAnIssueSaysHowManyCommentsItHas(t *testing.T) {
	if !strings.Contains(","+issueFields+",", ",comment,") {
		t.Fatalf("a search doesn't ask for comments: %s", issueFields)
	}
	var iss jiraIssue
	if err := json.Unmarshal([]byte(`{"key":"ENG-7","fields":{"summary":"S","status":{"name":"To Do"},"issuetype":{"name":"Bug"},"comment":{"total":3,"comments":[{"author":{"accountId":"a"}}]}}}`), &iss); err != nil {
		t.Fatal(err)
	}
	if got := buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{}).CommentCount; got != 3 {
		t.Fatalf("comment count %d, want 3", got)
	}
}

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

// A comment Jira keeps from some of the people who see its issue, to a
// project role or a group, or a service desk's internal note (jsdPublic on
// Cloud, the sd.public.comment property on both), is marked to stay in Jira;
// one everyone who sees the issue reads isn't.
func TestACommentJiraKeepsFromSomePeopleIsMarked(t *testing.T) {
	var resp jiraCommentsResp
	if err := json.Unmarshal([]byte(`{"total":7,"comments":[
		{"id":"1","body":"open"},
		{"id":"2","jsdPublic":true},
		{"id":"3","visibility":{"type":"role","value":"Administrators","identifier":"Administrators"}},
		{"id":"4","visibility":{"type":"group","value":"hr","identifier":"276f955c"}},
		{"id":"5","jsdPublic":false},
		{"id":"6","properties":[{"key":"sd.public.comment","value":{"internal":true}}]},
		{"id":"7","properties":[{"key":"sd.public.comment","value":{"internal":false}}]}]}`), &resp); err != nil {
		t.Fatal(err)
	}
	role, note := "a project role or group", "an internal note"
	want := map[string]string{"1": "", "2": "", "3": role, "4": role, "5": note, "6": note, "7": ""}
	for _, c := range resp.Comments {
		if got := c.keptBy(); got != want[c.ID] {
			t.Errorf("comment %s: kept by %q, want %q", c.ID, got, want[c.ID])
		}
	}
}

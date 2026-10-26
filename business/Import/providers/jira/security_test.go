package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// An issue with a security level is one Jira lets only some people see: it
// comes in restricted, to its reporter besides its creator and assignee. One
// without a level comes in as every other issue does. The field reads the
// same on Cloud's REST v3 and Data Center's REST v2.
func TestAnIssueWithASecurityLevelComesInRestricted(t *testing.T) {
	if !strings.Contains(issueFields, "security") {
		t.Fatalf("issues aren't asked for their security level: %s", issueFields)
	}
	read := func(raw string) jiraIssue {
		t.Helper()
		var iss jiraIssue
		if err := json.Unmarshal([]byte(raw), &iss); err != nil {
			t.Fatal(err)
		}
		return iss
	}
	locked := read(`{"key":"HR-4","fields":{"summary":"Pay bands","status":{"name":"To Do"},
		"security":{"self":"https://x.atlassian.net/rest/api/3/securitylevel/10001","id":"10001","name":"People team","description":"HR only"},
		"reporter":{"accountId":"557058:rae"},"creator":{"accountId":"557058:cy"},"assignee":{"accountId":"557058:al"}}}`)
	st := buildSourceTask(&locked, "HR", "https://x.atlassian.net", siteFields{})
	if st.Restricted == "" || len(st.Audience) != 1 || st.Audience[0] != "557058:rae" || st.CreatedBy != "557058:cy" {
		t.Fatalf("an issue with a security level: restricted %q, audience %v, creator %q", st.Restricted, st.Audience, st.CreatedBy)
	}
	if strings.Contains(st.Restricted, "People team") {
		t.Errorf("the report would name the level: %q", st.Restricted)
	}

	open := read(`{"key":"HR-5","fields":{"summary":"Desk move","status":{"name":"To Do"},"security":null,"reporter":{"accountId":"557058:rae"}}}`)
	if st := buildSourceTask(&open, "HR", "https://x.atlassian.net", siteFields{}); st.Restricted != "" || st.Audience != nil {
		t.Errorf("an issue with no level comes in restricted %q to %v", st.Restricted, st.Audience)
	}
}

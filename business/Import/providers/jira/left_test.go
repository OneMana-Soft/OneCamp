package jira

import (
	"encoding/json"
	"testing"
)

// An inactive Jira account is someone who left, never invited; an
// account Jira says nothing about is still there.
func TestAnInactiveJiraAccountLeft(t *testing.T) {
	var users []jiraUser
	if err := json.Unmarshal([]byte(`[{"accountId":"a","displayName":"Ada","active":false},{"accountId":"b","displayName":"Bo","active":true},{"accountId":"c","displayName":"Cy"}]`), &users); err != nil {
		t.Fatal(err)
	}
	if !users[0].source().Left || users[1].source().Left || users[2].source().Left {
		t.Errorf("left: %v %v %v, want true false false", users[0].source().Left, users[1].source().Left, users[2].source().Left)
	}
}

package jira

import (
	"encoding/json"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// An issue's duedate is a day with no time, so it arrives as a calendar
// day (written where the team is), never as midnight UTC.
func TestAnIssuesDueDateArrivesAsADay(t *testing.T) {
	var iss jiraIssue
	if err := json.Unmarshal([]byte(`{"key":"ENG-7","fields":{"summary":"S","status":{"name":"To Do"},"issuetype":{"name":"Bug"},"duedate":"2026-10-22"}}`), &iss); err != nil {
		t.Fatal(err)
	}
	st := buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{})
	if st.DueDay != (importProvider.Day{Year: 2026, Month: time.October, Day: 22}) || st.DueDate != nil {
		t.Errorf("duedate: day %v, instant %v; want the day 2026-10-22 and no instant", st.DueDay, st.DueDate)
	}
}

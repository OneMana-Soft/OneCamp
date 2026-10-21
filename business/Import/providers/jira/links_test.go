package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

// I1: Jira's "blocks" links come across as dependencies (they were lost: a
// search didn't ask for issuelinks).
func TestAnIssueKeepsWhatBlocksIt(t *testing.T) {
	if !strings.Contains(","+issueFields+",", ",issuelinks,") {
		t.Fatalf("a search doesn't ask for links: %s", issueFields)
	}
	var iss jiraIssue
	raw := `{"key":"ENG-7","fields":{"summary":"S","status":{"name":"To Do"},"issuetype":{"name":"Bug"},"issuelinks":[
	  {"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"inwardIssue":{"key":"ENG-3"}},
	  {"type":{"name":"Blocks","inward":"is blocked by","outward":"blocks"},"outwardIssue":{"key":"ENG-9"}},
	  {"type":{"name":"Relates","inward":"relates to","outward":"relates to"},"outwardIssue":{"key":"ENG-4"}}]}}`
	if err := json.Unmarshal([]byte(raw), &iss); err != nil {
		t.Fatal(err)
	}
	deps := buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{}).Dependencies
	// ENG-3 blocks this one; this one blocking ENG-9 is ENG-9's to say; "relates" isn't waiting.
	if len(deps) != 1 || deps[0].BlockerSourceID != "ENG-3" || deps[0].Kind != "fs" {
		t.Fatalf("dependencies %+v", deps)
	}
}

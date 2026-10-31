package jira

import (
	"encoding/json"
	"testing"
)

// A project's statuses come with their category (To Do, In Progress,
// Done: new, indeterminate, done), so a status with a name of the team's own
// ("Ready for QA") is proposed by its category instead of as To do.
func TestAProjectsStatusesComeWithTheirCategory(t *testing.T) {
	var resp []jiraTypeStatuses
	if err := json.Unmarshal([]byte(`[
	  {"name":"Story","statuses":[
	    {"name":"To Do","statusCategory":{"key":"new"}},
	    {"name":"Ready for QA","statusCategory":{"key":"indeterminate"}},
	    {"name":"Shipped","statusCategory":{"key":"done"}}]},
	  {"name":"Bug","statuses":[{"name":"ready for qa","statusCategory":{"key":"indeterminate"}}]}
	]`), &resp); err != nil {
		t.Fatal(err)
	}
	names, kinds := statusesOf(resp)
	if len(names) != 3 {
		t.Errorf("names %v, want each status once", names)
	}
	for name, want := range map[string]string{"ready for qa": "indeterminate", "shipped": "done", "to do": "new"} {
		if kinds[name] != want {
			t.Errorf("kinds[%q] = %q, want %q (all %v)", name, kinds[name], want, kinds)
		}
	}
	d := New().DefaultStatusMap()
	if d["indeterminate"] != "inProgress" || d["new"] == "" || d["done"] != "done" {
		t.Errorf("the default map has no entry for a category: %v", d)
	}
}

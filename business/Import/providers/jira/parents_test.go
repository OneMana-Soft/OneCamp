package jira

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// parented is an issue with the given parent ("" for none), as a search
// gives it.
func parented(t *testing.T, key, parent string, subtask bool) importProvider.SourceTask {
	t.Helper()
	p := ""
	if parent != "" {
		p = `,"parent":` + parent
	}
	raw := fmt.Sprintf(`{"key":%q,"fields":{"summary":"Pay with a card","status":{"name":"To Do"},"issuetype":{"name":"Story","subtask":%t}%s}}`, key, subtask, p)
	var iss jiraIssue
	if err := json.Unmarshal([]byte(raw), &iss); err != nil {
		t.Fatal(err)
	}
	return buildSourceTask(&iss, "ENG", "https://x.atlassian.net", siteFields{})
}

func epic(key, summary string) string {
	return fmt.Sprintf(`{"id":"1","key":%q,"fields":{"summary":%q,"status":{"name":"In Progress"},"issuetype":{"name":"Epic","subtask":false}}}`, key, summary)
}

// parentOf is the task's parent value, and its field, or nil.
func parentOf(st importProvider.SourceTask) *importProvider.SourceFieldValue {
	for i := range st.Fields {
		if strings.HasPrefix(st.Fields[i].Field.SourceID, parentFieldPrefix) {
			return &st.Fields[i]
		}
	}
	return nil
}

// An issue under an epic holds the epic in the project's Epic field, named
// by key and then summary, so two epics of one name stay two and the key
// still opens the epic's task. A sub-task's parent is its task here, so it
// holds none; a parent that comes without its fields is named by key, in a
// Parent field. Before, an epic's issues came with nothing that said which
// epic they were under.
func TestAnIssueUnderAnEpicHoldsIt(t *testing.T) {
	v := parentOf(parented(t, "ENG-9", epic("ENG-1", "Checkout"), false))
	want := importProvider.SourceFieldValue{
		Field: importProvider.SourceField{SourceID: "jira-parent:epic", Name: "Epic", Type: importProvider.FieldSelect,
			Options: []importProvider.SourceOption{{SourceID: "ENG-1", Label: "ENG-1 Checkout"}}},
		Value: "ENG-1",
		Text:  "ENG-1 Checkout",
	}
	if v == nil || !reflect.DeepEqual(*v, want) {
		t.Fatalf("under an epic:\n got %+v\nwant %+v", v, want)
	}
	other := parentOf(parented(t, "ENG-10", epic("ENG-2", "Checkout"), false))
	if other == nil || other.Field.SourceID != v.Field.SourceID || other.Text == v.Text {
		t.Errorf("two epics of one name stay two options of one field: %+v %+v", v, other)
	}
	if v := parentOf(parented(t, "ENG-11", epic("ENG-9", "Pay with a card"), true)); v != nil {
		t.Errorf("a sub-task holds no parent: %+v", v)
	}
	if v := parentOf(parented(t, "ENG-12", "", false)); v != nil {
		t.Errorf("an issue with no parent holds none: %+v", v)
	}
	bare := parentOf(parented(t, "ENG-13", `{"id":"7","key":"ENG-7"}`, false))
	if bare == nil || bare.Field.Name != "Parent" || bare.Field.SourceID != "jira-parent:parent" || bare.Text != "ENG-7" || bare.Value != "ENG-7" {
		t.Errorf("a parent without its fields, by key: %+v", bare)
	}
}

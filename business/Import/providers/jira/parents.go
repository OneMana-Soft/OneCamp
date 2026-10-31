package jira

import (
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// An issue's parent above it in Jira's hierarchy (jira-16): an epic, or an
// epic's own parent on a plan with more levels. Sub-tasks here are one level
// deep (business/Import task_worker.go), so the epic can't hold its stories
// and they their sub-tasks too. The epic comes as its own task, with its key,
// and each issue under it holds it in a select field named for the parent's
// type ("Epic"), the way Linear's milestones come (until the work hierarchy,
// #96, lands).

// parentFieldPrefix starts a parent field's source id: a project has one such
// field for each type of parent.
const parentFieldPrefix = "jira-parent:"

// parentValue is the issue's parent as its value in that field: the option
// names the parent by key and then summary, so two epics of one name stay
// two (options are told apart by name), and the key still opens its task.
// nil for an issue with no parent, and for a sub-task, whose parent is its
// task here.
func parentValue(iss *jiraIssue) []importProvider.SourceFieldValue {
	p := iss.Fields.Parent
	if p == nil || p.Key == "" || iss.Fields.IssueType.Subtask {
		return nil
	}
	kind, label := "Parent", p.Key
	if p.Fields != nil {
		if n := strings.TrimSpace(p.Fields.IssueType.Name); n != "" {
			kind = n
		}
		if s := strings.TrimSpace(p.Fields.Summary); s != "" {
			label = p.Key + " " + s
		}
	}
	field := importProvider.SourceField{
		SourceID: parentFieldPrefix + strings.ToLower(kind),
		Name:     kind,
		Type:     importProvider.FieldSelect,
		Options:  []importProvider.SourceOption{{SourceID: p.Key, Label: label}},
	}
	return []importProvider.SourceFieldValue{{Field: field, Value: p.Key, Text: label}}
}

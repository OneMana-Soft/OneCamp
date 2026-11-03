package jira

import (
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// projectPlan is a Jira project's plan as the import takes it (linear-15):
// its lead leads it. Jira keeps no dates, status, priority or labels on a
// project, so a project Jira shows no lead for has no plan. Pure.
func projectPlan(pr jiraProject) *importProvider.SourcePlan {
	if pr.Lead == nil || pr.Lead.AccountID == "" {
		return nil
	}
	return &importProvider.SourcePlan{LeadID: pr.Lead.AccountID}
}

// issueStatusSince is when an issue entered the status it comes in, from
// Jira's own times: a done issue when it was resolved (or, with no
// resolution, when its status's category became done), any other when its
// status's category last changed. A reopened issue keeps its old resolution
// date, which no longer says where it stands, so only a done issue reads it.
// Pure.
func issueStatusSince(iss *jiraIssue) *time.Time {
	if iss.done() {
		if t := parseJiraTime(iss.Fields.ResolutionDate); !t.IsZero() {
			return &t
		}
	}
	return importProvider.GivenTime(parseJiraTime(iss.Fields.StatusCategoryChanged))
}

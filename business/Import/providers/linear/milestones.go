package linear

import (
	"context"
	"sort"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// A Linear project's milestones (L-2) come as tasks of the built-in type
// Milestone, each due on its target date, and the issues in one say which in
// the project's "Milestone" select field, its options the project's
// milestones in Linear's order. Milestones as their own thing are B5's
// (design/projects-vs-asana-jira/PROGRAM.md).

// milestonesQuery reads every project's milestones in one list, rather than
// a list per project.
const milestonesQuery = `
query ($cursor: String) {
  projectMilestones(first: 250, after: $cursor) {
    nodes { id name description targetDate sortOrder status createdAt updatedAt project { id } }
    pageInfo { hasNextPage endCursor }
  }
}`

// milestoneFieldID is the source id of a project's Milestone field. Fields
// are a project's own (business/Import fieldKey), so each project's field
// holds its own milestones.
const milestoneFieldID = "project-milestone"

// milestoneTaskPrefix marks a milestone's task among the import's source ids,
// so its mapping and the report say what it was.
const milestoneTaskPrefix = "milestone:"

type linearMilestone struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"` // Markdown, or none
	TargetDate  *string   `json:"targetDate"`  // YYYY-MM-DD: a day with no time
	SortOrder   float64   `json:"sortOrder"`
	Status      string    `json:"status"` // done, next, overdue or unstarted
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Project     struct {
		ID string `json:"id"`
	} `json:"project"`
}

// fetchMilestones reads every project's milestones, a page at a time.
func (p *Provider) fetchMilestones(ctx context.Context, tok string) ([]linearMilestone, error) {
	return rootList[linearMilestone](ctx, p, tok, milestonesQuery, "projectMilestones")
}

// milestonesByProject is each project's milestones, in Linear's order.
func milestonesByProject(ms []linearMilestone) map[string][]linearMilestone {
	out := map[string][]linearMilestone{}
	for _, m := range ms {
		out[m.Project.ID] = append(out[m.Project.ID], m)
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool { return list[i].SortOrder < list[j].SortOrder })
	}
	return out
}

// milestoneField is a project's Milestone select field, its options the
// milestones given, in their order.
func milestoneField(ms []linearMilestone) importProvider.SourceField {
	f := importProvider.SourceField{SourceID: milestoneFieldID, Name: "Milestone", Type: importProvider.FieldSelect}
	for _, m := range ms {
		f.Options = append(f.Options, importProvider.SourceOption{SourceID: m.ID, Label: m.Name})
	}
	return f
}

// milestoneFields are the fields a project gets from its milestones: its
// Milestone field, when it has any.
func milestoneFields(ms []linearMilestone) []importProvider.SourceField {
	if len(ms) == 0 {
		return nil
	}
	return []importProvider.SourceField{milestoneField(ms)}
}

// milestoneValue is an issue's milestone as its value in the Milestone field.
func milestoneValue(m *linearMilestone) []importProvider.SourceFieldValue {
	if m == nil {
		return nil
	}
	return []importProvider.SourceFieldValue{{Field: milestoneField([]linearMilestone{*m}), Value: m.ID, Text: m.Name}}
}

// task is the milestone as a task of the built-in type Milestone, due on its
// target date, and done when Linear marks it done.
func (m linearMilestone) task(projectID string) importProvider.SourceTask {
	st := importProvider.SourceTask{
		SourceID:        milestoneTaskPrefix + m.ID,
		ProjectSourceID: projectID,
		Type:            importProvider.TaskMilestone,
		Name:            truncate(m.Name, 256),
		Description:     markdownToHTML(m.Description),
		Created:         m.CreatedAt,
		Updated:         m.UpdatedAt,
		Completed:       m.Status == "done",
	}
	if m.TargetDate != nil {
		st.DueDay, _ = importProvider.ParseDay(*m.TargetDate)
	}
	return st
}

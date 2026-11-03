package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// linearPlan is a project's plan as projectsQuery reads it (linear-15): its
// lead, its start and target dates with the resolution each was chosen at,
// its priority (0 none, 1 urgent to 4 low), its status and its labels.
type linearPlan struct {
	Lead *struct {
		ID string `json:"id"`
	} `json:"lead"`
	StartDate            *string `json:"startDate"`
	StartDateResolution  *string `json:"startDateResolution"`
	TargetDate           *string `json:"targetDate"`
	TargetDateResolution *string `json:"targetDateResolution"`
	Priority             int     `json:"priority"`
	// Status is the project's ProjectStatus; Project.state is deprecated
	// (schema_test.go keeps the import off deprecated fields).
	Status struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"status"`
	// Labels is the first page of its labels; allLabels reads the rest.
	Labels struct {
		Nodes []linearLabelNode `json:"nodes"`
		Page  pageInfo          `json:"pageInfo"`
	} `json:"labels"`
	// unread is what of the plan Linear didn't give, in the report's words
	// (SourcePlan.Unread).
	unread []string
}

type linearLabelNode struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// projectLabelsQuery is a project's labels after the first page its projects
// query gives.
const projectLabelsQuery = `
query ($id: String!, $cursor: String) {
  project(id: $id) {
    labels(first: 100, after: $cursor) {
      nodes { name color }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// allLabels reads a project's labels after the first page, a page at a
// time, following Linear's cursor (L2: it took 20 and dropped the rest
// without a word). A page Linear doesn't give leaves the rest out, and the
// plan says so for the import's report.
func (p *Provider) allLabels(ctx context.Context, tok, projectID string, l *linearPlan) {
	if !l.Labels.Page.HasNextPage {
		return
	}
	err := p.listPages(ctx, tok, "project", projectID, projectLabelsQuery, "labels", l.Labels.Page.EndCursor, func(raw json.RawMessage) error {
		var n linearLabelNode
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		l.Labels.Nodes = append(l.Labels.Nodes, n)
		return nil
	})
	if err != nil {
		l.unread = append(l.unread, fmt.Sprintf("its labels after the first %d (Linear didn't give them: %v)", len(l.Labels.Nodes), err))
	}
}

// source is the plan as the import takes it. Pure.
func (l linearPlan) source() *importProvider.SourcePlan {
	p := &importProvider.SourcePlan{
		Start:  linearWhen(l.StartDate, l.StartDateResolution),
		Target: linearWhen(l.TargetDate, l.TargetDateResolution),
		Status: importProvider.SourcePlanStatus{Category: planCategories[l.Status.Type], Name: l.Status.Name, Color: l.Status.Color},
	}
	if l.Lead != nil {
		p.LeadID = l.Lead.ID
	}
	if l.Priority >= 1 && l.Priority <= len(planPriorities) {
		p.Priority = planPriorities[l.Priority-1]
	}
	for _, n := range l.Labels.Nodes {
		p.Labels = append(p.Labels, importProvider.SourceLabel{Name: n.Name, Color: n.Color})
	}
	p.Unread = append(p.Unread, l.unread...)
	return p
}

// planPriorities is Linear's priorities 1 to 4 by the names its issues'
// priorities come as (DefaultPriorityMap).
var planPriorities = []string{"urgent", "high", "medium", "low"}

// planCategories is the category each of Linear's project status types falls
// in.
var planCategories = map[string]string{
	"backlog": "planned", "planned": "planned", "started": "in_progress",
	"paused": "paused", "completed": "done", "canceled": "cancelled",
}

// linearWhen is a project date at its resolution: Linear's halfYear is a
// half, and a date with none is a day.
func linearWhen(date, resolution *string) *importProvider.SourceWhen {
	if date == nil {
		return nil
	}
	d, ok := importProvider.ParseDay(*date)
	if !ok {
		return nil
	}
	r := "day"
	if resolution != nil {
		switch *resolution {
		case "month", "quarter", "year":
			r = *resolution
		case "halfYear":
			r = "half"
		}
	}
	return &importProvider.SourceWhen{Day: d, Resolution: r}
}

// issueStatusSince is when an issue entered the state it comes in, from
// Linear's own times: when it was completed or cancelled, or, while it's
// started, when it started. Pure.
func issueStatusSince(iss linearIssue) *time.Time {
	switch {
	case iss.CompletedAt != nil:
		return iss.CompletedAt
	case iss.CanceledAt != nil:
		return iss.CanceledAt
	case iss.StateType == "started":
		return iss.StartedAt
	}
	return nil
}

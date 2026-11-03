package asana

import (
	"fmt"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// asanaStatus is a kind of project status update as the plan takes it: the
// category it falls in and Asana's own name for it.
type asanaStatus struct{ category, name string }

// projectStatuses is each kind of status update an Asana project posts.
var projectStatuses = map[string]asanaStatus{
	"on_track":  {"in_progress", "On track"},
	"at_risk":   {"in_progress", "At risk"},
	"off_track": {"in_progress", "Off track"},
	"on_hold":   {"paused", "On hold"},
	"complete":  {"done", "Complete"},
}

// plan is the project's plan as the import takes it (linear-15): its owner
// leads it, its start and due days come as days, and its latest status
// update's kind falls in its category with Asana's name for it. A completed
// project is done whatever its last update said, and a kind the import
// doesn't know is named for the report. Asana keeps no priority or labels on
// a project. nil when it keeps none of it. Pure.
func (pr asanaProject) plan() *importProvider.SourcePlan {
	p := &importProvider.SourcePlan{Start: importProvider.OnDay(pr.StartOn), Target: importProvider.OnDay(pr.DueOn)}
	if pr.Owner != nil {
		p.LeadID = pr.Owner.GID
	}
	kind := ""
	if pr.StatusUpdate != nil {
		kind = pr.StatusUpdate.StatusType
	}
	if pr.Completed {
		kind = "complete"
	}
	if s, ok := projectStatuses[kind]; ok {
		p.Status = importProvider.SourcePlanStatus{Category: s.category, Name: s.name}
	} else if kind != "" {
		p.Unread = append(p.Unread, fmt.Sprintf("its status, %q (a kind Asana gave that the import doesn't know)", kind))
	}
	if p.LeadID == "" && p.Start == nil && p.Target == nil && p.Status == (importProvider.SourcePlanStatus{}) && len(p.Unread) == 0 {
		return nil
	}
	return p
}

// taskStatusSince is when a task entered the state it comes in, from Asana's
// own times: when it was completed. Asana keeps no time a task entered its
// section, so an open task has none. Pure.
func taskStatusSince(t *asanaTask) *time.Time {
	if !t.Completed {
		return nil
	}
	at, _ := time.Parse(time.RFC3339, t.CompletedAt)
	return importProvider.GivenTime(at)
}

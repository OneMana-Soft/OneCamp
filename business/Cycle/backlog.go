package business

// The backlog (B3, STEP4_PLAN §4 #9 and STEP5 spec F; Jira's backlog): a
// project's open cycles as containers, each with its tasks, their estimate
// and the project's capacity (what its recent cycles finished), then the
// open top-level tasks in no open cycle, counted and totalled over all of
// them. Moving a task in or out is SetTask (POST /task/cycle); starting the
// next cycle is Start.

import (
	"context"
	"errors"
	"sort"
	"time"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// OpenShown is how many open cycles the backlog shows: the current one and
// the next two, as Linear keeps them ready.
const OpenShown = 3

// PlanCycle is an open cycle as a container: its tasks and their estimate.
type PlanCycle struct {
	View
	Tasks           []*dgraphStruct.DgraphTask `json:"tasks"`
	EstimateMinutes int                        `json:"estimate_minutes"`
}

// Backlog is the backlog tab's answer.
type Backlog struct {
	Cycles []PlanCycle                `json:"cycles"`
	Tasks  []*dgraphStruct.DgraphTask `json:"tasks"`
	// Count and EstimateMinutes are every task in the backlog; Tasks may be
	// a page of them.
	Count           int `json:"count"`
	EstimateMinutes int `json:"estimate_minutes"`
	// CapacityMinutes is what the project's latest completed cycles finished
	// on average, in estimated minutes; none until one has.
	CapacityMinutes *int `json:"capacity_minutes,omitempty"`
}

// openCycles is the cycles not yet completed, by number, at most OpenShown.
func openCycles(cycles []cycleModel.Cycle) []cycleModel.Cycle {
	var out []cycleModel.Cycle
	for _, c := range cycles {
		if c.CompletedAt == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	if len(out) > OpenShown {
		out = out[:OpenShown]
	}
	return out
}

func minutesOf(t *dgraphStruct.DgraphTask) int {
	if t.EstimateMinutes != nil && *t.EstimateMinutes > 0 {
		return *t.EstimateMinutes
	}
	return 0
}

// Assemble puts the backlog together from the project's cycles, their
// members and the graph's read. Pure.
func Assemble(cycles []cycleModel.Cycle, members map[uuid.UUID][]string, read *projectDomain.BacklogRead, now time.Time) Backlog {
	byID := make(map[string]*dgraphStruct.DgraphTask, len(read.Planned))
	progress := make(map[string]TaskProgress, len(read.Planned))
	for _, t := range read.Planned {
		byID[t.Uuid] = t
		progress[t.Uuid] = progressOf(t)
	}
	b := Backlog{Cycles: []PlanCycle{}, Tasks: read.Backlog, Count: read.Count, EstimateMinutes: read.Minutes}
	if b.Tasks == nil {
		b.Tasks = []*dgraphStruct.DgraphTask{}
	}
	for _, c := range openCycles(cycles) {
		pc := PlanCycle{Tasks: []*dgraphStruct.DgraphTask{}}
		var statuses []string
		for _, id := range members[c.Id] {
			t, ok := byID[id]
			if !ok { // deleted, or kept from the reader
				continue
			}
			pc.Tasks = append(pc.Tasks, t)
			pc.EstimateMinutes += minutesOf(t)
			statuses = append(statuses, t.Status)
		}
		// Open work first, then done; each oldest first.
		sort.SliceStable(pc.Tasks, func(i, j int) bool {
			oi, oj := isOpen(pc.Tasks[i].Status), isOpen(pc.Tasks[j].Status)
			if oi != oj {
				return oi
			}
			return createdBefore(pc.Tasks[i], pc.Tasks[j])
		})
		pc.View = View{Cycle: c, Progress: Count(statuses), State: State(c, now)}
		b.Cycles = append(b.Cycles, pc)
	}
	if v := BuildVelocity(cycles, members, progress); v.TypicalHours != nil {
		m := int(*v.TypicalHours*60 + 0.5)
		b.CapacityMinutes = &m
	}
	return b
}

func createdBefore(a, b *dgraphStruct.DgraphTask) bool {
	if a.CreatedAt == nil || b.CreatedAt == nil {
		return a.CreatedAt != nil
	}
	return a.CreatedAt.Before(*b.CreatedAt)
}

// plannedIDs is the tasks the read needs beyond the backlog: the open
// cycles' (also left out of the backlog) and the latest completed ones' (for
// capacity).
func plannedIDs(cycles []cycleModel.Cycle, members map[uuid.UUID][]string) (inOpen, all []string) {
	for _, c := range cycles {
		if c.CompletedAt == nil {
			inOpen = append(inOpen, members[c.Id]...)
		}
	}
	all = append(all, inOpen...)
	for _, c := range completedCycles(cycles) {
		all = append(all, members[c.Id]...)
	}
	return inOpen, all
}

// GetBacklog reads a project's backlog for the reader of ctx: two Postgres
// reads and one graph query, however many tasks and cycles, and one read of
// keys.
func GetBacklog(ctx context.Context, project uuid.UUID, now time.Time) (*Backlog, error) {
	cycles, err := cycleModel.List(project)
	if err != nil {
		return nil, err
	}
	members, err := cycleModel.Members(project)
	if err != nil {
		return nil, err
	}
	inOpen, all := plannedIDs(cycles, members)
	read, err := projectDomain.GetDgraphProjectBacklog(ctx, project.String(), inOpen, all)
	if err != nil {
		return nil, err
	}
	b := Assemble(cycles, members, read, now)
	shown := append([]*dgraphStruct.DgraphTask{}, b.Tasks...)
	for _, c := range b.Cycles {
		shown = append(shown, c.Tasks...)
	}
	taskKeys.Lay(ctx, shown...)
	return &b, nil
}

// StartWindow is when a cycle started now runs: from now, as long as it was
// planned to. Only an upcoming cycle starts. Pure.
func StartWindow(c cycleModel.Cycle, now time.Time) (time.Time, time.Time, error) {
	switch State(c, now) {
	case "completed":
		return time.Time{}, time.Time{}, &CycleError{"That cycle is already complete."}
	case "current", "ended":
		return time.Time{}, time.Time{}, &CycleError{"That cycle has already started."}
	}
	return now, now.Add(c.EndsAt.Sub(c.StartsAt)), nil
}

// Start starts an upcoming cycle now, keeping its length. A completed cycle
// still running on the calendar ends now; one that isn't completed must be
// completed first.
func Start(ctx context.Context, c *cycleModel.Cycle, now time.Time) (*cycleModel.Cycle, error) {
	s, e, err := StartWindow(*c, now)
	if err != nil {
		return nil, err
	}
	started, err := cycleModel.Start(ctx, c.ProjectUUID, c.Id, s, e)
	if errors.Is(err, cycleModel.ErrOverlap) {
		return nil, &CycleError{"Complete the current cycle before starting the next."}
	}
	return started, err
}

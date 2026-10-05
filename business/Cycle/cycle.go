// Package business holds cycles: Linear's sprints. A project works in
// numbered, time-boxed cycles that never overlap; each task is in at most one.
// The list shows each cycle's progress, a task list can be filtered to one
// cycle, and completing a cycle carries its unfinished tasks into the next
// (making the next one, the same length, if there isn't one).
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// CycleError is a request the person can fix; its text is written for them.
type CycleError struct{ msg string }

func (e *CycleError) Error() string { return e.msg }

// Progress counts a cycle's live tasks by where they stand.
type Progress struct {
	Total   int `json:"total"`
	Started int `json:"started"`
	Done    int `json:"done"`
}

// View is a cycle with its progress and whether it's the current one.
type View struct {
	cycleModel.Cycle
	Progress Progress `json:"progress"`
	State    string   `json:"state"` // upcoming, current, ended, completed
}

// State says where a cycle stands at now. Pure.
func State(c cycleModel.Cycle, now time.Time) string {
	switch {
	case c.CompletedAt != nil:
		return "completed"
	case now.Before(c.StartsAt):
		return "upcoming"
	case now.Before(c.EndsAt):
		return "current"
	default:
		return "ended"
	}
}

// Count tallies statuses into progress. Done and cancelled both close a
// task; in progress and in review have started. Pure.
func Count(statuses []string) Progress {
	p := Progress{Total: len(statuses)}
	for _, s := range statuses {
		switch s {
		case dgraphStruct.TASK_STATUS_DONE, dgraphStruct.TASK_STATUS_CANCELED:
			p.Done++
		case dgraphStruct.TASK_STATUS_INPROGRESS, dgraphStruct.TASK_STATUS_INREVIEW:
			p.Started++
		}
	}
	return p
}

func isOpen(status string) bool {
	return status != dgraphStruct.TASK_STATUS_DONE && status != dgraphStruct.TASK_STATUS_CANCELED
}

// List is a project's cycles with their progress.
func List(ctx context.Context, project uuid.UUID, now time.Time) ([]View, error) {
	cycles, err := cycleModel.List(project)
	if err != nil {
		return nil, err
	}
	members, err := cycleModel.Members(project)
	if err != nil {
		return nil, err
	}
	var all []string
	for _, ts := range members {
		all = append(all, ts...)
	}
	status := map[string]string{}
	if len(all) > 0 {
		tasks, err := taskDomain.GetDgraphTaskStatuses(ctx, all)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			status[t.Uuid] = t.Status
		}
	}
	out := make([]View, 0, len(cycles))
	for _, c := range cycles {
		var ss []string
		for _, t := range members[c.Id] {
			if s, ok := status[t]; ok { // archived tasks don't count
				ss = append(ss, s)
			}
		}
		out = append(out, View{Cycle: c, Progress: Count(ss), State: State(c, now)})
	}
	return out, nil
}

// CreateInput is a new cycle: when it starts and how many weeks it runs.
type CreateInput struct {
	Name     string    `json:"name"`
	StartsAt time.Time `json:"starts_at"`
	Weeks    int       `json:"weeks"`
}

// Window is a cycle's start and end: whole weeks from the moment given. The
// client sends midnight where the person is; cutting it to a day here would
// cut it in UTC and move the cycle by a day for half the world. Pure.
func Window(startsAt time.Time, weeks int) (time.Time, time.Time, error) {
	if weeks < 1 || weeks > 8 {
		return time.Time{}, time.Time{}, &CycleError{"Cycles run 1 to 8 weeks."}
	}
	if startsAt.IsZero() {
		return time.Time{}, time.Time{}, &CycleError{"Pick a start date."}
	}
	return startsAt, startsAt.AddDate(0, 0, 7*weeks), nil
}

func cleanName(n string) (string, error) {
	n = strings.Join(strings.Fields(n), " ")
	if utf8.RuneCountInString(n) > 60 {
		return "", &CycleError{"Keep the name under 60 characters."}
	}
	return n, nil
}

// Create adds a cycle to a project.
func Create(project uuid.UUID, in CreateInput, by uuid.UUID) (*cycleModel.Cycle, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return nil, err
	}
	s, e, err := Window(in.StartsAt, in.Weeks)
	if err != nil {
		return nil, err
	}
	c, err := cycleModel.Create(project, name, s, e, by)
	if errors.Is(err, cycleModel.ErrOverlap) {
		return nil, &CycleError{"That overlaps another cycle. Cycles run one after another."}
	}
	return c, err
}

// Rename changes a cycle's name.
func Rename(id uuid.UUID, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	return cycleModel.Rename(id, name)
}

// Completed says how a cycle ended.
type Completed struct {
	Done    int               `json:"done"`
	Carried int               `json:"carried"`
	Next    *cycleModel.Cycle `json:"next,omitempty"`
}

// Complete closes a cycle. Unfinished tasks go to the next cycle (made if
// needed, the same length, starting when this one ends), or leave cycles
// when carry is false.
func Complete(ctx context.Context, c *cycleModel.Cycle, carry bool, by uuid.UUID) (*Completed, error) {
	if c.CompletedAt != nil {
		return nil, &CycleError{"That cycle is already complete."}
	}
	tasks, err := cycleModel.TasksIn(c.Id)
	if err != nil {
		return nil, err
	}
	statuses, err := taskDomain.GetDgraphTaskStatuses(ctx, tasks)
	if err != nil {
		return nil, err
	}
	var open []string
	done := 0
	for _, t := range statuses {
		if isOpen(t.Status) {
			open = append(open, t.Uuid)
		} else {
			done++
		}
	}
	res := &Completed{Done: done, Carried: len(open)}
	var next *uuid.UUID
	if carry && len(open) > 0 {
		n, err := nextCycle(c, by)
		if err != nil {
			return nil, err
		}
		res.Next, next = n, &n.Id
	} else {
		res.Carried = 0
	}
	ok, err := cycleModel.Complete(c.Id, done, open, next)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &CycleError{"That cycle is already complete."}
	}
	return res, nil
}

// nextCycle is the cycle after c, made when there isn't one.
func nextCycle(c *cycleModel.Cycle, by uuid.UUID) (*cycleModel.Cycle, error) {
	cycles, err := cycleModel.List(c.ProjectUUID)
	if err != nil {
		return nil, err
	}
	for i := range cycles {
		if cycles[i].Number > c.Number && cycles[i].CompletedAt == nil {
			return &cycles[i], nil
		}
	}
	length := c.EndsAt.Sub(c.StartsAt)
	start := c.EndsAt
	if now := time.Now(); start.Before(now.Add(-length)) {
		// A cycle completed long after it ended starts the next today.
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, c.StartsAt.Location())
	}
	n, err := cycleModel.Create(c.ProjectUUID, "", start, start.Add(length), by)
	if errors.Is(err, cycleModel.ErrOverlap) {
		return nil, &CycleError{"The next cycle's dates are taken. Make the next cycle first, then complete this one."}
	}
	return n, err
}

// FilterClause narrows a project's task list to the tasks in the given
// cycles ("none" for tasks in no cycle is not offered). The ids come from the
// person; anything not a cycle of this project matches nothing.
func FilterClause(project string, ids []string) (string, error) {
	pid, err := uuid.Parse(project)
	if err != nil {
		return "", err
	}
	var tasks []string
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		c, err := cycleModel.Get(id)
		if err != nil {
			return "", err
		}
		if c == nil || c.ProjectUUID != pid {
			continue
		}
		ts, err := cycleModel.TasksIn(id)
		if err != nil {
			return "", err
		}
		tasks = append(tasks, ts...)
	}
	if len(tasks) == 0 {
		return `eq(task_uuid, "00000000-0000-0000-0000-000000000000")`, nil
	}
	b, _ := json.Marshal(tasks)
	return fmt.Sprintf(`eq(task_uuid, %s)`, b), nil
}

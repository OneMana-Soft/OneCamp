package business

// Cycles across projects (linear-12, part 3): every project's running cycle
// in one list, for the person reading it. Linear shows a team's cycle on its
// own; here a lead sees each project they're in, how far its cycle has run,
// how much of it is done and whether that keeps pace, in one place.
//
// Who sees what is the per-project cycle list's own rule over the projects
// the reader is in (read from them, so other projects are never read): a
// project is in the list when its reader may view it (Work Access's NeedView, decided by
// OfMany for them all in one read of their roles), and an archived project's
// cycle is left out. Nothing here changes anything.

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	"github.com/google/uuid"
)

// Running is one project's running cycle as the list across projects shows it.
type Running struct {
	View
	ProjectName string `json:"project_name"`
	// Elapsed is how much of the cycle's time has gone, 0 to 1.
	Elapsed float64 `json:"elapsed"`
	// DaysLeft is whole days to its end, counting a part day as one.
	DaysLeft int `json:"days_left"`
	// Pace is "on_track", "behind" or "empty" (no tasks in it yet).
	Pace string `json:"pace"`
}

// Seams for tests: the reads the list is made of.
var (
	runningCycles = cycleModel.RunningIn
	cycleMembers  = cycleModel.MembersOfMany
	readProjects  = projectDomain.GetReaderProjectsReadAccess
	standingsOf   = workAccess.OfMany
	taskStatuses  = taskDomain.GetDgraphTaskStatuses
)

// paceSlack is how far done may trail time gone before a cycle is behind:
// a tenth of it, so a cycle a day into a two-week run isn't behind already.
const paceSlack = 0.1

// Pace says whether a cycle's done share keeps up with its time gone. Pure.
func Pace(p Progress, elapsed float64) string {
	if p.Total == 0 {
		return "empty"
	}
	if float64(p.Done)/float64(p.Total)+paceSlack >= elapsed {
		return "on_track"
	}
	return "behind"
}

// elapsedOf is how much of c's time has gone at now, 0 to 1. Pure.
func elapsedOf(c cycleModel.Cycle, now time.Time) float64 {
	span := c.EndsAt.Sub(c.StartsAt)
	if span <= 0 {
		return 1
	}
	e := float64(now.Sub(c.StartsAt)) / float64(span)
	return math.Max(0, math.Min(1, math.Round(e*1000)/1000))
}

// daysLeft is whole days from now to c's end, a part day counting as one. Pure.
func daysLeft(c cycleModel.Cycle, now time.Time) int {
	left := c.EndsAt.Sub(now)
	if left <= 0 {
		return 0
	}
	return int(math.Ceil(left.Hours() / 24))
}

// RunningAcross is the running cycle of each project the reader (by uuid and
// graph uid) is in and may view, soonest to end first, then by project name.
// Their projects are read first, so only their projects' cycles are.
func RunningAcross(ctx context.Context, readerUUID, readerUID string, now time.Time) ([]Running, error) {
	out := []Running{}
	projects, err := readProjects(ctx, readerUID)
	if err != nil || len(projects) == 0 {
		return out, err
	}
	standings, err := standingsOf(ctx, readerUUID, projects)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	var viewable []uuid.UUID
	for _, p := range projects {
		if p == nil {
			continue
		}
		s, ok := standings[strings.ToLower(p.Uuid)]
		id, err := uuid.Parse(p.Uuid)
		if !ok || err != nil || s.Archived || !s.Can(workAccess.NeedView) {
			continue
		}
		names[strings.ToLower(p.Uuid)] = p.Name
		viewable = append(viewable, id)
	}
	shown, err := runningCycles(ctx, viewable, now)
	if err != nil || len(shown) == 0 {
		return out, err
	}
	cycleIDs := make([]uuid.UUID, 0, len(shown))
	for _, c := range shown {
		cycleIDs = append(cycleIDs, c.Id)
	}
	members, err := cycleMembers(ctx, cycleIDs)
	if err != nil {
		return nil, err
	}
	var all []string
	for _, ts := range members {
		all = append(all, ts...)
	}
	status := map[string]string{}
	if len(all) > 0 {
		tasks, err := taskStatuses(ctx, all)
		if err != nil {
			return nil, err
		}
		for _, t := range tasks {
			if t != nil {
				status[t.Uuid] = t.Status
			}
		}
	}
	for _, c := range shown {
		out = append(out, runningOf(c, members[c.Id], status, names[strings.ToLower(c.ProjectUUID.String())], now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].EndsAt.Equal(out[j].EndsAt) {
			return out[i].EndsAt.Before(out[j].EndsAt)
		}
		return strings.ToLower(out[i].ProjectName) < strings.ToLower(out[j].ProjectName)
	})
	return out, nil
}

// runningOf is c as the list shows it, from its tasks and their statuses
// (a task missing from status is archived, and doesn't count). Pure.
func runningOf(c cycleModel.Cycle, tasks []string, status map[string]string, name string, now time.Time) Running {
	var ss []string
	for _, t := range tasks {
		if s, ok := status[t]; ok {
			ss = append(ss, s)
		}
	}
	p := Count(ss)
	e := elapsedOf(c, now)
	return Running{View: View{Cycle: c, Progress: p, State: State(c, now)}, ProjectName: name, Elapsed: e, DaysLeft: daysLeft(c, now), Pace: Pace(p, e)}
}

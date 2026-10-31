package business

// A project's critical path and slack, and the impact of a move before it is
// made (DG10). One graph read (domain.PlanTasksOf: the project's tasks and
// every task linked to them, however far, in any project), Work Access's
// verdict on each, then the planner (helpers/topo). Only tasks the person may
// see take part: a link with an end they can't see is dropped whole, so no
// slack, finish or count says anything about work they can't see.

import (
	"context"
	"errors"
	"strings"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers/topo"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// planLimit bounds what a plan that sums a project up reads (summaryLimits):
// the project's own open tasks, the soonest due first, and its finished ones,
// the latest finished first (domain.PlanTasksOf). 1,000 (main, 29 Oct 16:1x):
// at 10,000 open and 5,100 finished tasks the forecast's p95 was 371-423 ms
// at 1,000 and 318-344 ms at 100, most of it the reads over the whole
// project, so ten times the work counted costs 50-80 ms. A var, so the
// integration build's CapPlansForTest can cut a plan with a few tasks.
var planLimit = 1000

// planSeams are where a plan reads: a seam for tests.
var planSeams = struct {
	read func(ctx context.Context, projectUUIDs []string, readerUUID string, limits domain.PlanLimits) (map[string]domain.PlanRead, error)
}{read: domain.PlanTasksOf}

// summaryLimits are what a plan that sums a project up reads, its critical
// path or its forecast: at most planLimit of its own open tasks and of its
// finished ones. A move's impact reads every open task (domain.PlanLimits).
func summaryLimits() domain.PlanLimits {
	return domain.PlanLimits{Open: planLimit, Finished: planLimit}
}

// PlanTask is one task of a critical path.
type PlanTask struct {
	UUID          string  `json:"task_uuid"`
	ProjectUUID   string  `json:"project_uuid"`
	External      bool    `json:"external"`
	Name          string  `json:"task_name,omitempty"`
	ProjectName   string  `json:"project_name,omitempty"`
	Start         string  `json:"start"`
	End           string  `json:"end"`
	EarliestStart *string `json:"earliest_start"`
	LatestEnd     string  `json:"latest_end"`
	Slack         int     `json:"slack"`
	FreeSlack     int     `json:"free_slack"`
	Critical      bool    `json:"critical"`
	Late          bool    `json:"late"`
}

// PlanLink is one dependency of a critical path: To waits on From.
type PlanLink struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Kind     string `json:"kind"`
	Lag      int    `json:"lag"`
	Critical bool   `json:"critical"`
	External bool   `json:"external"`
}

// CriticalPathAnswer is GET /project/{id}/critical-path's answer
// (design/dgraph/DG10-CONTRACT.md §1).
type CriticalPathAnswer struct {
	ProjectUUID string     `json:"project_uuid"`
	Finish      *string    `json:"finish"`
	Tasks       []PlanTask `json:"tasks"`
	Links       []PlanLink `json:"links"`
	Path        []string   `json:"path"`
	Looped      []string   `json:"looped"`
	Truncated   bool       `json:"truncated"`
	CanEdit     bool       `json:"can_edit"`
}

// ofProject is whether t is one of projectUUID's own tasks.
func ofProject(t *dgraphStruct.DgraphTask, projectUUID string) bool {
	return t != nil && t.Project != nil && strings.EqualFold(t.Project.Uuid, projectUUID)
}

// plan is what a project's plan starts from: the tasks read for its reader,
// the ones they may see, their verdicts, and what the read's limits left out.
type plan struct {
	all, visible []*dgraphStruct.DgraphTask
	verdicts     map[string]workAccess.Verdict
	cut          domain.PlanCut
}

// readPlans is each project's plan for user as limits keeps it, by lower-case
// project id, in one graph read and one read of Work Access's verdicts however
// many projects there are. Linked work can show in more than one plan, and is
// judged once.
func readPlans(ctx context.Context, projectUUIDs []string, user *dgraphStruct.DgraphUser, limits domain.PlanLimits) (map[string]plan, error) {
	read, err := planSeams.read(ctx, projectUUIDs, user.Uuid, limits)
	if err != nil {
		return nil, err
	}
	var every []*dgraphStruct.DgraphTask
	judged := map[string]bool{}
	for _, r := range read {
		for _, t := range r.Tasks {
			if t != nil && !judged[strings.ToLower(t.Uuid)] {
				judged[strings.ToLower(t.Uuid)] = true
				every = append(every, t)
			}
		}
	}
	verdicts, err := shiftSeams.judge(ctx, user.Uuid, user.Uid, every...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]plan, len(read))
	for id, r := range read {
		p := plan{all: r.Tasks, verdicts: verdicts, cut: r.Cut}
		for _, t := range r.Tasks {
			if t != nil && verdicts[strings.ToLower(t.Uuid)].See {
				p.visible = append(p.visible, t)
			}
		}
		out[strings.ToLower(id)] = p
	}
	return out, nil
}

// readPlan is readPlans for one project.
func readPlan(ctx context.Context, projectUUID string, user *dgraphStruct.DgraphUser, limits domain.PlanLimits) (all, visible []*dgraphStruct.DgraphTask, verdicts map[string]workAccess.Verdict, cut domain.PlanCut, err error) {
	plans, err := readPlans(ctx, []string{projectUUID}, user, limits)
	if err != nil {
		return nil, nil, nil, domain.PlanCut{}, err
	}
	p := plans[strings.ToLower(projectUUID)]
	return p.all, p.visible, p.verdicts, p.cut, nil
}

// CriticalPath is projectUUID's critical path and each task's slack, as user
// may see them, in loc's days.
func CriticalPath(ctx context.Context, projectUUID string, loc *time.Location, user *dgraphStruct.DgraphUser) (*CriticalPathAnswer, error) {
	ctx = taskread.ActingFor(ctx, user.Uuid)
	_, visible, _, cut, err := readPlan(ctx, projectUUID, user, summaryLimits())
	if err != nil {
		return nil, err
	}
	s, byID := scheduleOf(visible, loc)
	plan, err := topo.CriticalPath(s)
	if err != nil && !errors.Is(err, topo.ErrLoop) {
		return nil, err
	}
	out := &CriticalPathAnswer{ProjectUUID: projectUUID, Tasks: []PlanTask{}, Links: []PlanLink{}, Path: plan.Path, Looped: plan.Looped, Truncated: cut.Open || cut.Finished}
	if out.Path == nil {
		out.Path = []string{}
	}
	if out.Looped == nil {
		out.Looped = []string{}
	}
	if len(plan.Slots) > 0 {
		f := plan.Finish.String()
		out.Finish = &f
	}
	external := func(t *dgraphStruct.DgraphTask) bool { return !ofProject(t, projectUUID) }
	for _, st := range s.Tasks {
		slot, ok := plan.Slots[st.ID]
		if !ok {
			continue
		}
		t := byID[st.ID]
		pt := PlanTask{UUID: t.Uuid, External: external(t), Start: st.Start.String(), End: st.End.String(),
			LatestEnd: slot.LatestEnd.String(), Slack: slot.Slack, FreeSlack: slot.FreeSlack, Critical: slot.Critical, Late: slot.Late}
		if t.Project != nil {
			pt.ProjectUUID = t.Project.Uuid
		}
		if pt.External {
			pt.Name = t.Name
			if t.Project != nil {
				pt.ProjectName = t.Project.Name
			}
		}
		if slot.EarliestStart != nil {
			es := slot.EarliestStart.String()
			pt.EarliestStart = &es
		}
		out.Tasks = append(out.Tasks, pt)
	}
	for _, l := range s.Links {
		_, from := plan.Slots[l.From]
		_, to := plan.Slots[l.To]
		if !from || !to {
			continue
		}
		out.Links = append(out.Links, PlanLink{From: l.From, To: l.To, Kind: string(l.Kind), Lag: l.Lag,
			Critical: plan.CriticalLink(l.From, l.To), External: external(byID[l.From]) || external(byID[l.To])})
	}
	return out, nil
}

// ImpactMove is one task a move would push along.
type ImpactMove struct {
	UUID        string     `json:"task_uuid"`
	ProjectUUID string     `json:"project_uuid"`
	Start       *time.Time `json:"task_start_date,omitempty"`
	Due         *time.Time `json:"task_due_date,omitempty"`
}

// ImpactAnswer is GET /task/{id}/impact's answer (DG10-CONTRACT.md §2).
type ImpactAnswer struct {
	Delays struct {
		Tasks    int `json:"tasks"`
		Projects int `json:"projects"`
	} `json:"delays"`
	Moves []ImpactMove `json:"moves"`
	Stuck struct {
		Tasks int `json:"tasks"`
	} `json:"stuck"`
	Finish struct {
		Before *string `json:"before"`
		After  *string `json:"after"`
	} `json:"finish"`
}

// Impact is what moving task to dates would do, as user may see it, in loc's
// days, without writing anything: the tasks it would push along (exactly what
// MoveTaskDates would write if nothing changes in between), the tasks user
// can see but not change that would then start before the work they wait on,
// and the task's project's finish before and after.
func Impact(ctx context.Context, task *dgraphStruct.DgraphTask, dates TaskDates, loc *time.Location, user *dgraphStruct.DgraphUser) (*ImpactAnswer, error) {
	if task == nil || task.Project == nil || task.Project.Uuid == "" {
		return nil, ErrDependencyMissing
	}
	ctx = taskread.ActingFor(ctx, user.Uuid)
	all, visible, _, _, err := readPlan(ctx, task.Project.Uuid, user, domain.PlanLimits{Finished: planLimit})
	if err != nil {
		return nil, err
	}
	out := &ImpactAnswer{Moves: []ImpactMove{}}
	found := false
	for _, t := range all {
		found = found || (t != nil && strings.EqualFold(t.Uuid, task.Uuid))
	}
	if !found {
		// A sub-task takes no dependencies: moving it pushes nothing along.
		return out, nil
	}
	m, err := planMove(ctx, all, task.Uuid, &dates, loc, user)
	if err != nil {
		return nil, err
	}
	after := map[string]*dgraphStruct.DgraphTask{strings.ToLower(m.moved.Uuid): m.moved}
	projects := map[string]bool{}
	for _, s := range m.shifted {
		p := ""
		if s.before.Project != nil {
			p = s.before.Project.Uuid
		}
		projects[strings.ToLower(p)] = true
		out.Moves = append(out.Moves, ImpactMove{UUID: s.UUID, ProjectUUID: p, Start: s.Start, Due: s.Due})
		c := *s.before
		if s.Start != nil {
			c.StartDate = s.Start
		}
		if s.Due != nil {
			c.DueDate = s.Due
		}
		after[strings.ToLower(s.UUID)] = &c
	}
	out.Delays.Tasks, out.Delays.Projects = len(m.shifted), len(projects)
	// Stuck: seen, not theirs to change, waiting on a task that moves, and
	// that move breaks the link.
	for _, t := range all {
		v := m.verdicts[strings.ToLower(t.Uuid)]
		if !v.See || v.Move || after[strings.ToLower(t.Uuid)] != nil {
			continue
		}
		to := topoTaskOf(t, loc)
		if !to.Dated || to.Closed {
			continue
		}
		for _, b := range t.BlockedBy {
			if b == nil {
				continue
			}
			moved := after[strings.ToLower(b.Uuid)]
			if moved == nil {
				continue
			}
			from := topoTaskOf(moved, loc)
			if from.Dated && !from.Closed && !topo.Holds(topo.Link{From: b.Uuid, To: t.Uuid, Kind: topo.Kind(kindOf(b)), Lag: b.DependencyLag}, from, to) {
				out.Stuck.Tasks++
				break
			}
		}
	}
	// The project's finish, before and after.
	finish := func(ts []*dgraphStruct.DgraphTask) *string {
		var mine []*dgraphStruct.DgraphTask
		for _, t := range ts {
			if t.Project != nil && strings.EqualFold(t.Project.Uuid, task.Project.Uuid) {
				mine = append(mine, t)
			}
		}
		s, _ := scheduleOf(mine, loc)
		p, _ := topo.CriticalPath(s)
		if len(p.Slots) == 0 {
			return nil
		}
		f := p.Finish.String()
		return &f
	}
	out.Finish.Before = finish(visible)
	moved := make([]*dgraphStruct.DgraphTask, 0, len(visible))
	for _, t := range visible {
		if a := after[strings.ToLower(t.Uuid)]; a != nil {
			t = a
		}
		moved = append(moved, t)
	}
	out.Finish.After = finish(moved)
	return out, nil
}

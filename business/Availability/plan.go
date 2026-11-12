package business

// "Plan my week" (calendar B13 on the engine, CAL52-T3): blocks proposed for
// the viewer's own open tasks in their free time, over the rest of their
// work week in their hours, and never written. Go places each block in the
// free time the engine reads (Rank, one block at a time, each placed block
// busy for the next); a model only orders the tasks and says why, through
// the orderer the AI edition registers (business/AI planWeek.go), so it never
// invents a time. Without one, with AI off or when the model fails, the order
// is by due date then length, said once. Confirming is the calendar's: each
// kept proposal is created as a task_block through the one event create.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

const (
	// planMaxTasks is how many open tasks a plan considers, soonest due first.
	planMaxTasks = 50
	// A block is the task's estimate, or planDefaultBlock minutes without
	// one, and at most planMaxBlock: a two-day task gets its first 4 hours.
	planDefaultBlock = 30
	planMaxBlock     = 4 * 60
	// planMinLeft is the work time this week worth planning; with less left
	// (Friday evening, the weekend) the plan is for next week.
	planMinLeft = time.Hour

	noTimeLeft    = "No free time left this week"
	nothingToPlan = "Nothing to plan: no open tasks"
)

// PlanInput is a request for a plan: the viewer's hours as the calendar
// sends them, standing in until they keep their own, and the tasks that
// already have a block this week (the grid has them), not planned again.
type PlanInput struct {
	Hours calendarBusiness.WorkingHours `json:"hours"`
	Skip  []string                      `json:"skip,omitempty"`
}

// PlanTask is an open task a plan may place: how long its block is, when it
// is due, and how many open tasks it waits on (in the viewer's projects),
// with the first of them when the viewer may read it (business/Task
// ReadableBlockTasks).
type PlanTask struct {
	UUID        string                    `json:"task_uuid"`
	Name        string                    `json:"task_name"`
	ProjectName string                    `json:"project_name,omitempty"`
	Due         *time.Time                `json:"due,omitempty"`
	Minutes     int                       `json:"minutes"`
	BlockedOpen int                       `json:"-"`
	Blocker     *taskBusiness.ReadableRef `json:"-"`
}

// Ordered is a task in the order a plan takes it, and why it comes there.
type Ordered struct {
	UUID   string
	Reason string
}

// PlanOrder is a model's order for a plan, and which model gave it and
// whether it runs on the workspace's own network.
type PlanOrder struct {
	Order []Ordered
	Model string
	Local bool
}

// PlanOrderer orders a plan's tasks: the AI edition's, with the workspace's
// model. nil and no error is AI off.
type PlanOrderer func(ctx context.Context, who *userModels.UserInfo, tasks []PlanTask, from, to time.Time) (*PlanOrder, error)

// planOrderer is the orderer business/AI registers; nil without the AI
// edition.
var planOrderer PlanOrderer

// RegisterPlanOrderer sets the orderer (business/AI's init) and returns the
// one it replaces.
func RegisterPlanOrderer(o PlanOrderer) PlanOrderer {
	was := planOrderer
	planOrderer = o
	return was
}

// Proposal is a block a plan proposes for a task, and why then. After is
// the task whose block it follows, when it waits on that one.
type Proposal struct {
	TaskUUID    string    `json:"task_uuid"`
	TaskName    string    `json:"task_name"`
	ProjectName string    `json:"project_name,omitempty"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Reason      string    `json:"reason"`
	After       string    `json:"after,omitempty"`
}

// Unplanned is an open task a plan leaves out, and why, in words.
type Unplanned struct {
	TaskUUID string `json:"task_uuid"`
	TaskName string `json:"task_name"`
	Reason   string `json:"reason"`
}

// WeekPlan is a plan: its week, the proposals and what is left out. By is
// "model" or "due"; Model and Local say which model ordered it and where
// it runs; Note says once why the plan is by due date, or why it is empty;
// Unread is a Google calendar that couldn't be read (a block may clash
// there).
type WeekPlan struct {
	From      time.Time   `json:"from"`
	To        time.Time   `json:"to"`
	Proposals []Proposal  `json:"proposals"`
	Left      []Unplanned `json:"left"`
	By        string      `json:"by"`
	Model     string      `json:"model,omitempty"`
	Local     bool        `json:"local,omitempty"`
	Note      string      `json:"note,omitempty"`
	Unread    bool        `json:"unread,omitempty"`
}

// openTasksOf is the viewer's open tasks: a seam for tests.
var openTasksOf = readOpenTasks

// PlanWeek proposes blocks for the viewer's open tasks over the rest of
// their work week, in their free time and hours. It writes nothing.
func PlanWeek(ctx context.Context, who *userModels.UserInfo, in PlanInput, now time.Time) (*WeekPlan, error) {
	if who == nil {
		return nil, errors.New("no one to plan for")
	}
	skip := make(map[string]bool, len(in.Skip))
	for _, id := range in.Skip {
		skip[strings.ToLower(strings.TrimSpace(id))] = true
	}
	tasks, err := openTasksOf(ctx, who, skip)
	if err != nil {
		return nil, errors.New("couldn't read your tasks")
	}
	asker := who.UserDgraphInfo.Uuid
	// Two weeks read: next week's whole when this one is over.
	people, _, read, err := readPeople(ctx, asker, []string{asker}, now, now.Add(maxRange), in.Hours)
	if err != nil {
		return nil, err
	}
	if len(people) == 0 {
		return nil, errors.New("couldn't read calendars")
	}
	me := people[0]
	if me.Hours == nil {
		h := officeHours(in.Hours.TZ)
		me.Hours = &h
	}
	zone, err := me.Hours.Check()
	if err != nil {
		zone = time.UTC
	}
	from, to := weekWindow(*me.Hours, now)
	plan := &WeekPlan{From: from, To: to, Proposals: []Proposal{}, Left: []Unplanned{}, By: "due", Unread: len(read.Unread) > 0}
	if len(tasks) == 0 {
		plan.Note = nothingToPlan
		return plan, nil
	}
	if len(tasks) > planMaxTasks {
		keep := map[string]bool{}
		for _, o := range byDue(tasks, zone)[:planMaxTasks] {
			keep[o.UUID] = true
		}
		kept := tasks[:0]
		for _, t := range tasks {
			if keep[t.UUID] {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	order := orderTasks(ctx, who, tasks, from, to, zone, plan)
	plan.Proposals, plan.Left = place(tasks, order, me, Ask{Step: 15 * time.Minute, From: from, To: to,
		Earliest: now.Truncate(15 * time.Minute).Add(15 * time.Minute), Zone: zone})
	if len(plan.Proposals) == 0 {
		for _, l := range plan.Left {
			if l.Reason == noTimeLeft {
				plan.Note = noTimeLeft
				break
			}
		}
	}
	return plan, nil
}

// orderTasks is the order a plan takes the tasks in: the model's, then any
// it left out by due date; without a model, by due date, plan's note saying
// so once.
func orderTasks(ctx context.Context, who *userModels.UserInfo, tasks []PlanTask, from, to time.Time, zone *time.Location, plan *WeekPlan) []Ordered {
	due := byDue(tasks, zone)
	if planOrderer == nil {
		plan.Note = "Planned by due date"
		return due
	}
	o, err := planOrderer(ctx, who, tasks, from, to)
	switch {
	case err != nil:
		plan.Note = "Planned by due date: AI didn't answer"
		return due
	case o == nil:
		plan.Note = "Planned by due date: AI is off"
		return due
	}
	known := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		known[t.UUID] = true
	}
	out := make([]Ordered, 0, len(tasks))
	seen := map[string]bool{}
	for _, x := range o.Order {
		if known[x.UUID] && !seen[x.UUID] {
			seen[x.UUID] = true
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		plan.Note = "Planned by due date: AI didn't answer"
		return due
	}
	for _, x := range due {
		if !seen[x.UUID] {
			out = append(out, x)
		}
	}
	plan.By, plan.Model, plan.Local = "model", o.Model, o.Local
	return out
}

// place puts each task, in order, in the first free time in hours between
// ask.From and ask.To at or after ask.Earliest, its block then busy for the
// next. A task waiting on one open task in the plan comes after that one's
// block (or is left out with it); one waiting on anything else is left out,
// saying on what. Pure.
func place(tasks []PlanTask, order []Ordered, me Person, ask Ask) ([]Proposal, []Unplanned) {
	byID := make(map[string]PlanTask, len(tasks))
	for _, t := range tasks {
		byID[t.UUID] = t
	}
	reasons := make(map[string]string, len(order))
	for _, o := range order {
		reasons[o.UUID] = o.Reason
	}
	busy := append([]calendarBusiness.Interval(nil), me.Busy...)
	proposals, left := []Proposal{}, []Unplanned{}
	placed := map[string]Proposal{}
	decided := map[string]bool{}
	waiters := map[string][]string{}
	leave := func(t PlanTask, why string) {
		decided[t.UUID] = true
		left = append(left, Unplanned{TaskUUID: t.UUID, TaskName: t.Name, Reason: why})
	}
	var decide func(id string)
	decide = func(id string) {
		t := byID[id]
		a := ask
		after := ""
		if t.BlockedOpen > 0 {
			b := t.Blocker
			if t.BlockedOpen != 1 || b == nil {
				leave(t, waitingOn(t))
				return
			}
			if _, inPlan := byID[b.UUID]; !inPlan {
				leave(t, waitingOn(t))
				return
			}
			if !decided[b.UUID] {
				waiters[b.UUID] = append(waiters[b.UUID], id)
				return
			}
			p, ok := placed[b.UUID]
			if !ok {
				leave(t, waitingOn(t))
				return
			}
			if p.End.After(a.From) {
				a.From = p.End
			}
			after = b.UUID
		}
		a.Duration, a.PerDay, a.Limit = time.Duration(t.Minutes)*time.Minute, 0, 1
		got := Rank([]Person{{ID: me.ID, Name: me.Name, Hours: me.Hours, Busy: busy}}, a)
		if len(got) == 0 {
			leave(t, noTimeLeft)
		} else {
			decided[id] = true
			p := Proposal{TaskUUID: id, TaskName: t.Name, ProjectName: t.ProjectName, Start: got[0].Start, End: got[0].End, Reason: reasons[id], After: after}
			placed[id] = p
			proposals = append(proposals, p)
			busy = append(busy, calendarBusiness.Interval{Start: p.Start, End: p.End})
		}
		next := waiters[id]
		delete(waiters, id)
		for _, w := range next {
			decide(w)
		}
	}
	for _, o := range order {
		if _, ok := byID[o.UUID]; ok && !decided[o.UUID] {
			decide(o.UUID)
		}
	}
	// Tasks that wait on each other in a ring never come up: left out.
	for _, o := range order {
		if t, ok := byID[o.UUID]; ok && !decided[o.UUID] {
			leave(t, waitingOn(t))
		}
	}
	return proposals, left
}

// waitingOn says what a task waits on: its blocker by name only when the
// viewer may read it, otherwise how many. Pure.
func waitingOn(t PlanTask) string {
	switch {
	case t.BlockedOpen == 1 && t.Blocker != nil && t.Blocker.Name != "":
		return "Waiting on " + t.Blocker.Name
	case t.BlockedOpen == 1:
		return "Waiting on 1 task"
	}
	return fmt.Sprintf("Waiting on %d tasks", t.BlockedOpen)
}

// byDue is the order without a model: soonest due first, then the shorter
// task, undated last, each saying when it is due in zone. Pure.
func byDue(tasks []PlanTask, zone *time.Location) []Ordered {
	sorted := append([]PlanTask(nil), tasks...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if (a.Due == nil) != (b.Due == nil) {
			return a.Due != nil
		}
		if a.Due != nil && !a.Due.Equal(*b.Due) {
			return a.Due.Before(*b.Due)
		}
		return a.Minutes < b.Minutes
	})
	out := make([]Ordered, len(sorted))
	for i, t := range sorted {
		out[i] = Ordered{UUID: t.UUID, Reason: "No due date"}
		if t.Due != nil {
			out[i].Reason = "Due " + t.Due.In(zone).Format("Mon 2 Jan")
		}
	}
	return out
}

// weekWindow is the week a plan covers: from now to the end of this week
// (Monday to Sunday in the hours' zone), or next week's whole when less than
// planMinLeft of this week's hours is left. Pure.
func weekWindow(h calendarBusiness.WorkingHours, now time.Time) (time.Time, time.Time) {
	loc, err := h.Check()
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	monday := time.Date(local.Year(), local.Month(), local.Day()-(int(local.Weekday())+6)%7, 0, 0, 0, 0, loc)
	end := monday.AddDate(0, 0, 7)
	var left time.Duration
	for _, w := range calendarBusiness.Windows(h, now, end) {
		left += w.End.Sub(w.Start)
	}
	if left >= planMinLeft {
		return now, end
	}
	return end, end.AddDate(0, 0, 7)
}

// officeHours is Monday to Friday, 09:00 to 17:00, in tz (UTC when it isn't
// a zone): the plan's hours for a viewer who keeps none and sends none.
func officeHours(tz string) calendarBusiness.WorkingHours {
	h := calendarBusiness.WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: tz}
	if _, err := h.Check(); err != nil {
		h.TZ = "UTC"
	}
	return h
}

// planTasksFrom is the open tasks a plan may place, from the open=1 read,
// leaving out those in skip. Pure.
func planTasksFrom(list []*dgraphStruct.DgraphTask, skip map[string]bool) []PlanTask {
	out := make([]PlanTask, 0, len(list))
	for _, t := range list {
		if t == nil || t.Uuid == "" {
			continue
		}
		id := strings.ToLower(t.Uuid)
		if skip[id] {
			continue
		}
		p := PlanTask{UUID: id, Name: t.Name, Due: t.DueDate, Minutes: planDefaultBlock, BlockedOpen: int(t.BlockedOpen)}
		if t.EstimateMinutes != nil && *t.EstimateMinutes > 0 {
			p.Minutes = min(*t.EstimateMinutes, planMaxBlock)
		}
		if t.Project != nil {
			p.ProjectName = t.Project.Name
		}
		out = append(out, p)
	}
	return out
}

// readOpenTasks is the viewer's own open tasks, by the calendar Tasks list's
// read (/user/assignedTaskList?open=1, its checks), and what the blocked ones
// wait on, read as a block's task is (business/Task ReadableBlockTasks).
func readOpenTasks(ctx context.Context, who *userModels.UserInfo, skip map[string]bool) ([]PlanTask, error) {
	clause, err := taskDomain.TaskListFilter{Open: true, Now: time.Now(), Status: taskStatusBusiness.FilterClause}.Clause()
	if err != nil {
		return nil, err
	}
	sorting, err := dgraphquery.TaskListOrder("")
	if err != nil {
		return nil, err
	}
	u, err := userBusiness.GetDgraphUserTaskList(ctx, who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid, clause, sorting, 0, 0, true)
	if err != nil || u == nil {
		return nil, err
	}
	tasks := planTasksFrom(u.Tasks, skip)
	var blocked []string
	for _, t := range tasks {
		if t.BlockedOpen > 0 {
			blocked = append(blocked, t.UUID)
		}
	}
	if len(blocked) == 0 {
		return tasks, nil
	}
	found, err := taskBusiness.ReadableBlockTasks(ctx, blocked, who.UserDgraphInfo.Uid, who.UserDgraphInfo.Uuid)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if r, ok := found[tasks[i].UUID]; ok {
			tasks[i].BlockedOpen, tasks[i].Blocker = r.BlockedOpen, r.FirstBlocker
		}
	}
	return tasks, nil
}

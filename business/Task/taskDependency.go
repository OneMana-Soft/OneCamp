package business

// Dependencies: a task can wait on other tasks of its project. Most often
// finish to start (it can start once they are done); also start to start,
// finish to finish and start to finish, each with a lag of days after (or,
// negative, ahead). The timeline draws them as arrows and, when a task moves
// later, moves the tasks waiting on it along (ShiftDependents); a board, a
// list and the timeline mark a task still waiting to start as blocked. Only a
// project's top-level tasks take part.

import (
	"context"
	"errors"
	"strings"
	"time"

	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/topo"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

var (
	ErrDependencySelf = errors.New("a task can't wait on itself")
	// ErrDependencyProject is a task in no project: dependencies are between
	// tasks of projects (the same one, or two, DG10).
	ErrDependencyProject = errors.New("a task in no project takes no dependencies")
	ErrDependencySubtask = errors.New("subtasks don't take dependencies")
	ErrDependencyLoop    = errors.New("the dependency would make a loop")
	ErrDependencyMissing = errors.New("task not found")
	ErrDependencyKind    = errors.New("not a kind of dependency, or too long a lag")
)

// mayChangeInTxn is whether user may change task, as read inside the
// dependency's transaction for them, by the rule (Work Access).
func mayChangeInTxn(ctx context.Context, task *dgraphStruct.DgraphTask, user *dgraphStruct.DgraphUser) error {
	s, f, err := workAccess.OfTask(ctx, user.Uuid, user.Uid, task)
	if err != nil {
		return err
	}
	return s.CheckTask(workAccess.NeedEditTask, f)
}

// mayLinkInTxn is whether user may make task wait on blocker, as read inside
// the dependency's transaction, by Work Access's dependency rule (DG10): in
// one project, changing the task is enough; across two, changing tasks in
// both. A blocker they can't see is not there (ErrDependencyMissing).
func mayLinkInTxn(ctx context.Context, task, blocker *dgraphStruct.DgraphTask, user *dgraphStruct.DgraphUser) error {
	verdicts, err := workAccess.JudgeTasks(ctx, user.Uuid, user.Uid, task, blocker)
	if err != nil {
		return err
	}
	same := strings.EqualFold(task.Project.Uuid, blocker.Project.Uuid)
	err = workAccess.MayLink(verdicts[strings.ToLower(task.Uuid)], verdicts[strings.ToLower(blocker.Uuid)], same)
	if errors.Is(err, workAccess.ErrHiddenEnd) {
		return ErrDependencyMissing
	}
	return err
}

// The kinds of dependency, as the edge's kind facet keeps them. A
// dependency with no kind is finish to start.
const (
	FinishToStart  = "fs"
	StartToStart   = "ss"
	FinishToFinish = "ff"
	StartToFinish  = "sf"
	// MaxLag bounds a lag, either way, in days.
	MaxLag = 365
)

// kindOf is a dependency's kind, finish to start when it has none.
func kindOf(blocker *dgraphStruct.DgraphTask) string {
	switch blocker.DependencyKind {
	case StartToStart, FinishToFinish, StartToFinish:
		return blocker.DependencyKind
	}
	return FinishToStart
}

func live(t *dgraphStruct.DgraphTask) bool {
	return t != nil && t.Uuid != "" && (t.DeletedAt == nil || t.DeletedAt.Year() <= 1970)
}

// AddTaskDependency makes taskUUID wait on blockerUUID in the way kind says
// (empty is finish to start), lag days after (negative: ahead). Both must be
// live top-level tasks of projects: of one, where the person may change the
// task, or of two, where they may change tasks in both (DG10). The blocker
// must not already wait on the task, however indirectly. Adding a
// dependency that's already there changes how it waits, if that's different;
// it goes into the task's history only when it's new.
func AddTaskDependency(ctx context.Context, taskUUID, blockerUUID, kind string, lag int, user *dgraphStruct.DgraphUser) error {
	if taskUUID == blockerUUID {
		return ErrDependencySelf
	}
	if kind == "" {
		kind = FinishToStart
	}
	if kind != FinishToStart && kind != StartToStart && kind != FinishToFinish && kind != StartToFinish || lag < -MaxLag || lag > MaxLag {
		return ErrDependencyKind
	}
	existed := false
	changed, err := domain.ChangeTaskDependency(ctx, taskUUID, blockerUUID, user.Uid, false, kind, lag, func(pair *domain.DependencyPair) error {
		existed = pair.Linked
		if !live(pair.Task) || !live(pair.Blocker) {
			return ErrDependencyMissing
		}
		if pair.Task.ParentTask != nil || pair.Blocker.ParentTask != nil {
			return ErrDependencySubtask
		}
		if pair.Task.Project == nil || pair.Blocker.Project == nil || pair.Task.Project.Uuid == "" || pair.Blocker.Project.Uuid == "" {
			return ErrDependencyProject
		}
		if err := mayLinkInTxn(ctx, pair.Task, pair.Blocker, user); err != nil {
			return err
		}
		if pair.Upstream[taskUUID] {
			return ErrDependencyLoop
		}
		return nil
	})
	if err != nil || !changed || existed {
		return err
	}
	return recordDependency(ctx, taskUUID, user, dgraphStruct.ACTIVITY_TYPE_ADD_DEPENDENCY, "", blockerUUID)
}

// RemoveTaskDependency stops taskUUID waiting on blockerUUID. Either may
// since have been deleted; the person must be an admin of the task's project.
// Taking off a dependency that isn't there changes nothing.
func RemoveTaskDependency(ctx context.Context, taskUUID, blockerUUID string, user *dgraphStruct.DgraphUser) error {
	changed, err := domain.ChangeTaskDependency(ctx, taskUUID, blockerUUID, user.Uid, true, "", 0, func(pair *domain.DependencyPair) error {
		if pair.Task == nil || pair.Task.Uuid == "" || pair.Task.Project == nil {
			return ErrDependencyMissing
		}
		if err := mayChangeInTxn(ctx, pair.Task, user); err != nil {
			return err
		}
		return nil
	})
	if err != nil || !changed {
		return err
	}
	return recordDependency(ctx, taskUUID, user, dgraphStruct.ACTIVITY_TYPE_REMOVE_DEPENDENCY, blockerUUID, "")
}

// recordDependency writes the line in the waiting task's history.
func recordDependency(ctx context.Context, taskUUID string, user *dgraphStruct.DgraphUser, kind, before, after string) error {
	now := time.Now()
	write := &dgraphStruct.DgraphTask{Uid: "uid(task)", Uuid: taskUUID, UpdatedAt: &now, Activity: []*dgraphStruct.DgraphTaskActivity{{
		Uuid:      uuid.NewString(),
		Type:      kind,
		CreatedBy: &dgraphStruct.DgraphUser{Uid: user.Uid},
		LogTime:   &now,
		PrevState: before,
		NextState: after,
	}}}
	_, err := domain.CreateOrUpdateDgraphTask(ctx, write)
	return err
}

// ShiftedTask is a task that moved because a task it waits on did.
type ShiftedTask struct {
	UUID  string     `json:"task_uuid"`
	Start *time.Time `json:"task_start_date,omitempty"`
	Due   *time.Time `json:"task_due_date,omitempty"`

	before *dgraphStruct.DgraphTask
}

// maxShifted bounds how many tasks one move can push along.
const maxShifted = 500

// moveTries is how many times a move reads and plans again when its tasks
// change under it before it gives up (ErrMoveStale).
const moveTries = 3

// ErrMoveStale is a move whose tasks kept changing while it was planned:
// nothing was written.
var ErrMoveStale = domain.ErrMoveStale

// MoveTaskDates sets task's dates and moves the tasks waiting on it along, as far
// as they must (PlanShifts), in one write: the task and its chain move
// together or not at all (DG10; domain.CommitMove). Tasks in other projects
// move where the person may change them, and a task they may not change
// stays, with the tasks beyond it (Work Access's dependency rule). Each move
// goes into its task's history; one message per project tells whoever has it
// open. Days are in loc. task is as the caller read it, for the person.
func MoveTaskDates(ctx context.Context, task *dgraphStruct.DgraphTask, dates TaskDates, loc *time.Location, user *dgraphStruct.DgraphUser) ([]ShiftedTask, error) {
	if helpers.IsSoftDeleted(task.DeletedAt) {
		return nil, ErrTaskDeleted
	}
	return moveAlong(ctx, task.Uuid, &dates, loc, user)
}

// moveAlong moves the task movedUUID to dates (nil: leaves it, and moves the
// tasks waiting on it for its dates now) and the tasks it pushes along.
func moveAlong(ctx context.Context, movedUUID string, dates *TaskDates, loc *time.Location, user *dgraphStruct.DgraphUser) ([]ShiftedTask, error) {
	// The reads are the mover's: what they see, and how they stand to it.
	ctx = taskread.ActingFor(ctx, user.Uuid)
	var err error
	for try := 0; try < moveTries; try++ {
		var all []*dgraphStruct.DgraphTask
		if all, err = shiftSeams.schedule(ctx, movedUUID); err != nil {
			return nil, err
		}
		var m *movePlan
		if m, err = planMove(ctx, all, movedUUID, dates, loc, user); err != nil {
			return nil, err
		}
		if len(m.writes) == 0 {
			return m.shifted, nil
		}
		err = shiftSeams.commit(ctx, m.ids, m.at, m.writes, m.guards)
		if errors.Is(err, domain.ErrMoveStale) {
			continue
		}
		if err != nil {
			return nil, err
		}
		shiftSeams.after(ctx, m.moves, user)
		return m.shifted, nil
	}
	return nil, err
}

// movedTask is one task a move writes: as it was, and its new dates.
type movedTask struct {
	before *dgraphStruct.DgraphTask
	dates  TaskDates
}

// movePlan is a move worked out from one read: what it writes, guarded by
// what it read, and the tasks it read with the person's verdict on each.
type movePlan struct {
	at       time.Time
	ids      []uuid.UUID
	writes   []*dgraphStruct.DgraphTask
	guards   []domain.MoveGuard
	moves    []movedTask
	shifted  []ShiftedTask
	all      []*dgraphStruct.DgraphTask
	verdicts map[string]workAccess.Verdict
	moved    *dgraphStruct.DgraphTask // as planned: at its new dates
}

// planMove works out a move from the tasks read (the moved one and those
// waiting on it): the moved task to dates (nil: as it is), and the tasks the
// person may change pushed along.
func planMove(ctx context.Context, all []*dgraphStruct.DgraphTask, movedUUID string, dates *TaskDates, loc *time.Location, user *dgraphStruct.DgraphUser) (*movePlan, error) {
	verdicts, err := shiftSeams.judge(ctx, user.Uuid, user.Uid, all...)
	if err != nil {
		return nil, err
	}
	m := &movePlan{at: time.Now(), all: all, verdicts: verdicts}
	var moved *dgraphStruct.DgraphTask
	kept := make([]*dgraphStruct.DgraphTask, 0, len(all))
	for _, t := range all {
		if t == nil {
			continue
		}
		if strings.EqualFold(t.Uuid, movedUUID) {
			if moved != nil {
				continue
			}
			moved = t
			continue
		}
		if verdicts[strings.ToLower(t.Uuid)].Move {
			kept = append(kept, t)
		}
	}
	if moved == nil || !verdicts[strings.ToLower(moved.Uuid)].See {
		return nil, ErrDependencyMissing
	}
	v := verdicts[strings.ToLower(moved.Uuid)]
	if err := v.Standing.CheckTask(workAccess.NeedEditTask, v.Facts); err != nil {
		return nil, err
	}
	planned := moved
	if dates != nil {
		if helpers.IsSoftDeleted(moved.DeletedAt) {
			return nil, ErrTaskDeleted
		}
		c := *moved
		if dates.Start != nil {
			c.StartDate = dates.Start
		}
		if dates.Due != nil {
			c.DueDate = dates.Due
		}
		planned = &c
		m.add(moved, *dates, user)
	}
	m.moved = planned
	for _, s := range PlanShifts(append([]*dgraphStruct.DgraphTask{planned}, kept...), moved.Uuid, loc) {
		if s.before == nil || helpers.IsSoftDeleted(s.before.DeletedAt) {
			continue
		}
		if m.add(s.before, TaskDates{Start: s.Start, Due: s.Due}, user) {
			m.shifted = append(m.shifted, s)
		}
	}
	return m, nil
}

// add puts task's new dates in the move, guarded by its dates as read; false
// when there's nothing to write (no change, or not a task in the graph).
func (m *movePlan) add(task *dgraphStruct.DgraphTask, dates TaskDates, user *dgraphStruct.DgraphUser) bool {
	id, err := uuid.Parse(task.Uuid)
	if err != nil || !strings.HasPrefix(task.Uid, "0x") {
		return false
	}
	w := datesWrite(task.Uid, id, dates, task, user, m.at)
	if w == nil {
		return false
	}
	m.ids = append(m.ids, id)
	m.writes = append(m.writes, w)
	m.guards = append(m.guards, domain.MoveGuard{Uid: task.Uid, Start: task.StartDate, Due: task.DueDate})
	m.moves = append(m.moves, movedTask{before: task, dates: dates})
	return true
}

// shiftSeams are where a move reads, decides and writes: seams for tests.
var shiftSeams = struct {
	schedule func(ctx context.Context, movedUUID string) ([]*dgraphStruct.DgraphTask, error)
	judge    func(ctx context.Context, userUUID, userUID string, ts ...*dgraphStruct.DgraphTask) (map[string]workAccess.Verdict, error)
	commit   func(ctx context.Context, ids []uuid.UUID, at time.Time, writes []*dgraphStruct.DgraphTask, guards []domain.MoveGuard) error
	after    func(ctx context.Context, moves []movedTask, user *dgraphStruct.DgraphUser)
}{
	schedule: domain.DownstreamSchedule,
	judge:    workAccess.JudgeTasks,
	commit:   domain.CommitMove,
	after:    afterMove,
}

// moveChanged is how a move tells the rules and webhooks watching a task's
// due date (task.changed) that it moved: a seam for tests.
var moveChanged = dispatchTaskChanged

// afterMove tells what follows a move (each moved task as it was before it,
// with its new dates): the rules and webhooks watching its due date
// (task.changed, as afterDates tells them of one task's), its assignee's
// calendar, and one message per project for everyone with it open. Off the
// request.
func afterMove(ctx context.Context, moves []movedTask, user *dgraphStruct.DgraphUser) {
	byProject := map[string][]mqttStruct.MqttTaskDate{}
	var order []string
	for _, mv := range moves {
		t := mv.before
		if mv.dates.Due != nil {
			moveChanged(ctx, t, FieldDue, dayOf(t.DueDate), dayOf(mv.dates.Due), user)
		}
		if t.Assignee != nil && t.Assignee.Uuid != "" {
			taskUUID, assignee := t.Uuid, t.Assignee.Uuid
			helpers.GoSafeNamed("Task.afterMove.calendar", func() {
				integrationBusiness.SyncTaskToGoogleCalendar(context.Background(), taskUUID, assignee)
			})
		}
		if t.Project == nil || t.Project.Uuid == "" {
			continue
		}
		start, due := t.StartDate, t.DueDate
		if mv.dates.Start != nil {
			start = mv.dates.Start
		}
		if mv.dates.Due != nil {
			due = mv.dates.Due
		}
		p := t.Project.Uuid
		if _, ok := byProject[p]; !ok {
			order = append(order, p)
		}
		byProject[p] = append(byProject[p], mqttStruct.MqttTaskDate{TaskUuid: t.Uuid, StartDate: wireDate(start), DueDate: wireDate(due)})
	}
	for _, p := range order {
		project, tasks := p, byProject[p]
		helpers.GoSafeNamed("Task.afterMove.dates", func() {
			mqttBusiness.PublishTaskDatesMany(project, user.Uuid, tasks)
		})
	}
}

func dated(t *time.Time) bool { return t != nil && t.Year() > 1970 }

// lastDay is the day a task is done by: its due date, else its start.
func lastDay(t *dgraphStruct.DgraphTask, loc *time.Location) (topo.Day, bool) {
	if dated(t.DueDate) {
		return topo.CivilDay(*t.DueDate, loc), true
	}
	if dated(t.StartDate) {
		return topo.CivilDay(*t.StartDate, loc), true
	}
	return 0, false
}

// firstDay is the day a task starts: its start date, else its due date.
func firstDay(t *dgraphStruct.DgraphTask, loc *time.Location) (topo.Day, bool) {
	if dated(t.StartDate) {
		return topo.CivilDay(*t.StartDate, loc), true
	}
	if dated(t.DueDate) {
		return topo.CivilDay(*t.DueDate, loc), true
	}
	return 0, false
}

func closed(t *dgraphStruct.DgraphTask) bool {
	return t.Status == dgraphStruct.TASK_STATUS_DONE || t.Status == dgraphStruct.TASK_STATUS_CANCELED
}

// topoTaskOf is t as the planner takes it, in loc's days.
func topoTaskOf(t *dgraphStruct.DgraphTask, loc *time.Location) topo.Task {
	first, ok := firstDay(t, loc)
	last, _ := lastDay(t, loc)
	return topo.Task{ID: t.Uuid, Start: first, End: last, Dated: ok, Closed: closed(t)}
}

// scheduleOf is tasks as the planner (helpers/topo) takes them, in loc's
// days, with each link they hold to another of them; and the tasks by uuid.
func scheduleOf(tasks []*dgraphStruct.DgraphTask, loc *time.Location) (topo.Schedule, map[string]*dgraphStruct.DgraphTask) {
	byID := make(map[string]*dgraphStruct.DgraphTask, len(tasks))
	var s topo.Schedule
	for _, t := range tasks {
		if t == nil || byID[t.Uuid] != nil {
			continue
		}
		byID[t.Uuid] = t
		s.Tasks = append(s.Tasks, topoTaskOf(t, loc))
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		for _, b := range t.BlockedBy {
			if b != nil && b.Uuid != t.Uuid && byID[b.Uuid] != nil {
				s.Links = append(s.Links, topo.Link{From: b.Uuid, To: t.Uuid, Kind: topo.Kind(kindOf(b)), Lag: b.DependencyLag})
			}
		}
	}
	return s, byID
}

// PlanShifts is ShiftDependents' arithmetic: which tasks move, and to what
// dates, given the tasks waiting on movedUUID however indirectly (and it,
// with its new dates). The planner (topo.Shift) decides how many days each
// moves, in loc's days: a task moves only when a task it waits on moved and
// the dependency no longer holds, just far enough for all of them, keeping
// how long it runs. Here each moved date moves by those days in loc, keeping
// its time of day across a clock change. Nothing moves earlier; finished
// work neither moves nor holds up another; a task without dates stays; a
// loop never comes round; at most maxShifted tasks move.
func PlanShifts(tasks []*dgraphStruct.DgraphTask, movedUUID string, loc *time.Location) []ShiftedTask {
	s, byID := scheduleOf(tasks, loc)
	if byID[movedUUID] == nil {
		return nil
	}
	moves := topo.Shift(s, movedUUID, maxShifted)
	out := make([]ShiftedTask, 0, len(moves))
	for _, m := range moves {
		t := byID[m.ID]
		st := ShiftedTask{UUID: m.ID, before: t}
		if dated(t.StartDate) {
			v := t.StartDate.In(loc).AddDate(0, 0, m.Days)
			st.Start = &v
		}
		if dated(t.DueDate) {
			v := t.DueDate.In(loc).AddDate(0, 0, m.Days)
			st.Due = &v
		}
		out = append(out, st)
	}
	return out
}

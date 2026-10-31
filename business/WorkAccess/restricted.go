package business

// Restricted tasks (WA2, design/projects-vs-asana-jira/WA2_PLAN.md). Who may
// see one is the rule's LetIn: its project's admins, its assignee, its
// creator and the people granted on it. A task below a restricted one (a
// sub-task) is restricted with it, made before or after: whoever may see that
// one sees it too. hiddenFor decides it for one reader across every
// restricted task at once, as models/taskread's one filter; Restrict and
// Unrestrict change a task's restriction.

import (
	"context"
	"errors"
	"strings"
	"time"

	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

func init() {
	taskread.Decide = hiddenFor
}

// Seams for tests: the rows and the graph the decision reads, and who may be
// granted.
var (
	restrictionCandidates = restrictionModel.Candidates
	readStanding          = taskDomain.GetTasksStandingFor
	mayReadTask           = taskDomain.InProject
	tasksBelow            = taskDomain.GetTasksBelow
	restrict              = restrictionModel.Restrict
	inherit               = restrictionModel.Inherit
	unrestrict            = restrictionModel.Unrestrict
)

// hiddenFor is every restricted task, by lower-case uuid, true for those
// reader (a user uuid) may not see and false for those they may, which carry
// the lock (WA2b; models/taskread.Decide). Hidden are those its rows don't
// let them into (granted, or made by them, it or the task it is restricted
// with), less those they are the assignee of, or whose project they run, by
// the rule, and those restricted with a task they are the assignee of. For
// nobody (""), every restricted task is hidden. Two reads at most, and one
// when no restricted task is left out for the reader by its rows.
func hiddenFor(ctx context.Context, reader string) (map[string]bool, error) {
	return hidden(ctx, reader, true)
}

// hidden is hiddenFor, with the reader's assignments counted or not.
func hidden(ctx context.Context, reader string, assignments bool) (map[string]bool, error) {
	me, err := uuid.Parse(strings.TrimSpace(reader))
	if err != nil {
		me = uuid.Nil
	}
	rows, err := restrictionCandidates(ctx, me)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	hidden, candidates, ids := leftOut(me, rows)
	if len(candidates) == 0 {
		return hidden, nil
	}
	// The decision reads the restricted tasks it decides about: as a read
	// with no audience, or it would ask itself.
	tasks, err := readStanding(taskread.WithSystem(ctx), ids, me.String())
	if err != nil {
		return nil, err
	}
	standings, err := OfMany(ctx, me.String(), projectsOf(tasks))
	if err != nil {
		return nil, err
	}
	letInBy(me, hidden, candidates, tasks, standings, assignments)
	return hidden, nil
}

// leftOut starts the decision from the reader's rows: every restricted task
// hidden but those its rows let them into, the ones left out (candidates;
// none for nobody, who is left out of every one), and the tasks to read for
// them: theirs, and those they are restricted with. Pure.
func leftOut(me uuid.UUID, rows []restrictionModel.Candidate) (map[string]bool, []restrictionModel.Candidate, []string) {
	hidden := make(map[string]bool, len(rows))
	var candidates []restrictionModel.Candidate
	ids := make([]string, 0, len(rows))
	for _, c := range rows {
		if c.LetIn && me != uuid.Nil {
			hidden[c.TaskID.String()] = false
			continue
		}
		candidates = append(candidates, c)
		hidden[c.TaskID.String()] = true
		ids = append(ids, c.TaskID.String())
	}
	if me == uuid.Nil || len(candidates) == 0 {
		return hidden, nil, nil
	}
	// The tasks they're restricted with are read too, for their assignees.
	asked := map[string]bool{}
	for _, id := range ids {
		asked[id] = true
	}
	for _, c := range candidates {
		if v := c.Via.String(); c.Via != uuid.Nil && !asked[v] {
			asked[v] = true
			ids = append(ids, v)
		}
	}
	return hidden, candidates, ids
}

// projectsOf is the projects of the tasks read for the decision. Pure.
func projectsOf(tasks []*dgraphStruct.DgraphTask) []*dgraphStruct.DgraphProject {
	var projects []*dgraphStruct.DgraphProject
	for _, t := range tasks {
		if t != nil && t.Project != nil {
			projects = append(projects, t.Project)
		}
	}
	return projects
}

// letInBy settles the candidates in hidden: one shows when the rule lets the
// reader in as they stand in its project (as its assignee too when
// assignments count), or when the task it is restricted with lets them in,
// by its rows or the rule. Pure.
func letInBy(me uuid.UUID, hidden map[string]bool, candidates []restrictionModel.Candidate, tasks []*dgraphStruct.DgraphTask, standings map[string]Standing, assignments bool) {
	is := func(u *dgraphStruct.DgraphUser) bool { return u != nil && strings.EqualFold(u.Uuid, me.String()) }
	letIn := map[string]bool{}
	for _, t := range tasks {
		if t == nil || t.Uuid == "" {
			continue
		}
		var s Standing
		if t.Project != nil {
			s = standings[strings.ToLower(t.Project.Uuid)]
		}
		letIn[strings.ToLower(t.Uuid)] = s.LetIn(TaskFacts{Restricted: true, Assignee: assignments && is(t.Assignee), Creator: is(t.CreatedBy)})
	}
	for _, c := range candidates {
		id := c.TaskID.String()
		// The task it is restricted with lets them in when its rows do (it
		// is no candidate), or the rule does.
		through := c.Via != uuid.Nil && (!hidden[c.Via.String()] || letIn[c.Via.String()])
		if letIn[id] || through {
			hidden[id] = false
		}
	}
}

// ErrNotProjectPeople is a grant to someone who can't see the task's
// project: a restricted task is let into only by its project's people.
var ErrNotProjectPeople = errors.New("only people in the task's project can be let into it")

// ErrNoProject is a task in no project: there is nobody to keep it from.
var ErrNoProject = errors.New("a task in no project is its maker's already")

// MayRestrict is whether someone with standing s and facts f may restrict the
// task or open it again: its project's admins, and its creator while they may
// change it. Pure.
func (s Standing) MayRestrict(f TaskFacts) bool {
	return s.Can(NeedManage) || (f.Creator && s.CanOnTask(NeedEditTask, f))
}

// Restrict keeps task to its project's admins, its assignee, its creator and
// people (user uuids, replacing any granted before), by by, who may
// (MayRestrict; the caller checks it, as it words the refusal), and with it
// the tasks below it that aren't restricted already: whoever may see it sees
// those, and opening it opens them again. Each of people must be one of the
// project's people; the first who isn't is named in the error.
func Restrict(ctx context.Context, task *dgraphStruct.DgraphTask, by uuid.UUID, people []uuid.UUID, now time.Time) error {
	if task == nil || task.Project == nil || task.Project.Uuid == "" {
		return ErrNoProject
	}
	taskID, err := uuid.Parse(task.Uuid)
	if err != nil {
		return err
	}
	projectID, err := uuid.Parse(task.Project.Uuid)
	if err != nil {
		return err
	}
	if len(people) > 0 {
		asked := make([]string, len(people))
		for i, p := range people {
			asked[i] = p.String()
		}
		readers, err := mayReadTask(ctx, task.Uuid, asked)
		if err != nil {
			return err
		}
		for _, p := range asked {
			if !readers[p] {
				return &NotProjectPeopleError{UserUUID: p}
			}
		}
	}
	// The tasks below it, as the person restricting it sees them: one kept
	// from them is restricted already, and keeps its own people.
	below, err := tasksBelow(ctx, task.Uuid)
	if err != nil {
		return err
	}
	under := make([]uuid.UUID, 0, len(below))
	for _, id := range below {
		if parsed, err := uuid.Parse(id); err == nil {
			under = append(under, parsed)
		}
	}
	return restrict(ctx, taskID, projectID, by, people, under, now)
}

// NotProjectPeopleError names the person a grant was refused for.
type NotProjectPeopleError struct{ UserUUID string }

func (e *NotProjectPeopleError) Error() string { return ErrNotProjectPeople.Error() }
func (e *NotProjectPeopleError) Unwrap() error { return ErrNotProjectPeople }

// Unrestrict opens task to its project again; false when it wasn't
// restricted.
func Unrestrict(ctx context.Context, task *dgraphStruct.DgraphTask) (bool, error) {
	if task == nil {
		return false, ErrNoProject
	}
	taskID, err := uuid.Parse(task.Uuid)
	if err != nil {
		return false, err
	}
	return unrestrict(ctx, taskID)
}

type likeKey struct{}

// RestrictedLike makes a task created through ctx kept to the same people as
// from, when from is restricted: a repeating task's next copy. CreateTask
// settles it (SettleRestriction) before anyone is told of the new task, so
// no notice or live update of it reaches someone the copy is kept from.
func RestrictedLike(ctx context.Context, from uuid.UUID) context.Context {
	return context.WithValue(ctx, likeKey{}, from)
}

// CopiesRestricted is whether a task made through ctx is a copy of another
// (RestrictedLike), and so may have to be kept to that one's people.
func CopiesRestricted(ctx context.Context) bool {
	like, _ := ctx.Value(likeKey{}).(uuid.UUID)
	return like != uuid.Nil
}

// SettleRestriction keeps a task just made (taskID) to the same people as the
// task it comes from: from, a sub-task's parent, which it is restricted with
// (whoever may see the parent sees it); or else the task ctx names
// (RestrictedLike), a repeat's last copy, whose people it is restricted to on
// its own. Nothing when that one isn't restricted. The project is the same,
// so the people are its people still.
func SettleRestriction(ctx context.Context, taskID, from, by uuid.UUID, now time.Time) error {
	if taskID == uuid.Nil {
		return nil
	}
	if from != uuid.Nil {
		return inherit(ctx, taskID, from, by, now)
	}
	like, _ := ctx.Value(likeKey{}).(uuid.UUID)
	if like == uuid.Nil {
		return nil
	}
	got, err := restrictionsOf(ctx, []uuid.UUID{like})
	if err != nil {
		return err
	}
	r, ok := got[like]
	if !ok || !r.Restricted {
		return nil
	}
	return restrict(ctx, taskID, r.ProjectID, by, r.Grants, nil, now)
}

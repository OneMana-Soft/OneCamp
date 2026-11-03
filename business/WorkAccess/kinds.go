package business

// The work kinds of the Access Resolver (PROGRAM.md §5): a project, a task, a
// saved view, a cycle and a form, each decided by the rule (rule.go) over what
// one read per kind says of everyone asked about, so a check made through the
// resolver answers as a handler's does.
//
//   - a project: its standing (OfMany, one read of the person's roles);
//   - a task: its project's standing and its own facts; a deleted task
//     answers none, with Was what it was;
//   - a saved view (today's task_views): its owner's alone, in full; a
//     deleted view answers none, with Was what it was;
//   - a cycle and a form: their project's, read only below its admins, who
//     change them; a deleted one answers none, with Was what it was.
//
// Each ends on Decision.Settled, the one rule for a deleted resource (I9).

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	"github.com/google/uuid"
)

// The kinds, as resources name them.
const (
	KindProject = "project"
	KindTask    = "task"
	KindView    = "view"
	KindCycle   = "cycle"
	KindForm    = "form"
)

func init() {
	accessBusiness.RegisterKind(KindProject, projectRules{})
	accessBusiness.RegisterKind(KindTask, taskRules{})
	accessBusiness.RegisterKind(KindView, viewRules{})
	accessBusiness.RegisterKind(KindCycle, cycleRules{})
	accessBusiness.RegisterKind(KindForm, formRules{})
}

// Seams for tests: the reads each kind decides by, one per kind.
var (
	readProjects = projectDomain.GetProjectsReadAccessFor
	readTasks    = taskDomain.GetTasksReadAccessFor
	viewOwners   = taskViewModel.OwnersOf
	heldBy       = softDelete.OwnersOf
	uidOf        = accessBusiness.UIDOf
)

// ------------------------------------------------------------------ projects

type projectRules struct{}

func (projectRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	standings, err := projectStandings(ctx, who, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]accessBusiness.Decision, len(ids))
	for _, id := range ids {
		if s, ok := standings[strings.ToLower(id)]; ok {
			out[id] = projectDecision(s)
		}
	}
	return out, nil
}

// projectStandings is who's standing on each project there is of ids, by
// lower-case id, in one read. Someone who isn't an active person stands
// nowhere.
func projectStandings(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Standing, error) {
	out := map[string]Standing{}
	uid, err := uidOf(ctx, who)
	if err != nil || uid == "" || len(ids) == 0 {
		return out, err
	}
	projects, err := readProjects(ctx, ids, uid)
	if err != nil {
		return nil, err
	}
	return OfMany(ctx, who.UserUUID, projects)
}

// ProjectsViewable is which of ids (project uuids) the person userUUID (graph
// uid userUID) may see, by lower-case id: the check a project's own routes
// make of one (NeedView), for many in one read of the projects and one of
// their roles, so a read across projects asks once however many it names.
// Ids that name no project are left out.
func ProjectsViewable(ctx context.Context, userUUID, userUID string, ids []string) (map[string]bool, error) {
	standings, err := projectStandings(ctx, accessBusiness.Subject{UserUUID: userUUID, UserUID: userUID}, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(standings))
	for key, s := range standings {
		if s.Can(NeedView) {
			out[key] = true
		}
	}
	return out, nil
}

// projectDecision is the resolver's decision for standing s on a project.
// Pure.
func projectDecision(s Standing) accessBusiness.Decision {
	d := accessBusiness.Decision{Level: s.ProjectLevel(), Archived: s.Archived}
	d.Was = d.Level
	if d.Level > accessBusiness.LevelNone {
		d.Reasons = reasonsFor(s, nil)
	}
	return d
}

// --------------------------------------------------------------------- tasks

type taskRules struct{}

func (taskRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := make(map[string]accessBusiness.Decision, len(ids))
	uid, err := uidOf(ctx, who)
	if err != nil || uid == "" || len(ids) == 0 {
		return out, err
	}
	tasks, err := readTasks(ctx, ids, uid)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*dgraphStruct.DgraphTask, len(tasks))
	var projects []*dgraphStruct.DgraphProject
	for _, t := range tasks {
		if t != nil && t.Uuid != "" {
			byID[strings.ToLower(t.Uuid)] = t
			if t.Project != nil {
				projects = append(projects, t.Project)
			}
		}
	}
	// One read of the person's roles for every task's project, and one of
	// the tasks' restrictions.
	standings, err := OfMany(ctx, who.UserUUID, projects)
	if err != nil {
		return nil, err
	}
	facts, err := FactsForMany(ctx, who.UserUUID, uid, tasks...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		t, ok := byID[strings.ToLower(id)]
		if !ok {
			continue
		}
		var s Standing
		if t.Project != nil && (t.Project.Uuid != "" || t.Project.Uid != "") {
			s = standings[strings.ToLower(t.Project.Uuid)]
		}
		out[id] = taskDecision(s, facts[strings.ToLower(t.Uuid)])
	}
	return out, nil
}

// taskDecision is the resolver's decision for standing s and facts f on a
// task: none while it is deleted, with Was what it was. Pure.
func taskDecision(s Standing, f TaskFacts) accessBusiness.Decision {
	d := accessBusiness.Decision{Level: s.TaskLevel(f), Archived: s.Archived}
	if d.Level > accessBusiness.LevelNone {
		d.Reasons = reasonsFor(s, &f)
	}
	return d.Settled(f.Deleted)
}

// ---------------------------------------------------------------- saved views

type viewRules struct{}

func (viewRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	owners, err := viewOwners(ctx, parseIDs(ids))
	if err != nil {
		return nil, err
	}
	me, _ := uuid.Parse(who.UserUUID)
	out := make(map[string]accessBusiness.Decision, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if v, ok := owners[parsed]; ok {
			out[id] = viewDecision(me, v)
		}
	}
	return out, nil
}

// viewDecision is the resolver's decision for the person me on a saved view:
// its owner's alone, in full; none while it is deleted, with Was what it was,
// as a deleted task's (I9). Pure.
func viewDecision(me uuid.UUID, v taskViewModel.Owner) accessBusiness.Decision {
	var d accessBusiness.Decision
	if me != uuid.Nil && v.Person == me {
		d = accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, Reasons: []string{ReasonOwner}}
	}
	return d.Settled(v.Deleted)
}

// ------------------------------------------------------------ cycles, forms

type cycleRules struct{}

func (cycleRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return byProject(ctx, who, ids, softDelete.Cycle)
}

type formRules struct{}

func (formRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return byProject(ctx, who, ids, softDelete.Form)
}

// byProject decides the things of kind that belong to a project (a cycle, a
// form) by their project, from one read of them live or deleted: its admins
// change them, the rest of it reads them, and a deleted one answers none
// with Was what it was. One with no project (a table's own form) is left
// out, as one that isn't there is.
func byProject(ctx context.Context, who accessBusiness.Subject, ids []string, kind softDelete.Kind) (map[string]accessBusiness.Decision, error) {
	held, err := heldBy(ctx, kind, parseIDs(ids))
	if err != nil {
		return nil, err
	}
	projectIDs := make([]string, 0, len(held))
	for _, h := range held {
		if h.Project != uuid.Nil {
			projectIDs = append(projectIDs, h.Project.String())
		}
	}
	standings, err := projectStandings(ctx, who, projectIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]accessBusiness.Decision, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if h, ok := held[parsed]; ok && h.Project != uuid.Nil {
			out[id] = projectThingDecision(standings[h.Project.String()], h.Deleted)
		}
	}
	return out, nil
}

// projectThingDecision is the resolver's decision for standing s on a thing
// of its project (a cycle, a form): full for its admins, view for the rest
// of it; none while it is deleted, with Was what it was. Pure.
func projectThingDecision(s Standing, deleted bool) accessBusiness.Decision {
	d := accessBusiness.Decision{Archived: s.Archived}
	switch {
	case s.Can(NeedManage):
		d.Level, d.Reasons = accessBusiness.LevelFull, []string{ReasonAdmin}
	case s.Can(NeedView):
		d.Level, d.Reasons = accessBusiness.LevelView, []string{ReasonMember}
	}
	return d.Settled(deleted)
}

// parseIDs is the ids that are uuids. Pure.
func parseIDs(ids []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if parsed, err := uuid.Parse(id); err == nil {
			out = append(out, parsed)
		}
	}
	return out
}

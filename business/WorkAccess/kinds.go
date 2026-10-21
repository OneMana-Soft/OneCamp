package business

// The work kinds of the Access Resolver (PROGRAM.md §5): a project, a task, a
// saved view, a cycle and a form, each decided by the rule (rule.go) over what
// one read per kind says of everyone asked about, so a check made through the
// resolver answers as a handler's does.
//
//   - a project: its standing (OfMany, one read of the person's roles);
//   - a task: its project's standing and its own facts; a deleted task
//     answers none, with Was what it was;
//   - a saved view (today's task_views): its owner's alone, in full;
//   - a cycle and a form: their project's, read only below its admins, who
//     change them.

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
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
	readProjects  = projectDomain.GetProjectsReadAccessFor
	readTasks     = taskDomain.GetTasksReadAccessFor
	viewOwners    = taskViewModel.OwnersOf
	cycleProjects = cycleModel.ProjectsOf
	formProjects  = formModel.ProjectsOf
	uidOf         = accessBusiness.UIDOf
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
	d.Was = d.Level
	if d.Level > accessBusiness.LevelNone {
		d.Reasons = reasonsFor(s, &f)
	}
	if f.Deleted {
		d.Deleted, d.Level = true, accessBusiness.LevelNone
	}
	return d
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
		owner, ok := owners[parsed]
		if !ok {
			continue
		}
		d := accessBusiness.Decision{}
		if me != uuid.Nil && owner == me {
			d = accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, Reasons: []string{ReasonOwner}}
		}
		d.Was = d.Level
		out[id] = d
	}
	return out, nil
}

// ------------------------------------------------------------ cycles, forms

type cycleRules struct{}

func (cycleRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return byProject(ctx, who, ids, cycleProjects)
}

type formRules struct{}

func (formRules) DecideMany(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	return byProject(ctx, who, ids, formProjects)
}

// byProject decides things that belong to a project (a cycle, a form) by
// their project: its admins change them, the rest of it reads them.
func byProject(ctx context.Context, who accessBusiness.Subject, ids []string, projectsOf func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)) (map[string]accessBusiness.Decision, error) {
	owners, err := projectsOf(ctx, parseIDs(ids))
	if err != nil {
		return nil, err
	}
	projectIDs := make([]string, 0, len(owners))
	for _, p := range owners {
		projectIDs = append(projectIDs, p.String())
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
		project, ok := owners[parsed]
		if !ok {
			continue
		}
		s := standings[project.String()]
		d := accessBusiness.Decision{Archived: s.Archived}
		switch {
		case s.Can(NeedManage):
			d.Level, d.Reasons = accessBusiness.LevelFull, []string{ReasonAdmin}
		case s.Can(NeedView):
			d.Level, d.Reasons = accessBusiness.LevelView, []string{ReasonMember}
		}
		d.Was = d.Level
		out[id] = d
	}
	return out, nil
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

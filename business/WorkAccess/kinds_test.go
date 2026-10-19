package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// world is the graph and the tables the kinds read, as one person sees them:
// their role rows, and every project's "Who can change tasks" (members, the
// default, unless policy says otherwise).
type world struct {
	projects []*dgraphStruct.DgraphProject
	tasks    []*dgraphStruct.DgraphTask
	views    map[uuid.UUID]uuid.UUID
	cycles   map[uuid.UUID]uuid.UUID
	forms    map[uuid.UUID]uuid.UUID
	roles    map[uuid.UUID]string
	policy   string
}

func (w *world) use(t *testing.T) {
	t.Helper()
	policy := w.policy
	if policy == "" {
		policy = EditMembers
	}
	fakeRoles(t, w.roles, policy)
	old := []any{readProjects, readTasks, viewOwners, cycleProjects, formProjects, uidOf}
	t.Cleanup(func() {
		readProjects = old[0].(func(context.Context, []string, string) ([]*dgraphStruct.DgraphProject, error))
		readTasks = old[1].(func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error))
		viewOwners = old[2].(func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error))
		cycleProjects = old[3].(func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error))
		formProjects = old[4].(func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error))
		uidOf = old[5].(func(context.Context, accessBusiness.Subject) (string, error))
	})
	readProjects = func(_ context.Context, ids []string, _ string) ([]*dgraphStruct.DgraphProject, error) {
		var out []*dgraphStruct.DgraphProject
		for _, p := range w.projects {
			for _, id := range ids {
				if id == p.Uuid {
					out = append(out, p)
				}
			}
		}
		return out, nil
	}
	readTasks = func(_ context.Context, _ []string, _ string) ([]*dgraphStruct.DgraphTask, error) { return w.tasks, nil }
	viewOwners = func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) { return w.views, nil }
	cycleProjects = func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) { return w.cycles, nil }
	formProjects = func(context.Context, []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) { return w.forms, nil }
	uidOf = func(_ context.Context, who accessBusiness.Subject) (string, error) { return who.UserUID, nil }
}

func resolve(t *testing.T, who accessBusiness.Subject, kind, id string) (accessBusiness.Decision, bool) {
	t.Helper()
	got, err := accessBusiness.ResolveAll(context.Background(), who, []accessBusiness.Resource{{Kind: kind, UUID: id}})
	if err != nil {
		t.Fatalf("%s %s: %v", kind, id, err)
	}
	d, ok := got[accessBusiness.Resource{Kind: kind, UUID: id}]
	return d, ok
}

// Each work kind answers through the resolver by the rule.
func TestTheWorkKindsAnswerByTheRule(t *testing.T) {
	me := accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()}
	launch, archived := uuid.NewString(), uuid.NewString()
	gone := time.Now()
	w := &world{
		projects: []*dgraphStruct.DgraphProject{
			{Uuid: launch, IsProjectMember: 1},
			{Uuid: archived, IsProjectAdmin: 1, DeletedAt: &gone},
		},
	}
	w.use(t)

	d, ok := resolve(t, me, KindProject, launch)
	if !ok || d.Level != accessBusiness.LevelEdit || d.Was != d.Level || len(d.Reasons) != 1 || d.Reasons[0] != ReasonMember {
		t.Errorf("a member on the project: %+v %v", d, ok)
	}
	d, _ = resolve(t, me, KindProject, archived)
	if d.Level != accessBusiness.LevelFull || !d.Archived {
		t.Errorf("an admin on an archived project: %+v", d)
	}
	if _, ok := resolve(t, me, KindProject, uuid.NewString()); ok {
		t.Error("a project that isn't there is left out")
	}
	if _, ok := resolve(t, accessBusiness.Subject{}, KindProject, launch); ok {
		t.Error("someone who isn't a person stands nowhere")
	}
}

// A task: an admin full, a member edit, a commenter comment, the assignee
// outside its project view; a deleted task none, with what it was.
func TestTheTaskKindAnswersByTheRule(t *testing.T) {
	me := accessBusiness.Subject{UserUID: "0x1", UserUUID: uuid.NewString()}
	mine := &dgraphStruct.DgraphUser{Uid: "0x1"}
	gone := time.Now()
	run, work, talk, assigned, deleted := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	w := &world{tasks: []*dgraphStruct.DgraphTask{
		{Uuid: run, Project: &dgraphStruct.DgraphProject{Uuid: a.String(), IsProjectAdmin: 1}},
		{Uuid: work, Project: &dgraphStruct.DgraphProject{Uuid: b.String(), IsProjectMember: 1}},
		{Uuid: talk, Project: &dgraphStruct.DgraphProject{Uuid: d.String(), IsProjectMember: 1}},
		{Uuid: assigned, Assignee: mine, Project: &dgraphStruct.DgraphProject{Uuid: c.String()}},
		{Uuid: deleted, DeletedAt: &gone, Project: &dgraphStruct.DgraphProject{Uuid: b.String(), IsProjectMember: 1}},
	}, roles: map[uuid.UUID]string{d: "commenter"}}
	w.use(t)
	for id, want := range map[string]accessBusiness.Level{run: accessBusiness.LevelFull, work: accessBusiness.LevelEdit, talk: accessBusiness.LevelComment, assigned: accessBusiness.LevelView} {
		if d, ok := resolve(t, me, KindTask, id); !ok || d.Level != want || d.Deleted {
			t.Errorf("%s: %+v %v, want %s", id, d, ok, want)
		}
	}
	if d, _ := resolve(t, me, KindTask, assigned); len(d.Reasons) != 1 || d.Reasons[0] != ReasonAssignee {
		t.Errorf("the assignee's reasons: %v", d.Reasons)
	}
	if d, ok := resolve(t, me, KindTask, deleted); !ok || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelEdit {
		t.Errorf("a deleted task: %+v %v", d, ok)
	}
}

// A saved view is its owner's; a cycle and a form are their project's, read
// by its members and changed by its admins.
func TestViewsCyclesAndFormsAnswerByTheirOwners(t *testing.T) {
	me := uuid.New()
	who := accessBusiness.Subject{UserUID: "0x1", UserUUID: me.String()}
	run, work, outside := uuid.New(), uuid.New(), uuid.New()
	myView, theirView := uuid.New(), uuid.New()
	adminCycle, memberForm, outsideCycle := uuid.New(), uuid.New(), uuid.New()
	w := &world{
		projects: []*dgraphStruct.DgraphProject{
			{Uuid: run.String(), IsProjectAdmin: 1},
			{Uuid: work.String(), IsProjectMember: 1},
			{Uuid: outside.String()},
		},
		views:  map[uuid.UUID]uuid.UUID{myView: me, theirView: uuid.New()},
		cycles: map[uuid.UUID]uuid.UUID{adminCycle: run, outsideCycle: outside},
		forms:  map[uuid.UUID]uuid.UUID{memberForm: work},
	}
	w.use(t)
	for _, c := range []struct {
		kind  string
		id    uuid.UUID
		level accessBusiness.Level
	}{
		{KindView, myView, accessBusiness.LevelFull},
		{KindView, theirView, accessBusiness.LevelNone},
		{KindCycle, adminCycle, accessBusiness.LevelFull},
		{KindCycle, outsideCycle, accessBusiness.LevelNone},
		{KindForm, memberForm, accessBusiness.LevelView},
	} {
		if d, ok := resolve(t, who, c.kind, c.id.String()); !ok || d.Level != c.level {
			t.Errorf("%s %s: %+v %v, want %s", c.kind, c.id, d, ok, c.level)
		}
	}
	if _, ok := resolve(t, who, KindCycle, uuid.NewString()); ok {
		t.Error("a cycle that isn't there is left out")
	}
}

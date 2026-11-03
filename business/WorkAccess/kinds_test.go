package business

import (
	"context"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	taskViewModel "github.com/akashc777/OneCamp/models/postgres/TaskView"
	"github.com/google/uuid"
)

// world is the graph and the tables the kinds read, as one person sees them:
// their role rows, and every project's "Who can change tasks" (members, the
// default, unless policy says otherwise).
type world struct {
	projects []*dgraphStruct.DgraphProject
	tasks    []*dgraphStruct.DgraphTask
	views    map[uuid.UUID]taskViewModel.Owner
	cycles   map[uuid.UUID]softDelete.Held
	forms    map[uuid.UUID]softDelete.Held
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
	old := []any{readProjects, readTasks, viewOwners, heldBy, uidOf}
	t.Cleanup(func() {
		readProjects = old[0].(func(context.Context, []string, string) ([]*dgraphStruct.DgraphProject, error))
		readTasks = old[1].(func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error))
		viewOwners = old[2].(func(context.Context, []uuid.UUID) (map[uuid.UUID]taskViewModel.Owner, error))
		heldBy = old[3].(func(context.Context, softDelete.Kind, []uuid.UUID) (map[uuid.UUID]softDelete.Held, error))
		uidOf = old[4].(func(context.Context, accessBusiness.Subject) (string, error))
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
	viewOwners = func(context.Context, []uuid.UUID) (map[uuid.UUID]taskViewModel.Owner, error) { return w.views, nil }
	heldBy = func(_ context.Context, kind softDelete.Kind, _ []uuid.UUID) (map[uuid.UUID]softDelete.Held, error) {
		return map[softDelete.Kind]map[uuid.UUID]softDelete.Held{softDelete.Cycle: w.cycles, softDelete.Form: w.forms}[kind], nil
	}
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

// A saved view is its owner's, and a deleted one none to everyone, its owner
// with what it was (I9, as a deleted task); a cycle and a form are their
// project's, read by its members and changed by its admins, and a deleted
// one is none to everyone, with what it was (V-324).
func TestViewsCyclesAndFormsAnswerByTheirOwners(t *testing.T) {
	me := uuid.New()
	who := accessBusiness.Subject{UserUID: "0x1", UserUUID: me.String()}
	run, work, outside := uuid.New(), uuid.New(), uuid.New()
	myView, theirView, myDeleted, theirDeleted := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	adminCycle, memberForm, outsideCycle := uuid.New(), uuid.New(), uuid.New()
	deletedCycle, deletedForm, outsideDeleted, tableForm := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	of := func(project uuid.UUID, deleted bool) softDelete.Held {
		return softDelete.Held{Owner: softDelete.Owner{Project: project}, Deleted: deleted}
	}
	w := &world{
		projects: []*dgraphStruct.DgraphProject{
			{Uuid: run.String(), IsProjectAdmin: 1},
			{Uuid: work.String(), IsProjectMember: 1},
			{Uuid: outside.String()},
		},
		views: map[uuid.UUID]taskViewModel.Owner{
			myView: {Person: me}, theirView: {Person: uuid.New()},
			myDeleted: {Person: me, Deleted: true}, theirDeleted: {Person: uuid.New(), Deleted: true},
		},
		cycles: map[uuid.UUID]softDelete.Held{
			adminCycle: of(run, false), outsideCycle: of(outside, false),
			deletedCycle: of(run, true), outsideDeleted: of(outside, true),
		},
		forms: map[uuid.UUID]softDelete.Held{
			memberForm: of(work, false), deletedForm: of(work, true),
			tableForm: {Owner: softDelete.Owner{Table: uuid.New()}},
		},
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
		if d, ok := resolve(t, who, c.kind, c.id.String()); !ok || d.Level != c.level || d.Deleted {
			t.Errorf("%s %s: %+v %v, want %s", c.kind, c.id, d, ok, c.level)
		}
	}
	if d, ok := resolve(t, who, KindView, myDeleted.String()); !ok || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelFull || !d.Owner {
		t.Errorf("my deleted view: %+v %v; want none, deleted, with Was full and still mine", d, ok)
	}
	if d, ok := resolve(t, who, KindView, theirDeleted.String()); !ok || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != accessBusiness.LevelNone || d.Owner {
		t.Errorf("someone else's deleted view: %+v %v; want none, deleted, with Was none", d, ok)
	}
	if _, ok := resolve(t, who, KindView, uuid.NewString()); ok {
		t.Error("a view that isn't there is left out")
	}
	if _, ok := resolve(t, who, KindCycle, uuid.NewString()); ok {
		t.Error("a cycle that isn't there is left out")
	}
	for _, c := range []struct {
		kind string
		id   uuid.UUID
		was  accessBusiness.Level
	}{
		{KindCycle, deletedCycle, accessBusiness.LevelFull},
		{KindCycle, outsideDeleted, accessBusiness.LevelNone},
		{KindForm, deletedForm, accessBusiness.LevelView},
	} {
		if d, ok := resolve(t, who, c.kind, c.id.String()); !ok || !d.Deleted || d.Level != accessBusiness.LevelNone || d.Was != c.was {
			t.Errorf("a deleted %s: %+v %v; want none, deleted, with Was %s", c.kind, d, ok, c.was)
		}
	}
	if _, ok := resolve(t, who, KindForm, tableForm.String()); ok {
		t.Error("a table's own form (no project) is left out")
	}
}

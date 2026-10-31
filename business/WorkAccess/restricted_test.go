package business

import (
	"context"
	"errors"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// WA2: a restricted task is for its project's admins, its assignee, its
// creator and the people granted on it; to anyone else it isn't there.
func TestARestrictedTaskLetsInOnlyItsPeople(t *testing.T) {
	restricted := TaskFacts{Restricted: true}
	cases := []struct {
		name  string
		s     Standing
		f     TaskFacts
		sees  bool
		edits bool
	}{
		{"an admin", Standing{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditMembers}, restricted, true, true},
		{"a member", Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, restricted, false, false},
		{"a member it's assigned to", Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, TaskFacts{Restricted: true, Assignee: true}, true, true},
		{"a member who made it", Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, TaskFacts{Restricted: true, Creator: true}, true, true},
		{"a member granted on it", Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, TaskFacts{Restricted: true, Granted: true}, true, true},
		{"a viewer granted on it, who still only reads", Standing{ProjectUUID: "p", Role: RoleViewer, EditWho: EditMembers}, TaskFacts{Restricted: true, Granted: true}, true, false},
		{"a commenter", Standing{ProjectUUID: "p", Role: RoleCommenter, EditWho: EditMembers}, restricted, false, false},
		{"its team's admin, who reads no work", Standing{ProjectUUID: "p", Steward: true}, restricted, false, false},
		{"an admin through a token, a member there", Standing{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditMembers}.asToken(), restricted, false, false},
		{"anyone, on a task that isn't restricted", Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}, TaskFacts{}, true, true},
	}
	for _, c := range cases {
		if got := c.s.CanOnTask(NeedView, c.f); got != c.sees {
			t.Errorf("%s: sees %v, want %v", c.name, got, c.sees)
		}
		if got := c.s.CanOnTask(NeedEditTask, c.f); got != c.edits {
			t.Errorf("%s: changes %v, want %v", c.name, got, c.edits)
		}
		if !c.sees {
			for _, n := range allNeeds {
				if c.s.CanOnTask(n, c.f) {
					t.Errorf("%s: may do %d with a task they can't see", c.name, n)
				}
			}
		}
	}
	// The resolver's level follows: nothing for whoever isn't let in.
	if lvl := (Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}).TaskLevel(restricted); lvl != 0 {
		t.Fatalf("a member's level on a restricted task: %v", lvl)
	}
}

func TestWhoMayRestrictATask(t *testing.T) {
	admin := Standing{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditMembers}
	member := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}
	commenter := Standing{ProjectUUID: "p", Role: RoleCommenter, EditWho: EditMembers}
	if !admin.MayRestrict(TaskFacts{}) || member.MayRestrict(TaskFacts{}) {
		t.Fatal("its project's admins may, other members may not")
	}
	if !member.MayRestrict(TaskFacts{Creator: true}) {
		t.Fatal("its creator may, while they may change it")
	}
	if commenter.MayRestrict(TaskFacts{Creator: true}) {
		t.Fatal("a creator who can no longer change it may not")
	}
	if (Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAdmins}).MayRestrict(TaskFacts{Creator: true}) {
		t.Fatal("with admins only changing tasks, its creator may not")
	}
	// The panel offers the control from task_access, never a guess (WA2b).
	if !admin.TaskAccess(TaskFacts{}).Restrict || member.TaskAccess(TaskFacts{}).Restrict || !member.TaskAccess(TaskFacts{Creator: true}).Restrict {
		t.Fatal("task_access.restrict says MayRestrict")
	}
}

// The filter's decision: the rows leave out what grants and making let in;
// the graph and the rule leave out what being its assignee or running its
// project lets in. Nobody sees every restricted task hidden.
func TestTheFilterHidesWhatTheRuleKeepsFromEachReader(t *testing.T) {
	me := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000aa")
	mine := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000001")    // assigned to me
	run := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000002")     // in a project I run
	kept := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000003")    // kept from me
	missing := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000004") // the graph has no such task
	// Sub-tasks restricted with a task above them: with the one assigned to
	// me, with the one kept from me, and with one whose rows let me in (no
	// candidate: I'm granted on it).
	underMine := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000005")
	underKept := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000006")
	underGranted := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000007")
	granted := uuid.MustParse("7a5c00a8-0000-4000-8000-000000000008")
	projectA, projectB := uuid.New(), uuid.New()
	oldC, oldR, oldRoles, oldWho, oldTok := restrictionCandidates, readStanding, rolesIn, editWho, viaToken
	t.Cleanup(func() {
		restrictionCandidates, readStanding, rolesIn, editWho, viaToken = oldC, oldR, oldRoles, oldWho, oldTok
	})
	var askedFor uuid.UUID
	restrictionCandidates = func(_ context.Context, reader uuid.UUID) ([]restrictionModel.Candidate, error) {
		askedFor = reader
		return []restrictionModel.Candidate{
			{TaskID: mine, ProjectID: projectA}, {TaskID: run, ProjectID: projectB}, {TaskID: kept, ProjectID: projectA}, {TaskID: missing, ProjectID: projectA},
			{TaskID: underMine, ProjectID: projectA, Via: mine}, {TaskID: underKept, ProjectID: projectA, Via: kept}, {TaskID: underGranted, ProjectID: projectA, Via: granted},
			// Its rows let me in (granted, or made by me): restricted, and mine
			// to see, with no graph read (WA2b's lock).
			{TaskID: granted, ProjectID: projectA, LetIn: true},
		}, nil
	}
	var sawSystem bool
	var read []string
	readStanding = func(ctx context.Context, ids []string, user string) ([]*dgraphStruct.DgraphTask, error) {
		sawSystem, read = taskread.System(ctx), ids
		member := func(id uuid.UUID) *dgraphStruct.DgraphTask {
			return &dgraphStruct.DgraphTask{Uuid: id.String(), Project: &dgraphStruct.DgraphProject{Uuid: projectA.String(), IsProjectMember: 1}}
		}
		return []*dgraphStruct.DgraphTask{
			{Uuid: mine.String(), Assignee: &dgraphStruct.DgraphUser{Uuid: me.String()}, Project: &dgraphStruct.DgraphProject{Uuid: projectA.String(), IsProjectMember: 1}},
			{Uuid: run.String(), Project: &dgraphStruct.DgraphProject{Uuid: projectB.String(), IsProjectMember: 1, IsProjectAdmin: 1}},
			member(kept), member(underMine), member(underKept), member(underGranted), member(granted),
		}, nil
	}
	rolesIn = func(context.Context, uuid.UUID, []uuid.UUID) (map[uuid.UUID]string, error) { return nil, nil }
	editWho = func(context.Context, string) string { return EditMembers }
	viaToken = func(context.Context) bool { return false }

	hid, err := hiddenFor(context.Background(), me.String())
	if err != nil {
		t.Fatal(err)
	}
	if askedFor != me || !sawSystem {
		t.Fatalf("the rows are asked for the reader (%s), and the graph read as a read with no audience (%v)", askedFor, sawSystem)
	}
	want := map[string]bool{kept.String(): true, missing.String(): true, underKept.String(): true}
	if trues(hid) != len(want) || !hid[kept.String()] || !hid[missing.String()] || !hid[underKept.String()] {
		t.Fatalf("hidden: %v, want %v (a task the graph can't find stays hidden; a sub-task is let in with the task above it)", hid, want)
	}
	// Every restricted task the reader may see is named false: it shows,
	// with the lock (WA2b).
	for _, id := range []uuid.UUID{mine, run, underMine, underGranted, granted} {
		if v, ok := hid[id.String()]; !ok || v {
			t.Fatalf("%s: a restricted task the reader may see is named, as not hidden: %v", id, hid)
		}
	}
	// The task one is restricted with is read once, even when it is no
	// candidate.
	if n := len(read); n != 8 {
		t.Fatalf("read %d tasks for the decision, want the 7 candidates and the one above them that isn't: %v", n, read)
	}

	// With assignments not counted, the task given to me and the one
	// restricted with it are hidden too; the project I run, and what the
	// rows let me into, still show.
	unassigned, err := hidden(context.Background(), me.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]bool{mine: true, underMine: true, kept: true, underKept: true, missing: true, run: false, underGranted: false, granted: false} {
		if v, ok := unassigned[id.String()]; !ok || v != want {
			t.Fatalf("%s with no assignment counting: hidden %v (named: %v), want %v; all: %v", id, v, ok, want, unassigned)
		}
	}

	// Through a token, running the project is a member's place: hidden too.
	viaToken = func(context.Context) bool { return true }
	if hidden, _ := hiddenFor(context.Background(), me.String()); !hidden[run.String()] {
		t.Fatal("a token's call sees no more than a member's")
	}

	// Nobody: every restricted task, with no graph read.
	readStanding = func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error) {
		t.Fatal("nobody's filter reads the graph")
		return nil, nil
	}
	if hidden, _ := hiddenFor(context.Background(), ""); len(hidden) != 8 || trues(hidden) != 8 || askedFor != uuid.Nil {
		t.Fatalf("nobody's filter: %v, asked for %s", hidden, askedFor)
	}

	// With nothing restricted, one read and nothing hidden.
	restrictionCandidates = func(context.Context, uuid.UUID) ([]restrictionModel.Candidate, error) { return nil, nil }
	if hidden, err := hiddenFor(context.Background(), me.String()); err != nil || len(hidden) != 0 {
		t.Fatalf("nothing restricted: %v %v", hidden, err)
	}
	// Only tasks its rows let the reader into: one read, none hidden, each
	// named for its lock.
	restrictionCandidates = func(context.Context, uuid.UUID) ([]restrictionModel.Candidate, error) {
		return []restrictionModel.Candidate{{TaskID: granted, ProjectID: projectA, LetIn: true}}, nil
	}
	if hidden, err := hiddenFor(context.Background(), me.String()); err != nil || len(hidden) != 1 || hidden[granted.String()] {
		t.Fatalf("only let in by its rows: %v %v", hidden, err)
	}
	// A failed read is the filter's error, never a guess.
	boom := errors.New("down")
	restrictionCandidates = func(context.Context, uuid.UUID) ([]restrictionModel.Candidate, error) { return nil, boom }
	if _, err := hiddenFor(context.Background(), me.String()); !errors.Is(err, boom) {
		t.Fatalf("a failed read: %v", err)
	}
}

func TestADecisionOnATaskReadsItsRestriction(t *testing.T) {
	me := uuid.MustParse("5a5c00a8-0000-4000-8000-0000000000aa")
	task := &dgraphStruct.DgraphTask{Uuid: "7a5c00a8-0000-4000-8000-000000000003", Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString(), IsProjectMember: 1}}
	old := restrictionsOf
	t.Cleanup(func() { restrictionsOf = old })
	grants := []uuid.UUID{}
	restrictionsOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]restrictionModel.Restriction, error) {
		return map[uuid.UUID]restrictionModel.Restriction{ids[0]: {Restricted: true, Grants: grants}}, nil
	}
	f, err := FactsFor(context.Background(), me.String(), "0x9", task)
	if err != nil || !f.Restricted || f.Granted {
		t.Fatalf("restricted, not granted: %+v %v", f, err)
	}
	grants = []uuid.UUID{me}
	if f, _ := FactsFor(context.Background(), me.String(), "0x9", task); !f.Granted {
		t.Fatal("granted by name")
	}

	// Restricted with a task above it: let in when the one filter lets them
	// in (they may see that task), and only then.
	above := uuid.New()
	restrictionsOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]restrictionModel.Restriction, error) {
		return map[uuid.UUID]restrictionModel.Restriction{ids[0]: {Restricted: true, Via: above}}, nil
	}
	oldDecide := taskread.Decide
	t.Cleanup(func() { taskread.Decide = oldDecide })
	var hide map[string]bool
	taskread.Decide = func(_ context.Context, reader string) (map[string]bool, error) {
		if reader != me.String() {
			t.Errorf("decided for %q, want the person asked about", reader)
		}
		return hide, nil
	}
	if f, err := FactsFor(context.Background(), me.String(), "0x9", task); err != nil || !f.Restricted || !f.Granted {
		t.Fatalf("let in through the task above: %+v %v", f, err)
	}
	hide = map[string]bool{task.Uuid: true}
	if f, err := FactsFor(context.Background(), me.String(), "0x9", task); err != nil || f.Granted {
		t.Fatalf("kept out with the task above: %+v %v", f, err)
	}
}

func TestATaskMadeFromARestrictedOneIsKeptToTheSamePeople(t *testing.T) {
	from, to, project, by, grantee := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	oldOf, oldR, oldI := restrictionsOf, restrict, inherit
	t.Cleanup(func() { restrictionsOf, restrict, inherit = oldOf, oldR, oldI })
	restricted := map[uuid.UUID]bool{from: true}
	restrictionsOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]restrictionModel.Restriction, error) {
		out := map[uuid.UUID]restrictionModel.Restriction{}
		for _, id := range ids {
			if restricted[id] {
				out[id] = restrictionModel.Restriction{Restricted: true, ProjectID: project, Grants: []uuid.UUID{grantee}}
			}
		}
		return out, nil
	}
	var got struct {
		task, project, from uuid.UUID
		people, under       []uuid.UUID
	}
	restrict = func(_ context.Context, task, p, _ uuid.UUID, people, under []uuid.UUID, _ time.Time) error {
		got.task, got.project, got.people, got.under = task, p, people, under
		return nil
	}
	inherit = func(_ context.Context, task, from, _ uuid.UUID, _ time.Time) error {
		got.task, got.from = task, from
		return nil
	}
	// A sub-task of it is restricted with it, whatever it is now: the rows
	// say (Inherit).
	if err := SettleRestriction(context.Background(), to, from, by, time.Now()); err != nil || got.task != to || got.from != from {
		t.Fatalf("a sub-task: %+v %v", got, err)
	}
	// A repeat's next copy, named through the context, has its people, on
	// its own.
	got.task = uuid.Nil
	if err := SettleRestriction(RestrictedLike(context.Background(), from), to, uuid.Nil, by, time.Now()); err != nil || got.task != to || got.project != project || len(got.people) != 1 || got.people[0] != grantee || got.under != nil {
		t.Fatalf("a repeat: %+v %v", got, err)
	}
	// A repeat of a task that isn't restricted, or none: nothing.
	got.task = uuid.Nil
	if err := SettleRestriction(RestrictedLike(context.Background(), uuid.New()), to, uuid.Nil, by, time.Now()); err != nil || got.task != uuid.Nil {
		t.Fatal("a repeat of a task that isn't restricted")
	}
	if err := SettleRestriction(context.Background(), to, uuid.Nil, by, time.Now()); err != nil || got.task != uuid.Nil {
		t.Fatal("from none")
	}
}

func TestRestrictingATaskRestrictsTheTasksBelowIt(t *testing.T) {
	task := &dgraphStruct.DgraphTask{Uuid: uuid.NewString(), Project: &dgraphStruct.DgraphProject{Uuid: uuid.NewString()}}
	sub, subsub := uuid.New(), uuid.New()
	oldB, oldR := tasksBelow, restrict
	t.Cleanup(func() { tasksBelow, restrict = oldB, oldR })
	tasksBelow = func(_ context.Context, id string) ([]string, error) {
		if id != task.Uuid {
			t.Errorf("read below %s", id)
		}
		return []string{sub.String(), "not-a-task", subsub.String()}, nil
	}
	var under []uuid.UUID
	restrict = func(_ context.Context, _, _, _ uuid.UUID, _, u []uuid.UUID, _ time.Time) error {
		under = u
		return nil
	}
	if err := Restrict(context.Background(), task, uuid.New(), nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(under) != 2 || under[0] != sub || under[1] != subsub {
		t.Fatalf("restricted with it: %v", under)
	}
	// Not knowing what is below it restricts nothing.
	boom := errors.New("down")
	tasksBelow = func(context.Context, string) ([]string, error) { return nil, boom }
	under = nil
	if err := Restrict(context.Background(), task, uuid.New(), nil, time.Now()); !errors.Is(err, boom) || under != nil {
		t.Fatalf("a failed read below: %v, restricted %v", err, under)
	}
}

// trues is how many of m are true: the tasks a decision hides.
func trues(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/google/uuid"
)

// The rights matrix for a dependency (DG10): same project as before (change
// the waiting task); across projects, change tasks on both ends; an end the
// person can't see is not there for them.
func TestTheDependencyRightsMatrix(t *testing.T) {
	admin := Standing{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditMembers}
	member := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditMembers}
	viewer := Standing{ProjectUUID: "p", Role: RoleViewer, EditWho: EditMembers}
	outsider := Standing{ProjectUUID: "p"}
	assignedOnly := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned}
	adminsOnly := Standing{ProjectUUID: "p", Role: RoleMember, EditWho: EditAdmins}
	v := func(s Standing, f TaskFacts) Verdict { return VerdictOf(s, f) }
	none := TaskFacts{}
	for _, c := range []struct {
		name            string
		waiting, on     Verdict
		same            bool
		want            error // nil, ErrNotAllowed (a Refusal), ErrHiddenEnd, ErrAcrossProjects
		wantAcrossWords bool
	}{
		{"same project, member", v(member, none), v(member, none), true, nil, false},
		{"same project, viewer", v(viewer, none), v(viewer, none), true, ErrNotAllowed, false},
		{"same project, member on a viewer-only blocker is the same project", v(member, none), v(member, none), true, nil, false},
		{"across, admin and admin", v(admin, none), v(admin, none), false, nil, false},
		{"across, member and member", v(member, none), v(member, none), false, nil, false},
		{"across, member and viewer", v(member, none), v(viewer, none), false, ErrAcrossProjects, true},
		{"across, viewer and admin", v(viewer, none), v(admin, none), false, ErrNotAllowed, false},
		{"across, member and outsider", v(member, none), v(outsider, none), false, ErrHiddenEnd, false},
		{"across, member and the blocker's assignee outside it", v(member, none), v(outsider, TaskFacts{Assignee: true}), false, ErrAcrossProjects, true},
		{"across, member and assigned-only on someone else's task", v(member, none), v(assignedOnly, none), false, ErrAcrossProjects, true},
		{"across, member and assigned-only on their own task", v(member, none), v(assignedOnly, TaskFacts{Assignee: true}), false, nil, false},
		{"across, member and admins-only", v(member, none), v(adminsOnly, none), false, ErrAcrossProjects, true},
		{"across, a restricted blocker they're not let into", v(member, none), v(member, TaskFacts{Restricted: true}), false, ErrHiddenEnd, false},
		{"across, a restricted blocker they're granted", v(member, none), v(member, TaskFacts{Restricted: true, Granted: true}), false, nil, false},
		{"same project, a restricted blocker they can't see", v(member, none), v(member, TaskFacts{Restricted: true}), true, ErrHiddenEnd, false},
	} {
		err := MayLink(c.waiting, c.on, c.same)
		switch {
		case c.want == nil && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != nil && !errors.Is(err, c.want):
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
		if c.wantAcrossWords && (!errors.Is(err, ErrNotAllowed) || err.Error() != "You can link tasks in two projects only when you can change tasks in both.") {
			t.Errorf("%s: the refusal is a Work Access refusal in words: %v", c.name, err)
		}
		if errors.Is(err, ErrHiddenEnd) && errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: a hidden end is not found, never a refusal that says it's there", c.name)
		}
	}
}

// The verdicts follow the one rule: seeing is NeedView, moving is
// NeedEditTask, on the task.
func TestVerdictsAreTheTaskRule(t *testing.T) {
	for _, s := range []Standing{
		{ProjectUUID: "p", Role: RoleAdmin, EditWho: EditAdmins},
		{ProjectUUID: "p", Role: RoleMember, EditWho: EditAssigned},
		{ProjectUUID: "p", Role: RoleCommenter},
		{ProjectUUID: "p", Steward: true},
		{},
	} {
		for _, f := range []TaskFacts{{}, {Assignee: true}, {Restricted: true}, {Restricted: true, Creator: true}} {
			got := VerdictOf(s, f)
			if got.See != s.CanOnTask(NeedView, f) || got.Move != s.CanOnTask(NeedEditTask, f) {
				t.Errorf("%+v %+v: %+v", s, f, got)
			}
		}
	}
}

// JudgeTasks decides many tasks in many projects with one read of the role
// rows and one of the restrictions, and a token acts as a member: an admin's
// token can't move tasks in an admins-only project, nor see a restricted
// task only its admins may.
func TestJudgeTasksReadsOnceAndTokensActAsMembers(t *testing.T) {
	me := uuid.New()
	p1, p2 := uuid.New(), uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	oldRoles, oldWho, oldRes, oldTok, oldPrime := rolesIn, editWho, restrictionsOf, viaToken, primeEditWho
	t.Cleanup(func() {
		rolesIn, editWho, restrictionsOf, viaToken, primeEditWho = oldRoles, oldWho, oldRes, oldTok, oldPrime
	})
	roleReads, resReads := 0, 0
	rolesIn = func(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		roleReads++
		return map[uuid.UUID]string{}, nil
	}
	editWho = func(_ context.Context, project string) string {
		if project == p2.String() {
			return EditAdmins
		}
		return EditMembers
	}
	primeEditWho = func(context.Context, []string) {}
	restrictionsOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]restrictionModel.Restriction, error) {
		resReads++
		return map[uuid.UUID]restrictionModel.Restriction{c: {Restricted: true}}, nil
	}
	proj1 := &dgraphStruct.DgraphProject{Uuid: p1.String(), IsProjectMember: 1}
	proj2 := &dgraphStruct.DgraphProject{Uuid: strings.ToUpper(p2.String()), IsProjectAdmin: 1, IsProjectMember: 1}
	tasks := []*dgraphStruct.DgraphTask{
		{Uuid: a.String(), Project: proj1},
		{Uuid: b.String(), Project: proj2},
		{Uuid: c.String(), Project: &dgraphStruct.DgraphProject{Uuid: p2.String(), IsProjectAdmin: 1, IsProjectMember: 1}},
	}
	viaToken = func(context.Context) bool { return false }
	got, err := JudgeTasks(context.Background(), me.String(), "0x9", tasks...)
	if err != nil {
		t.Fatal(err)
	}
	if roleReads != 1 || resReads != 1 {
		t.Fatalf("one read of roles (%d) and one of restrictions (%d) for three tasks in two projects", roleReads, resReads)
	}
	if !got[a.String()].Move || !got[b.String()].Move || !got[c.String()].See || !got[c.String()].Move {
		t.Fatalf("in the app, an admin of p2 moves its tasks and sees its restricted one: %+v", got)
	}
	viaToken = func(context.Context) bool { return true }
	got, err = JudgeTasks(context.Background(), me.String(), "0x9", tasks...)
	if err != nil {
		t.Fatal(err)
	}
	if !got[a.String()].Move {
		t.Fatalf("a token moves what a member may: %+v", got[a.String()])
	}
	if got[b.String()].Move || !got[b.String()].See {
		t.Fatalf("a token is a member at most: it sees p2's task but can't move it in an admins-only project: %+v", got[b.String()])
	}
	if got[c.String()].See {
		t.Fatalf("a token doesn't see a restricted task only admins may: %+v", got[c.String()])
	}
	// A task in no project is its maker's; nobody else sees it.
	got, _ = JudgeTasks(context.Background(), me.String(), "0x9", &dgraphStruct.DgraphTask{Uuid: uuid.NewString()})
	for _, v := range got {
		if v.See || v.Move {
			t.Fatalf("a task in no project, not theirs: %+v", v)
		}
	}
}

// A task's panel keeps the dependency ends in other projects the reader may
// see and drops the rest whole: no stub, nothing to count. An end in its own
// project is the panel's as before, a restricted one a stub (WA2b).
func TestKeepSeenEndsDropsHiddenEndsWhole(t *testing.T) {
	p, q, r := uuid.New(), uuid.New(), uuid.New()
	inQ, inR, stubQ, stubP, mine := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	oldRoles, oldWho, oldRes, oldTok, oldPrime := rolesIn, editWho, restrictionsOf, viaToken, primeEditWho
	t.Cleanup(func() {
		rolesIn, editWho, restrictionsOf, viaToken, primeEditWho = oldRoles, oldWho, oldRes, oldTok, oldPrime
	})
	rolesIn = func(context.Context, uuid.UUID, []uuid.UUID) (map[uuid.UUID]string, error) { return nil, nil }
	editWho = func(context.Context, string) string { return EditMembers }
	primeEditWho = func(context.Context, []string) {}
	viaToken = func(context.Context) bool { return false }
	restrictionsOf = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]restrictionModel.Restriction, error) {
		return map[uuid.UUID]restrictionModel.Restriction{
			stubQ: {Restricted: true, ProjectID: q},
			stubP: {Restricted: true, ProjectID: p},
		}, nil
	}
	task := &dgraphStruct.DgraphTask{
		Uuid:    uuid.NewString(),
		Project: &dgraphStruct.DgraphProject{Uuid: p.String(), IsProjectMember: 1},
		BlockedBy: []*dgraphStruct.DgraphTask{
			{Uuid: mine.String(), Project: &dgraphStruct.DgraphProject{Uuid: p.String(), IsProjectMember: 1}},
			{Uuid: inQ.String(), Project: &dgraphStruct.DgraphProject{Uuid: q.String(), IsProjectMember: 1}},
			{Uuid: stubP.String(), Hidden: true},
		},
		Blocks: []*dgraphStruct.DgraphTask{
			{Uuid: inR.String(), Project: &dgraphStruct.DgraphProject{Uuid: r.String()}},
			{Uuid: stubQ.String(), Hidden: true},
			{Uuid: uuid.NewString()}, // no project: not an end anyone may see
		},
	}
	if err := KeepSeenEnds(context.Background(), uuid.NewString(), "0x9", task); err != nil {
		t.Fatal(err)
	}
	ids := func(ts []*dgraphStruct.DgraphTask) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Uuid)
		}
		return out
	}
	if got := ids(task.BlockedBy); len(got) != 3 || got[0] != mine.String() || got[1] != inQ.String() || got[2] != stubP.String() {
		t.Errorf("its own project's end, Q's they're in, and its own project's restricted stub stay: %v", got)
	}
	if got := ids(task.Blocks); len(got) != 0 {
		t.Errorf("R's end, Q's restricted stub and a task in no project go, whole: %v", got)
	}
}

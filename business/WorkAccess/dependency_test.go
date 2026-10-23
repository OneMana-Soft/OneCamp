package business

import (
	"context"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/google/uuid"
)

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

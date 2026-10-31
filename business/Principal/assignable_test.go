package business

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who may be given work or a place turns on their own deletion time alone. The
// three ways "present" is written (nil, Go zero on reactivation, the Unix epoch)
// all pass; bots, agents and external identities pass unless their own time says
// they are gone.
func TestCanBeAssignedTurnsOnTheirOwnDeletionTime(t *testing.T) {
	gone := time.Now().Add(-time.Hour).UTC()
	zero := time.Time{}
	epoch := time.Unix(0, 0).UTC()
	for _, c := range []struct {
		name string
		u    *dgraphStruct.DgraphUser
		ok   bool
	}{
		{"a member read without a deletion time", &dgraphStruct.DgraphUser{Uid: "0x1"}, true},
		{"a member reactivated (Go zero)", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &zero}, true},
		{"a member whose time is Dgraph's zero (the epoch)", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &epoch}, true},
		{"an agent", &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true, IsExternal: true, DeletedAt: &zero}, true},
		{"an identity GitHub brought along", &dgraphStruct.DgraphUser{Uid: "0x1", IsExternal: true}, true},
		{"a deactivated member", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &gone}, false},
		{"a deleted agent", &dgraphStruct.DgraphUser{Uid: "0x1", IsBot: true, IsExternal: true, DeletedAt: &gone}, false},
		{"nobody", nil, false},
		{"a person read without their node", &dgraphStruct.DgraphUser{Uuid: "u"}, false},
	} {
		if got := CanBeAssigned(c.u); got.Allowed != c.ok || got.Reason == "" {
			t.Errorf("%s: %+v, want allowed=%v with a reason", c.name, got, c.ok)
		}
	}
}

// The refusal names whom they picked and why, as a conflict, so every write
// answers it 409 in these words.
func TestUnassignableSaysWhoAndWhy(t *testing.T) {
	gone := time.Now().UTC()
	sam := &dgraphStruct.DgraphUser{Uid: "0x1", UserName: "Sam Rivera", DeletedAt: &gone}
	for _, c := range []struct {
		u    *dgraphStruct.DgraphUser
		as   string
		says string
	}{
		{sam, Assigned, "Sam Rivera can't be assigned: their account is deactivated."},
		{sam, Added, "Sam Rivera can't be added: their account is deactivated."},
		{nil, Added, "That person can't be added: they could not be found."},
	} {
		err := Unassignable(c.u, c.as)
		if !errors.Is(err, helpers.ErrConflict) || err.Error() != c.says {
			t.Errorf("%v: %v, want the conflict %q", c.u, err, c.says)
		}
	}
	if err := Unassignable(&dgraphStruct.DgraphUser{Uid: "0x2", UserName: "Maya"}, Assigned); err != nil {
		t.Errorf("a live member was refused: %v", err)
	}
}

// Every path that gives someone work or a place asks the one rule. Before it,
// a task or a sub-task could be given (in the app, through a token, by an
// agent or by GitHub), and a person added to a channel, a team or a project,
// after their account was deactivated; only group conversations refused, by a
// copy of their own. A path that stops asking fails here, not in a
// deactivated person's inbox.
func TestEveryAssignAndAddPathAsksTheOneRule(t *testing.T) {
	for _, path := range []struct{ file, fn, why string }{
		{"../../business/Task/taskBusiness.go", "CreateTask", "a new task (the app, a token, an agent, a form) could be given to them"},
		{"../../business/Task/taskBusiness.go", "UpdateTaskAssigneeByTaskUUID", "a task could be handed to them from any path"},
		{"../../business/Task/taskBusiness.go", "CreateSubTask", "a sub-task could be given to them"},
		{"../../business/Task/taskRecurrence.go", "repeatIfRecurring", "a repeating task given to them would stop repeating"},
		{"../../business/TaskWriter/contract.go", "Apply", "a rule's assign would fail halfway through a batch, or say they aren't in the project"},
		{"../../business/Form/form.go", "Submit", "a form whose assignee was deactivated would refuse every visitor"},
		{"../../business/Send/send.go", "MayBeInGroup", "group conversations would keep their own copy of the rule"},
		{"../../controllers/Channel/channelController.go", "AddChannelMember", "they could be added to a channel"},
		{"../../controllers/Team/teamController.go", "AddMemberToTeam", "they could be added to a team"},
		{"../../controllers/Project/memberRules.go", "memberChangeRefusal", "they could be added to a project"},
	} {
		if !asksTheRule(t, path.file, path.fn) {
			t.Errorf("%s %s doesn't ask CanBeAssigned (or Unassignable): %s", path.file, path.fn, path.why)
		}
	}
}

// Every path that gives someone a task asks whether they may see it: an
// assignee reads their task, so one given a task they can't otherwise read was
// let in by whoever gave it, past its project or a restriction an admin set.
// The paths through these functions ask (business/Task Unseeing,
// UnseeingNew); the data moves below bring their people with them and are
// named, each marked DataMove where it assigns, so a new path still has to ask.
func TestEveryAssignPathAsksWhoSeesTheTask(t *testing.T) {
	for _, path := range []struct{ file, fn, why string }{
		{"../../business/Task/taskBusiness.go", "CreateTask", "a new task could be given to someone outside its project (the app, a token, an agent, a rule)"},
		{"../../business/Task/taskBusiness.go", "CreateSubTask", "a sub-task could be given to someone who can't see its parent"},
		{"../../business/Task/taskBusiness.go", "UpdateTaskAssigneeByTaskUUID", "a task could be handed past its project or its restriction from any path"},
		{"../../business/Task/taskRecurrence.go", "repeatIfRecurring", "a repeat would fail, or go to someone taken off the project"},
		{"../../business/TaskWriter/contract.go", "Apply", "a restriction would refuse a rule's assign halfway through a batch"},
	} {
		if !callsAny(t, path.file, path.fn, "Unseeing", "UnseeingNew", "unseeing") {
			t.Errorf("%s %s doesn't ask Unseeing: %s", path.file, path.fn, path.why)
		}
	}
	for _, move := range []struct{ file, fn, why string }{
		{"../../business/GitHub/githubBusiness.go", "", "GitHub's sync gives a task to the person its issue names"},
		{"../../business/GitHub/githubSync.go", "", "GitHub's sync gives a task to the person its issue names"},
		{"../../business/Import/recheck.go", "", "an import's re-check puts back an assignee its source lets in"},
		{"../../business/ProjectTemplate/apply.go", "", "a template's apply makes its tasks as the template has them"},
	} {
		if !callsAny(t, move.file, move.fn, "DataMove") {
			t.Errorf("%s is a data move that keeps its assignees (%s) but no longer says so (DataMove)", move.file, move.why)
		}
	}
}

// callsAny is whether fn in file (any function of it, for "") names one of
// names, itself or through a function of the same file it calls.
func callsAny(t *testing.T, file, fn string, names ...string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	decls := map[string][]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
		}
	}
	seen := map[string]bool{}
	var mentions func(name string) bool
	mentions = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		found := false
		for _, fd := range decls[name] {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if found {
					return false
				}
				switch x := n.(type) {
				case *ast.SelectorExpr:
					found = want[x.Sel.Name]
				case *ast.Ident:
					found = want[x.Name]
				case *ast.CallExpr:
					id, ok := x.Fun.(*ast.Ident)
					found = ok && mentions(id.Name)
				}
				return !found
			})
		}
		return found
	}
	if fn != "" {
		return mentions(fn)
	}
	for name := range decls {
		if mentions(name) {
			return true
		}
	}
	return false
}

// asksTheRule is whether fn in file calls CanBeAssigned or Unassignable,
// itself or through a function of the same file it calls: CreateTask hands
// every task to CreateTaskAsked, which asks before anything is written
// (27 Oct, the first-status rules), and a scan of CreateTask's own body read
// that as a path that had stopped asking.
func asksTheRule(t *testing.T, file, fn string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	// By name, methods too (TaskWriter's Apply is one).
	decls := map[string][]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			decls[fd.Name.Name] = append(decls[fd.Name.Name], fd)
		}
	}
	seen := map[string]bool{}
	var asks func(name string) bool
	asks = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		found := false
		for _, fd := range decls[name] {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if found {
					return false
				}
				switch x := n.(type) {
				case *ast.SelectorExpr:
					found = x.Sel.Name == "CanBeAssigned" || x.Sel.Name == "Unassignable"
				case *ast.CallExpr:
					// A call to a function of this file, which may ask for it.
					id, ok := x.Fun.(*ast.Ident)
					found = ok && asks(id.Name)
				}
				return !found
			})
		}
		return found
	}
	return asks(fn)
}

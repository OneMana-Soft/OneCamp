package business

import (
	"context"
	"errors"
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who a task may be given to: someone who may read it already, in words when
// not; a task in no project to any member who can sign in; a bot and a data
// move as before.
func TestATaskGoesOnlyToSomeoneWhoMaySeeIt(t *testing.T) {
	oldSee, oldMembers := seeingOf, membersAmong
	t.Cleanup(func() { seeingOf, membersAmong = oldSee, oldMembers })
	stands := map[string]domain.Seeing{
		"maya":  {Sees: true, InProject: true},
		"olu":   {InProject: true},
		"kept":  {InProject: true, Restricted: true},
		"mine":  {},
		"ghost": {},
	}
	seeingOf = func(_ context.Context, task, user string) (domain.Seeing, error) {
		if task == "broken" {
			return domain.Seeing{}, errors.New("graph down")
		}
		if task == "personal" {
			return domain.Seeing{}, nil
		}
		return stands[user], nil
	}
	membersAmong = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		return map[string]bool{"maya": ids[0] == "maya"}, nil
	}
	person := func(id, name string) *dgraphStruct.DgraphUser {
		return &dgraphStruct.DgraphUser{Uid: "0x" + id, Uuid: id, UserName: name}
	}
	maya, olu, kept := person("maya", "Maya Chen"), person("olu", "Olu Ade"), person("kept", "Kim Park")
	ghost := person("ghost", "octocat")
	ghost.IsExternal = true
	bot := person("bot", "Builder")
	bot.IsBot, bot.IsExternal = true, true
	ctx := context.Background()
	task := &dgraphStruct.DgraphTask{Uuid: "t", Assignee: olu}

	for _, c := range []struct {
		what string
		err  error
		says string
	}{
		{"one of the project's people", Unseeing(ctx, "t", task, maya), ""},
		{"the one it's given to already", Unseeing(ctx, "t", task, olu), ""},
		{"nobody (unassigning)", Unseeing(ctx, "t", task, nil), ""},
		{"someone outside its project", Unseeing(ctx, "t", &dgraphStruct.DgraphTask{Uuid: "t"}, olu), "Olu Ade can't see this task. Add them to the project first."},
		{"one of its people its restriction keeps out", Unseeing(ctx, "t", task, kept), "Kim Park can't see this task: only some people can see it. A project admin can let them in."},
		{"a bot", Unseeing(ctx, "t", task, bot), ""},
		{"anyone, as a data move", Unseeing(DataMove(ctx), "t", task, kept), ""},
		{"a member, for a task in no project", Unseeing(ctx, "personal", nil, olu), ""},
		{"someone who can't sign in, for a task in no project", Unseeing(ctx, "personal", nil, ghost), "octocat can't be given a task outside a project: they aren't a member of this workspace."},
		{"a new task, to one of the project's people", UnseeingNew(ctx, "p", "", maya), ""},
		{"a new task, to someone outside the project", UnseeingNew(ctx, "p", "", olu), "Olu Ade can't see this task. Add them to the project first."},
		{"a sub-task, to someone its restricted parent keeps out", UnseeingNew(ctx, "p", "parent", kept), "Kim Park can't see this task: only some people can see it. A project admin can let them in."},
		{"a sub-task, to whoever sees its parent", UnseeingNew(ctx, "p", "parent", maya), ""},
		{"a new task in no project, to a member", UnseeingNew(ctx, "", "", olu), ""},
	} {
		switch {
		case c.says == "" && c.err != nil:
			t.Errorf("%s: refused: %v", c.what, c.err)
		case c.says != "" && (!errors.Is(c.err, helpers.ErrConflict) || c.err.Error() != c.says):
			t.Errorf("%s: %v, want the conflict %q", c.what, c.err, c.says)
		}
	}
	// A read that fails is that failure, never a refusal or a pass.
	if err := Unseeing(ctx, "broken", task, maya); err == nil || errors.Is(err, helpers.ErrConflict) {
		t.Errorf("a failed read answered %v", err)
	}
}

package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	taskFieldModel "github.com/akashc777/OneCamp/models/postgres/TaskField"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// whoSeams swaps the rule's reads for the test's own people.
func whoSeams(t *testing.T, inProject func(id string, project uuid.UUID) *dgraphStruct.DgraphUser, byID map[string]*dgraphStruct.DgraphUser) {
	t.Helper()
	a, u := lookAssignee, lookUser
	t.Cleanup(func() { lookAssignee, lookUser = a, u })
	lookAssignee = func(_ context.Context, id string, project uuid.UUID) (*dgraphStruct.DgraphUser, error) {
		return inProject(id, project), nil
	}
	lookUser = func(_ context.Context, id string) (*dgraphStruct.DgraphUser, error) { return byID[id], nil }
}

func TestShowNamesAnAgentPresetByTheSharedRule(t *testing.T) {
	project := uuid.New()
	key := "agents/release-captain.png"
	agent := &dgraphStruct.DgraphUser{Uuid: "agent-1", UserName: "Release Captain", ProfileKey: &key, IsBot: true, BotKind: "agent", EmailID: "agent@bots"}
	var asked uuid.UUID
	whoSeams(t, func(id string, p uuid.UUID) *dgraphStruct.DgraphUser {
		asked = p
		if id == agent.Uuid {
			return agent
		}
		return nil
	}, nil)

	got := Show(context.Background(), Template{ID: "t1", ProjectID: project.String(), Shape: Shape{Assignee: "agent-1"}}, project)
	if asked != project {
		t.Fatalf("the rule was asked about project %v, want %v", asked, project)
	}
	if got.AssigneeWho == nil {
		t.Fatal("an AI teammate the rule lets the project's tasks go to shows no who")
	}
	w := got.AssigneeWho
	if w.Uuid != "agent-1" || w.UserName != "Release Captain" || !w.IsBot || w.BotKind != "agent" || w.ProfileKey == nil || *w.ProfileKey != key {
		t.Fatalf("who = %+v, want the agent's uuid, name, avatar and kind", *w)
	}
	if w.EmailID != "" || w.Uid != "" {
		t.Fatalf("who carries more than a chip shows: %+v", *w)
	}
	if got.ID != "t1" || got.Shape.Assignee != "agent-1" {
		t.Fatalf("the template changed: %+v", got.Template)
	}
}

func TestShowLeavesOutWhomTheRuleRefuses(t *testing.T) {
	project := uuid.New()
	whoSeams(t, func(string, uuid.UUID) *dgraphStruct.DgraphUser { return nil }, nil)
	if got := Show(context.Background(), Template{Shape: Shape{Assignee: "left-the-project"}}, project); got.AssigneeWho != nil {
		t.Fatalf("someone the rule refuses shows as %+v", *got.AssigneeWho)
	}
	if got := Show(context.Background(), Template{}, project); got.AssigneeWho != nil {
		t.Fatalf("a template with no assignee shows %+v", *got.AssigneeWho)
	}
}

func TestShowForTheWorkspaceAsksWhetherTheAccountMayBeGivenWork(t *testing.T) {
	gone := time.Now().Add(-time.Hour)
	byID := map[string]*dgraphStruct.DgraphUser{
		"ana":  {Uid: "0xa", Uuid: "ana", UserName: "Ana", UserFullName: "Ana Ruiz"},
		"left": {Uid: "0xb", Uuid: "left", UserName: "Left", DeletedAt: &gone},
	}
	whoSeams(t, func(string, uuid.UUID) *dgraphStruct.DgraphUser {
		t.Fatal("a workspace template's who asked a project's rule")
		return nil
	}, byID)
	got := Show(context.Background(), Template{Shape: Shape{Assignee: "ana"}}, uuid.Nil)
	if got.AssigneeWho == nil || got.AssigneeWho.UserFullName != "Ana Ruiz" {
		t.Fatalf("an active account shows as %+v, want Ana Ruiz", got.AssigneeWho)
	}
	if got := Show(context.Background(), Template{Shape: Shape{Assignee: "left"}}, uuid.Nil); got.AssigneeWho != nil {
		t.Fatalf("a deactivated account shows as %+v", *got.AssigneeWho)
	}
	if got := Show(context.Background(), Template{Shape: Shape{Assignee: "nobody"}}, uuid.Nil); got.AssigneeWho != nil {
		t.Fatalf("someone OneCamp doesn't know shows as %+v", *got.AssigneeWho)
	}
}

// A project's template shows a money field's preset only to a reader the
// money judge (business/TaskField Hidden) lets read it: the server reads every
// project's money, as a project's admin reads theirs, while a reader the judge
// names admin of nothing (a bare context, as a member of a project they don't
// run is to it) gets the plain field's preset and not the money one. When the
// project's fields can't be read, no preset shows at all, to anyone. The
// caller's template keeps every preset: the task it makes still takes them.
// (Which person is a project's admin is the judge's: TaskField money_test.)
func TestShowKeepsMoneyPresetsFromWhoeverMayNotReadThem(t *testing.T) {
	project := uuid.New()
	budget := &taskFieldModel.Field{ID: uuid.New(), ProjectID: project, Name: "Budget", Type: taskFieldModel.TypeMoney}
	notes := &taskFieldModel.Field{ID: uuid.New(), ProjectID: project, Name: "Notes", Type: taskFieldModel.TypeText}
	money, plain := json.RawMessage(`{"amount":1200,"currency":"INR"}`), json.RawMessage(`"Ask the vendor first"`)
	presets := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{budget.ID.String(): money, notes.ID.String(): plain}
	}
	whoSeams(t, func(string, uuid.UUID) *dgraphStruct.DgraphUser { return nil }, nil)
	was := projectFields
	t.Cleanup(func() { projectFields = was })
	var asked []uuid.UUID
	var readErr error
	projectFields = func(_ context.Context, p uuid.UUID) ([]*taskFieldModel.Field, error) {
		asked = append(asked, p)
		if readErr != nil {
			return nil, readErr
		}
		return []*taskFieldModel.Field{budget, notes}, nil
	}
	server, member := taskread.WithSystem(context.Background()), context.Background()
	tpl := Template{ID: "t1", ProjectID: project.String(), Shape: Shape{Fields: presets()}}

	got := Show(server, tpl, project).Shape.Fields
	if len(got) != 2 || string(got[budget.ID.String()]) != string(money) || string(got[notes.ID.String()]) != string(plain) {
		t.Fatalf("the reader who may read money sees presets %s, want the budget and the notes", got)
	}
	got = Show(member, tpl, project).Shape.Fields
	if _, ok := got[budget.ID.String()]; ok {
		t.Fatalf("a reader who may not read money sees the budget's preset %s", got[budget.ID.String()])
	}
	if len(got) != 1 || string(got[notes.ID.String()]) != string(plain) {
		t.Fatalf("a reader who may not read money sees presets %s, want the notes alone", got)
	}
	if len(tpl.Shape.Fields) != 2 || string(tpl.Shape.Fields[budget.ID.String()]) != string(money) {
		t.Fatalf("showing the template changed its presets to %s", tpl.Shape.Fields)
	}
	for _, p := range asked {
		if p != project {
			t.Fatalf("the fields were read of project %v, want %v", p, project)
		}
	}

	readErr = errors.New("postgres down")
	for name, ctx := range map[string]context.Context{"the server": server, "a member": member} {
		if got := Show(ctx, tpl, project).Shape.Fields; len(got) != 0 {
			t.Fatalf("with the fields unread, %s sees presets %s, want none", name, got)
		}
	}
	if len(asked) != 4 {
		t.Fatalf("the project's fields were read %d times over four shows, want 4", len(asked))
	}
}

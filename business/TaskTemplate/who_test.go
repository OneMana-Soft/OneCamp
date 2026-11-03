package business

import (
	"context"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
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

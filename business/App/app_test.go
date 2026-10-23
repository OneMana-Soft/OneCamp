package business

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// An app reaches only where it was added; nothing is open by default
// (design/platform/API_CONTRACT.md §3.1).

func TestAnAppReachesOnlyWhereItWasAdded(t *testing.T) {
	added := uuid.NewString()
	elsewhere := uuid.NewString()
	app := &Identity{Name: "Deploy bot", Audience: Audience{Places: []Place{{Kind: "channel", ID: added}}, Public: true}}

	if !MayReach(context.Background(), app, "channel", added) {
		t.Error("the app can't reach the channel it was added to")
	}
	if MayReach(context.Background(), app, "channel", elsewhere) {
		t.Error("the app reached a channel it wasn't added to")
	}
	if MayReach(context.Background(), app, "project", added) {
		t.Error("a place of one kind let the app reach another kind with the same id")
	}

	if MayReach(context.Background(), nil, "channel", added) {
		t.Error("no app reached something")
	}
}

// A list or a search answers an app with only what sits in its places: a task
// in a project it was added to, or a task it was added to itself.
func TestAnAppsListHoldsOnlyItsPlaces(t *testing.T) {
	app := &Identity{Audience: Audience{Places: []Place{{Kind: "project", ID: "p1"}, {Kind: "task", ID: "t9"}}}}
	type task struct{ id, project string }
	kept := Narrow(context.Background(), app, []task{{"t1", "p1"}, {"t2", "p2"}, {"t9", "p2"}, {"t3", ""}}, func(t task) []Place {
		return []Place{{Kind: "task", ID: t.id}, {Kind: "project", ID: t.project}}
	})
	if len(kept) != 2 || kept[0].id != "t1" || kept[1].id != "t9" {
		t.Errorf("kept %v, want t1 (its project) and t9 (itself)", kept)
	}
	if got := Narrow(context.Background(), app, []task{{"t1", "p1"}}, func(task) []Place { return nil }); len(got) != 0 {
		t.Errorf("an item in no place was kept: %v", got)
	}
}

// A token that isn't an app's is no app, and nothing is read to say so.
func TestATokenWithNoAppActsAsNoApp(t *testing.T) {
	if app, err := ActsAsApp(context.Background()); app != nil || err != nil {
		t.Errorf("no app on the request: %+v, %v", app, err)
	}
}

func TestAnAppsPlacesAndScopesAreChecked(t *testing.T) {
	if _, err := cleanPlaces([]Place{{Kind: "channel", ID: "general"}}); err == nil || !strings.Contains(err.Error(), "isn't a channel's id") {
		t.Errorf("a place that isn't an id: %v", err)
	}
	if _, err := cleanPlaces([]Place{{Kind: "", ID: uuid.NewString()}}); err == nil {
		t.Error("a place with no kind was taken")
	}
	id := uuid.NewString()
	got, err := cleanPlaces([]Place{{Kind: "channel", ID: id}, {Kind: "channel", ID: " " + id + " "}})
	if err != nil || len(got) != 1 {
		t.Errorf("one place twice: %v %v", got, err)
	}

	if _, err := knownScopes([]string{"tasks:read", "nonsense:write"}); err == nil || !strings.Contains(err.Error(), "nonsense:write") {
		t.Errorf("an unknown scope: %v", err)
	}
	scopes, err := knownScopes([]string{"tasks:write", "tasks:read", "tasks:read", " "})
	if err != nil || strings.Join(scopes, ",") != "tasks:read,tasks:write" {
		t.Errorf("scopes cleaned to %v (%v)", scopes, err)
	}

	app := &Identity{Scopes: []string{"messages:write"}}
	if !app.MayCarry("messages:write") || app.MayCarry("tasks:write") {
		t.Error("MayCarry let through a scope the app doesn't have, or refused one it does")
	}
}

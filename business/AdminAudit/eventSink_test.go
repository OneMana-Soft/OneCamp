package business

import (
	"context"
	"errors"
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/google/uuid"
)

type eventRowSeen struct {
	actor                              *uuid.UUID
	email, kind, action, category, sum string
	meta                               map[string]interface{}
}

// An area's row goes into the chain with the actor's kind, the action's name
// and the area's category worked out here, the same for every area.
func TestEventSinkRunWritesTheAreasRow(t *testing.T) {
	var seen []eventRowSeen
	oldR, oldE := RecordEventRow, EventActorEmail
	t.Cleanup(func() { RecordEventRow, EventActorEmail = oldR, oldE })
	RecordEventRow = func(_ context.Context, actor *uuid.UUID, email, kind, action, category, summary string, meta map[string]interface{}) error {
		seen = append(seen, eventRowSeen{actor, email, kind, action, category, summary, meta})
		return nil
	}
	EventActorEmail = func(context.Context, uuid.UUID) string { return "akash@example.test" }
	sink := EventSink{
		Name:     "test.audit",
		Category: "boards",
		Wants:    func(resourceEventBusiness.Event) bool { return true },
		Row: func(_ context.Context, e resourceEventBusiness.Event) (string, map[string]interface{}) {
			return "Did " + string(e.Kind), map[string]interface{}{"board_uuid": e.ResourceUUID}
		},
	}
	person := uuid.New()
	ctx := context.Background()
	sink.Run(ctx, resourceEventBusiness.Event{Kind: "board.exported", ResourceKind: "board", ResourceUUID: "b1", Actor: resourceEventBusiness.Actor{UserUUID: person.String()}})
	sink.Run(ctx, resourceEventBusiness.Event{Kind: "board.exported", ResourceKind: "board", ResourceUUID: "b1", Actor: resourceEventBusiness.Actor{UserUUID: person.String(), AgentUUID: "agent-1"}})
	sink.Run(ctx, resourceEventBusiness.Event{Kind: "board.exported", ResourceKind: "board", ResourceUUID: "b1", Actor: resourceEventBusiness.Actor{System: true}})
	if len(seen) != 3 {
		t.Fatalf("rows: %d, want 3", len(seen))
	}
	want := []struct{ kind, action, email string }{
		{ActorHuman, "board.exported", "akash@example.test"},
		{ActorAgent, "agent.board.exported", "akash@example.test"},
		{ActorSystem, "board.exported", ""},
	}
	for i, w := range want {
		r := seen[i]
		if r.kind != w.kind || r.action != w.action || r.email != w.email || r.category != "boards" || r.sum != "Did board.exported" || r.meta["board_uuid"] != "b1" {
			t.Errorf("row %d: %+v, want kind %s action %s email %q under boards", i, r, w.kind, w.action, w.email)
		}
	}
	if seen[0].actor == nil || *seen[0].actor != person || seen[2].actor != nil {
		t.Errorf("actors: %v, %v; want the person, then none for OneCamp", seen[0].actor, seen[2].actor)
	}

	// A row that can't be written is logged, never panics, and the change it
	// records has already happened.
	RecordEventRow = func(context.Context, *uuid.UUID, string, string, string, string, string, map[string]interface{}) error {
		return errors.New("chain unavailable")
	}
	sink.Run(ctx, resourceEventBusiness.Event{Kind: "board.exported", ResourceKind: "board", ResourceUUID: "b1"})
}

// A sink that can't say where its rows go, which events it takes or how a row
// reads fails where it's registered, not silently at run time.
func TestRegisterEventSinkRefusesAnIncompleteSink(t *testing.T) {
	row := func(context.Context, resourceEventBusiness.Event) (string, map[string]interface{}) { return "", nil }
	wants := func(resourceEventBusiness.Event) bool { return true }
	for name, s := range map[string]EventSink{
		"no category": {Name: "a", Wants: wants, Row: row},
		"no filter":   {Name: "b", Category: "boards", Row: row},
		"no row":      {Name: "c", Category: "boards", Wants: wants},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered", name)
				}
			}()
			RegisterEventSink(s)
		}()
	}
}

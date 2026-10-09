package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// A routine runs for whoever asked for it: the sponsor, someone else, or, for
// one from before askers were recorded, nobody known, which refuses everything
// rather than run as the sponsor.
func TestARoutineRunsForWhoeverAskedForIt(t *testing.T) {
	sponsor, teammate := uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: sponsor}
	cases := []struct {
		name      string
		createdBy uuid.UUID
		asked     bool
		requester string
	}{
		{"the sponsor's", sponsor, false, ""},
		{"a teammate's", teammate, true, teammate.String()},
		{"nobody known's", uuid.Nil, true, ""},
	}
	for _, c := range cases {
		ctx := routineRunFor(context.Background(), agent, &model.AgentRoutine{CreatedBy: c.createdBy})
		requester, asked := ai.RunAskedBy(ctx)
		if asked != c.asked || requester != c.requester {
			t.Errorf("%s routine: asked %v by %q, want %v by %q", c.name, asked, requester, c.asked, c.requester)
		}
	}
}

// A routine counts as shared, and so as possibly someone else's, wherever
// anyone besides the sponsor can see what it posts, or where that can't be told.
func TestWhereARoutineIsShared(t *testing.T) {
	sponsor := uuid.New()
	agent := &model.AiAgent{CreatedBy: sponsor}
	yes, no := true, false
	channels := map[string]*dgraphStruct.DgraphChannel{
		"open":    {Uuid: "open", IsPrivate: &no},
		"unknown": {Uuid: "unknown"},
		"theirs": {Uuid: "theirs", IsPrivate: &yes, Members: []*dgraphStruct.DgraphUser{
			{Uuid: sponsor.String()}, {Uuid: uuid.NewString()}}},
		"alone": {Uuid: "alone", IsPrivate: &yes, Members: []*dgraphStruct.DgraphUser{
			{Uuid: strings.ToUpper(sponsor.String())}, {Uuid: uuid.NewString(), IsBot: true}}},
	}
	ids := map[string]uuid.UUID{}
	for name := range channels {
		ids[name] = uuid.New()
	}
	ids["broken"] = uuid.New()
	restore := lookupRoutineChannel
	t.Cleanup(func() { lookupRoutineChannel = restore })
	lookupRoutineChannel = func(_ context.Context, id uuid.UUID, _ string) (*dgraphStruct.DgraphChannel, error) {
		for name, cid := range ids {
			if cid == id {
				if name == "broken" {
					return nil, errors.New("graph down")
				}
				return channels[name], nil
			}
		}
		return nil, nil
	}
	in := func(name string) *model.AgentRoutine {
		id := ids[name]
		return &model.AgentRoutine{ChannelId: &id}
	}
	cases := map[string]bool{"open": true, "unknown": true, "theirs": true, "alone": false, "broken": true}
	for name, want := range cases {
		if got := routineShared(context.Background(), agent, in(name)); got != want {
			t.Errorf("a routine in the %q channel: shared %v, want %v", name, got, want)
		}
	}
	if !routineShared(context.Background(), agent, &model.AgentRoutine{GroupId: "g1"}) {
		t.Error("a routine in a group chat is not shared")
	}
}

func TestTheSponsorIsToldWhichRoutinesWerePaused(t *testing.T) {
	one := routinesPausedNote([]*model.AgentRoutine{{Name: "Post <b>standup</b>", Recurrence: "FREQ=DAILY", AtMinuteUTC: 540}})
	if !strings.Contains(one, "paused a routine that posts") || !strings.Contains(one, "Post &lt;b&gt;standup&lt;/b&gt;") || strings.Contains(one, "<b>") {
		t.Errorf("one routine: %s", one)
	}
	two := routinesPausedNote([]*model.AgentRoutine{{Name: "a"}, {Name: "b"}})
	if !strings.Contains(two, "paused 2 routines that post") || !strings.Contains(two, "They were") {
		t.Errorf("two routines: %s", two)
	}
}

// A run for someone else cancels only the routines that person set up.
func TestARunForSomeoneElseCancelsOnlyTheirRoutines(t *testing.T) {
	sponsor, asker, other := uuid.New(), uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: sponsor}
	theirs, sponsors, anothers, nobodys := &model.AgentRoutine{CreatedBy: asker}, &model.AgentRoutine{CreatedBy: sponsor},
		&model.AgentRoutine{CreatedBy: other}, &model.AgentRoutine{CreatedBy: uuid.Nil}
	forAsker := ai.WithRunRequester(context.Background(), asker.String(), sponsor.String())
	if !mayCancelRoutine(forAsker, agent, theirs) {
		t.Error("the asker may not cancel their own routine")
	}
	for name, r := range map[string]*model.AgentRoutine{"the sponsor's": sponsors, "another person's": anothers, "nobody known's": nobodys} {
		if mayCancelRoutine(forAsker, agent, r) {
			t.Errorf("a run for the asker may cancel %s routine", name)
		}
	}
	if mayCancelRoutine(ai.WithRunRequester(context.Background(), "", sponsor.String()), agent, theirs) {
		t.Error("a run for someone unidentified may cancel a routine")
	}
	for _, ctx := range []context.Context{ai.WithoutRunRequester(context.Background()), ai.WithRunRequester(context.Background(), sponsor.String(), sponsor.String())} {
		if !mayCancelRoutine(ctx, agent, anothers) {
			t.Error("a run for the sponsor may not cancel a routine here")
		}
	}
}

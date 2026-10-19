package Calendar

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// roleIn is the role who has on e, or "" when they aren't on it.
func roleIn(e *Event, who *model.UserInfo) string {
	for _, a := range e.Attendees {
		if a.UUID == who.UserDgraphInfo.Uuid {
			return a.Role
		}
	}
	return ""
}

// answerIn is who's answer on e.
func answerIn(e *Event, who *model.UserInfo) string {
	for _, a := range e.Attendees {
		if a.UUID == who.UserDgraphInfo.Uuid {
			return a.Response
		}
	}
	return ""
}

// The Event Form (calendar FORM): a guest is required or optional, and a
// change of role keeps their answer. A list that names no role keeps theirs.
func TestAGuestMadeOptionalKeepsTheirAnswer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	if _, err := h.s.Answer(ctx, maya, e.ID, AnswerInput{Answer: Accepted}); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(e.Sequence), Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid, Role: roleOptional}}})
	if err != nil {
		t.Fatal(err)
	}
	if roleIn(got, maya) != roleOptional || answerIn(got, maya) != responseAccepted {
		t.Fatalf("Maya is %q, answered %q", roleIn(got, maya), answerIn(got, maya))
	}
	// The next list names no role: hers stays.
	got, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(got.Sequence), Title: str("Design review, round 2"),
		Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil || roleIn(got, maya) != roleOptional {
		t.Fatalf("a list naming no role: %q %v", roleIn(got, maya), err)
	}
	got, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(got.Sequence), Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid, Role: roleRequired}}})
	if err != nil || roleIn(got, maya) != roleRequired || answerIn(got, maya) != responseAccepted {
		t.Fatalf("required again: %q %q %v", roleIn(got, maya), answerIn(got, maya), err)
	}
}

// A whole series changes a guest's role the same way.
func TestASeriesGuestMadeOptional(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya := h.member("Sam"), h.member("Maya")
	s := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	got, err := h.s.Update(ctx, sam, s.ID, EventPatch{Sequence: seq(s.Sequence), Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid, Role: roleOptional}}})
	if err != nil || roleIn(got, maya) != roleOptional {
		t.Fatalf("the series: %q %v", roleIn(got, maya), err)
	}
}

// A guest the organiser lets invite others (Google's "Invite others", on by
// default) adds guests, and changes nothing else; with it off, they can't.
func TestAGuestLetToInviteAddsGuestsAndNothingElse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas, lee := h.member("Sam"), h.member("Maya"), h.member("Jonas"), h.member("Lee")
	e := h.create(sam, "Design review", maya)
	both := []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {UUID: jonas.UserDgraphInfo.Uuid}}
	got, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(e.Sequence), Title: str("Design review"), Attendees: &both})
	if err != nil || guestNames(got) != "Maya,Jonas" {
		t.Fatalf("Maya invites Jonas: %+v %v", got, err)
	}
	refused := map[string]EventPatch{
		"taking Jonas off": {Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}},
		"inviting Lee and renaming it": {Title: str("Mine"), Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid},
			{UUID: jonas.UserDgraphInfo.Uuid}, {UUID: lee.UserDgraphInfo.Uuid}}},
		"making Jonas optional": {Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {UUID: jonas.UserDgraphInfo.Uuid, Role: roleOptional}}},
		"renaming it":           {Title: str("Mine")},
	}
	for name, p := range refused {
		p.Sequence = seq(got.Sequence)
		if _, err := h.s.Update(ctx, maya, e.ID, p); !errors.Is(err, ErrOnlyCreatorChanges) {
			t.Errorf("%s: %v", name, err)
		}
	}
	no := false
	off, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(got.Sequence), GuestsCanInvite: &no})
	if err != nil {
		t.Fatal(err)
	}
	three := append(both, GuestInput{UUID: lee.UserDgraphInfo.Uuid})
	if _, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(off.Sequence), Attendees: &three}); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("with inviting off, Maya invited Lee: %v", err)
	}
}

// A guest let to invite adds a guest to a whole series too.
func TestAGuestLetToInviteAddsToASeries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	lon := zone(t, "Europe/London")
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	s := h.series(sam, "Stand-up", "FREQ=WEEKLY", time.Date(2026, 10, 20, 9, 15, 0, 0, lon), 15, maya)
	got, err := h.s.Update(ctx, maya, s.ID, EventPatch{Sequence: seq(s.Sequence),
		Attendees: &[]GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {UUID: jonas.UserDgraphInfo.Uuid}}})
	if err != nil || guestNames(got) != "Maya,Jonas" {
		t.Fatalf("the series: %+v %v", got, err)
	}
}

package Calendar

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// What an event's call made (its recording and transcript) is deleted and
// brought back by whoever may change the event (meet-02), by the rule that
// gives it a call and takes it off (mayChange). A guest who may only read the
// event still reads what its call made, and no more; anyone else is told
// there is no such event.
func TestOnlyWhoMayChangeAnEventDeletesWhatItsCallMade(t *testing.T) {
	h := newHarness(t)
	jonas, maya, lee := h.member("Jonas"), h.member("Maya"), h.member("Lee")
	ctx := context.Background()
	start, end := timed(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), 60, "Europe/London")
	ev, err := h.s.Create(ctx, jonas, EventInput{Title: "Weekly sync", Start: start, End: end,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}}})
	if err != nil {
		t.Fatal(err)
	}
	own := []uuid.UUID{uuid.MustParse(ev.ID)}

	if keys, err := h.s.CallArtefactKeysToChange(ctx, jonas, ev.ID); err != nil || !slices.Equal(keys.Events, own) {
		t.Fatalf("Jonas, its organiser: %+v, %v; want the event's keys", keys, err)
	}
	if _, err := h.s.CallArtefactKeysToChange(ctx, maya, ev.ID); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Fatalf("Maya, a guest who may not change the event: %v, want it refused as any change is", err)
	}
	if keys, err := h.s.CallArtefactKeys(ctx, maya, ev.ID); err != nil || !slices.Equal(keys.Events, own) {
		t.Fatalf("Maya still reads what its call made: %+v, %v", keys, err)
	}
	if _, err := h.s.CallArtefactKeysToChange(ctx, lee, ev.ID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("Lee, who isn't on the event: %v, want no such event", err)
	}

	// Once Jonas lets guests change it, she may. Nothing has changed the event
	// since he made it, so his change names the sequence he made it at.
	yes := true
	if _, err := h.s.Update(ctx, jonas, ev.ID, EventPatch{Sequence: seq(ev.Sequence), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if keys, err := h.s.CallArtefactKeysToChange(ctx, maya, ev.ID); err != nil || !slices.Equal(keys.Events, own) {
		t.Fatalf("Maya, who may change the event now: %+v, %v; want its keys", keys, err)
	}
}

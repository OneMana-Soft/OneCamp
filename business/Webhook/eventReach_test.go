package business

import (
	"context"
	"testing"

	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	"github.com/google/uuid"
)

// A calendar event's webhook goes only to an owner the calendar says may see
// the event (CAL53-G15; business/Calendar webhooks.go), and one that names no
// event goes to nobody: filtered per subscriber, never fanned out raw.
func TestAnEventWebhookGoesOnlyToWhoMaySeeIt(t *testing.T) {
	savedMember, savedEvent := memberNow, readsEvent
	t.Cleanup(func() { memberNow, readsEvent = savedMember, savedEvent })
	ana, ben := uuid.New(), uuid.New()
	event := uuid.NewString()
	memberNow = func(context.Context, uuid.UUID) (bool, error) { return true, nil }
	asked := 0
	readsEvent = func(_ context.Context, who, id string) (bool, error) {
		asked++
		return who == ana.String() && id == event, nil
	}
	ctx := context.Background()
	for who, want := range map[uuid.UUID]hooksBusiness.Reach{ana: hooksBusiness.Reaches, ben: hooksBusiness.OwnerOutOfEvent} {
		got, err := reachOf(ctx, who, needs{}, needsOf(ctx, "event.created", map[string]interface{}{"event_id": event}))
		if err != nil || got != want {
			t.Errorf("an event.created to %s: %v (%v), want %v", who, got, err, want)
		}
	}
	if asked != 2 {
		t.Errorf("the calendar was asked %d times, want once per owner", asked)
	}
	if got, _ := reachOf(ctx, ana, needs{}, needsOf(ctx, "event.updated", map[string]interface{}{})); got != hooksBusiness.OwnerOutOfEvent {
		t.Errorf("an event webhook that names no event reached its owner: %v", got)
	}
	if !sentWholeAtOrg("event.created") || sentWholeAtOrg("event.rsvp") {
		t.Errorf("event.created is built for outside (whole at the workspace); event.rsvp names a person and isn't offered")
	}
}

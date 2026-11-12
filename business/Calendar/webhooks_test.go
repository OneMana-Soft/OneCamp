package Calendar

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// An event's webhook is busy-shaped: when, on which calendar, never what it
// is called, its notes, its kind or who is on it (CAL53-G15).
func TestAnEventWebhookIsBusyShaped(t *testing.T) {
	start, end := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	row := &models.Event{ID: uuid.New(), CalendarID: uuid.New(), Kind: "away", Title: "Doctor", Notes: "salary review", Location: "Clinic",
		StartAt: &start, EndAt: &end, Visibility: "private", Status: "confirmed", CreatedBy: uuid.New()}
	e := resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarEventCreated, ResourceKind: KindEvent, ResourceUUID: row.ID.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: row.CreatedBy.String()}, At: start, Data: map[string]string{"kind": "away", "changed": "title"}}
	raw, _ := json.Marshal(eventWebhookPayload(e, row))
	for _, secret := range []string{"Doctor", "salary", "Clinic", "away", row.CreatedBy.String(), "title"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the payload carries %q: %s", secret, raw)
		}
	}
	for _, want := range []string{row.ID.String(), row.CalendarID.String(), "2026-11-09T09:00:00Z", "2026-11-09T10:00:00Z"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the payload doesn't name %q: %s", want, raw)
		}
	}
}

// Only the five kinds the webhooks catalogue offers go to webhooks: an
// answer or a reminder names a person.
func TestOnlyFiveEventKindsGoToWebhooks(t *testing.T) {
	for kind, want := range map[resourceEventBusiness.Kind]bool{
		resourceEventBusiness.CalendarEventCreated: true, resourceEventBusiness.CalendarEventUpdated: true,
		resourceEventBusiness.CalendarEventCancelled: true, resourceEventBusiness.CalendarEventDeleted: true,
		resourceEventBusiness.CalendarEventRestored: true, resourceEventBusiness.CalendarEventRSVP: false,
		resourceEventBusiness.CalendarEventReminder: false, resourceEventBusiness.CalendarShared: false,
	} {
		if got := toWebhooks(resourceEventBusiness.Event{Kind: kind, ResourceKind: KindEvent}); got != want {
			t.Errorf("%s goes to webhooks: %v, want %v", kind, got, want)
		}
	}
}

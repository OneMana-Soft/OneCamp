package Calendar

// The calendar's webhooks (gcal-15 head A): an event made, changed, an
// instance cancelled, an event put in the trash or brought back, to the
// workspace's outgoing webhooks that subscribe to it (business/Webhook
// DispatchEvent), as Google Calendar's push channels tell a watcher.
//
// A payload is built for outside and busy-shaped, as the calendar shows an
// event to someone who sees its calendar only as busy (busyView): its id, its
// calendar, its series, when it is and its status; never its title, notes,
// place, kind or who is on it, so it is the same for every subscriber. Each
// webhook is held to its owner (business/Webhook reach.go, ReadEventsWith):
// one the calendar shows the event to, as busy at least, is sent it; anyone
// else, nothing. Details are read through the API as the reader
// (GET /v1/events/{id}), under the calendar's own rules. An answer or a
// reminder names a person, so neither is sent.

import (
	"context"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "calendar.webhooks", Wants: toWebhooks, Run: sendWebhook})
	webhookBusiness.ReadEventsWith(func(ctx context.Context, userUUID, eventID string) (bool, error) {
		return stored.webhookSees(ctx, userUUID, eventID)
	})
}

// webhookKinds are the event kinds the webhooks catalogue offers.
var webhookKinds = map[resourceEventBusiness.Kind]bool{
	resourceEventBusiness.CalendarEventCreated: true, resourceEventBusiness.CalendarEventUpdated: true,
	resourceEventBusiness.CalendarEventCancelled: true, resourceEventBusiness.CalendarEventDeleted: true,
	resourceEventBusiness.CalendarEventRestored: true,
}

// toWebhooks is an event's change a webhook may be sent. Pure.
func toWebhooks(e resourceEventBusiness.Event) bool {
	return webhookKinds[e.Kind] && e.ResourceKind == KindEvent
}

// dispatchWebhook is where an event's webhook goes; a seam for tests.
var dispatchWebhook = webhookBusiness.DispatchEvent

// sendWebhook is the sink: the event as it is stored now, busy-shaped. One
// that can't be read is sent nothing.
func sendWebhook(ctx context.Context, e resourceEventBusiness.Event) {
	id, err := uuid.Parse(e.ResourceUUID)
	if err != nil {
		return
	}
	rows, err := stored.store.getMany(ctx, []uuid.UUID{id})
	if err != nil || len(rows) != 1 {
		return
	}
	dispatchWebhook(ctx, string(e.Kind), eventWebhookPayload(e, rows[0]))
}

// eventWebhookPayload is what an event's webhook carries: ids, when, its
// status and transparency, and for a cancelled instance which one and how
// far. Never a title, notes, a place, a kind, a person or what changed. Pure.
func eventWebhookPayload(e resourceEventBusiness.Event, row *models.Event) map[string]interface{} {
	p := map[string]interface{}{"event_id": row.ID.String(), "calendar_id": row.CalendarID.String(),
		"status": row.Status, "transparency": row.Transparency, "at": e.At.UTC().Format(time.RFC3339Nano)}
	if row.SeriesID != nil {
		p["series_id"] = row.SeriesID.String()
	}
	if row.RecurrenceID != nil {
		p["recurrence_id"] = row.RecurrenceID.UTC().Format(time.RFC3339)
	}
	if row.StartAt != nil && row.EndAt != nil {
		p["start"], p["end"], p["all_day"] = row.StartAt.UTC().Format(time.RFC3339), row.EndAt.UTC().Format(time.RFC3339), false
	} else {
		p["start"], p["end"], p["all_day"] = row.StartDate, row.EndDate, true
	}
	if v := e.Data["instance"]; v != "" {
		p["instance_id"] = v
	}
	if v := e.Data["scope"]; v != "" {
		p["scope"] = v
	}
	if row.RRule != "" {
		p["repeats"] = true
	}
	return p
}

// webhookSees is whether the calendar shows an event to someone, as busy at
// least: they are on it, or they may see its calendar, and it isn't free
// time on a calendar they see only as busy (freeTimeHidden, CalendarEvents'
// rule). A person who isn't an active member sees nothing.
func (s *service) webhookSees(ctx context.Context, userUUID, eventID string) (bool, error) {
	person, err := uuid.Parse(userUUID)
	if err != nil {
		return false, nil
	}
	id, _, err := parseRef(eventID)
	if err != nil {
		return false, nil
	}
	rows, err := s.store.getMany(ctx, []uuid.UUID{id})
	if err != nil || len(rows) != 1 {
		return false, err
	}
	e := rows[0]
	guests, err := s.store.attendees(ctx, []uuid.UUID{e.ID})
	if err != nil {
		return false, err
	}
	if roleOf(e, guests[e.ID], person) != roleNone {
		return true, nil
	}
	node, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, person.String())
	if err != nil || node == nil {
		return false, nil
	}
	decided, err := s.decideCalendars(ctx, accessBusiness.Subject{UserUID: node.Uid, UserUUID: person.String()}, []string{e.CalendarID.String()})
	if err != nil {
		return false, err
	}
	cal := decided[e.CalendarID.String()]
	return cal.Level >= accessBusiness.LevelView && !freeTimeHidden(e, cal), nil
}

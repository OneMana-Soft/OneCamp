package Calendar

// The /event routes' writes by an instance's id (A7). The one store answers a
// repeating event as its instances, each by "<series id>_<its start>"
// (IsInstanceID), and the web and the assistant write back by that id. These
// take one: a change or a delete is to that instance alone, and leaving it
// leaves the series. The calendar's own API asks which (this event, this and
// following, all events). The two old stores have no instances: there such
// an id names no event.

import (
	"context"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// UpdateEventInstance is UpdateEvent for one instance of a repeating event.
func UpdateEventInstance(ctx context.Context, id string, in adapter.CreateOrUpdateEventInput, who *model.UserInfo) error {
	if !helpers.CalendarInPostgres() {
		return ErrEventNotFound
	}
	return stored.legacyUpdateAt(ctx, id, in, who)
}

// DeleteEventInstance is DeleteEvent for one instance of a repeating event.
func DeleteEventInstance(ctx context.Context, id string, who *model.UserInfo) error {
	if !helpers.CalendarInPostgres() {
		return ErrEventNotFound
	}
	return stored.legacyDeleteAt(ctx, id, who)
}

// LeaveEventInstance is LeaveEvent from an instance of a repeating event: off
// the whole series.
func LeaveEventInstance(ctx context.Context, id string, who *model.UserInfo) error {
	if !helpers.CalendarInPostgres() {
		return ErrEventNotFound
	}
	return stored.legacyLeaveAt(ctx, id, who)
}

package Calendar

// A person's zone, for an event they make without naming one (CAL3). The app
// has never sent its zone with an event, so it is the zone the person keeps
// (Person Preferences' `zone`, calendar A3), else the first one they have
// given OneCamp that this server's clock can read: their quiet hours', their
// newest booking page's, their newest task repeat's. Otherwise UTC, which
// leaves an event exactly as it was before the store: instants.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// zoneFrom is the zone the hints give, or UTC. Pure.
func zoneFrom(h models.ZoneHints) string {
	loc, ok := helpers.FirstLocation(h.Stored, h.QuietHours, h.BookingPage, h.TaskRepeat)
	if !ok {
		return "UTC"
	}
	return loc.String()
}

// zonesOf is each of these people's zone.
func (s *service) zonesOf(ctx context.Context, people []uuid.UUID) (map[uuid.UUID]string, error) {
	hints, err := s.store.zoneHints(ctx, people)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(people))
	for _, p := range people {
		out[p] = zoneFrom(hints[p])
	}
	return out, nil
}

// zoneOf is one person's zone; UTC when it can't be read, as before.
func (s *service) zoneOf(ctx context.Context, person uuid.UUID) string {
	zones, err := s.zonesOf(ctx, []uuid.UUID{person})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a zone couldn't be read, so UTC: %v", err)
		return "UTC"
	}
	return zones[person]
}

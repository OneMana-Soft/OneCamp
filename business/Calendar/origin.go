package Calendar

// Where a calendar's events come from, and whether its provider lets OneCamp
// write to it (V-594). A calendar moved from Google or Outlook that is
// see-only there (one shared with see-only rights, holidays) is read-only in
// calendar sync's ledger (calendar_sync_calendars.read_only): the push of a
// new event on it is refused later (syncGoogle.go pushOne, ErrReadOnly), so
// the list says so and a new event on it is refused up front, as Google does.

import (
	"context"

	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// kindExternal is a calendar brought from another calendar app: a file
// imported, or one of a provider's calendars a move brought in.
const kindExternal = "external"

// calendarOrigin is where a calendar's events come from: "google" or
// "microsoft" (moved from there and kept in step), "ics" (read from an
// address) or "onecamp"; and whether its provider lets OneCamp only read it.
type calendarOrigin struct {
	Source   string
	ReadOnly bool
}

// originOf is where a calendar of kind comes from, by the ledger's rows that
// come into it. One read from an address is that address's, never written
// to. Only a calendar a move made for one of a provider's calendars takes
// the provider's word: a person's own calendar holds their primary's events
// and stays OneCamp's, and one a file brought has no row. One name moved from
// two providers comes into one calendar: a row that can only be read wins,
// as the push to it would be refused. Pure.
func originOf(kind string, rows []models.SyncCalendar) calendarOrigin {
	switch kind {
	case kindSubscribed:
		return calendarOrigin{Source: providerICS, ReadOnly: true}
	case kindExternal:
		var o calendarOrigin
		for _, r := range rows {
			if o.Source == "" || (r.ReadOnly && !o.ReadOnly) {
				o = calendarOrigin{Source: r.Provider, ReadOnly: r.ReadOnly}
			}
		}
		if o.Source != "" {
			return o
		}
	}
	return calendarOrigin{Source: sourceOneCamp}
}

// originsOf are where each of cals comes from, by id: one read of the ledger
// for the whole list, of the calendars a move may have made.
func (s *service) originsOf(ctx context.Context, cals []models.Calendar) (map[uuid.UUID]calendarOrigin, error) {
	var moved []uuid.UUID
	for _, c := range cals {
		if c.Kind == kindExternal {
			moved = append(moved, c.ID)
		}
	}
	rows := map[uuid.UUID][]models.SyncCalendar{}
	if len(moved) > 0 {
		synced, err := s.ledger.calendarsInto(ctx, moved)
		if err != nil {
			return nil, err
		}
		for _, r := range synced {
			if r.CalendarID != nil {
				rows[*r.CalendarID] = append(rows[*r.CalendarID], r)
			}
		}
	}
	out := make(map[uuid.UUID]calendarOrigin, len(cals))
	for _, c := range cals {
		out[c.ID] = originOf(c.Kind, rows[c.ID])
	}
	return out, nil
}

// CalendarReadOnlyError is a new event put on a calendar its provider lets
// OneCamp only read, said in words that name the provider.
type CalendarReadOnlyError struct{ provider string }

func (e *CalendarReadOnlyError) Error() string {
	return "This calendar is read-only in " + syncWithOf(e.provider).name + ". Put the event on another calendar."
}

// mayAddTo is nil when a new event may go on cal as far as its provider
// goes, else a CalendarReadOnlyError: the one check of a calendar a member
// picks for an event (writableCalendar), before anything is written. A pull
// never asks it: it brings the provider's own events in. A calendar read from
// an address never gets here, as the access rules let its person only view
// it.
func (s *service) mayAddTo(ctx context.Context, cal uuid.UUID) error {
	cals, err := s.store.calendarsByID(ctx, []uuid.UUID{cal})
	if err != nil {
		return err
	}
	origins, err := s.originsOf(ctx, cals)
	if err != nil {
		return err
	}
	if o := origins[cal]; o.ReadOnly {
		return &CalendarReadOnlyError{provider: o.Source}
	}
	return nil
}

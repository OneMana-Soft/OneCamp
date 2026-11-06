package Calendar

// The calendar under retention rules and holds (the governance trail's H2):
// its events are business/Retention's first kind. A rule removes events that
// ended N days ago (models' removable says exactly which), never a held
// person's, and takes its search entry with it; an admin searches a hold's
// events and exports them as iCalendar, through the one codec every export
// uses (icsOf), or as JSON.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	ICS "github.com/akashc777/OneCamp/business/ICS"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	retentionBusiness "github.com/akashc777/OneCamp/business/Retention"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

func init() {
	retentionBusiness.Register(retentionBusiness.Kind{
		Name:    "calendar",
		Label:   heldLabel,
		Policy:  policyBusiness.CalendarRetentionDays,
		Count:   models.CountRemovable,
		Purge:   purgeRemovable,
		Search:  stored.heldSearch,
		Export:  stored.heldExport,
		Formats: []string{"ics", "json"},
	})
}

// purgeRemovable is the calendar's Purge: the events a rule removes, and in
// the same transaction the search writes that delete their entries
// (searchEntriesChangedIn), so a removed event's title, place and guests are
// left in no search. The writes are delivered once it commits, and the
// outbox tries them again until the index has taken them.
func purgeRemovable(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	var removed []string
	var asked []int64
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var err error
		if removed, err = models.PurgeRemovable(ctx, cutoff, limit); err != nil {
			return err
		}
		asked, err = searchEntriesChangedIn(ctx, removed)
		return err
	})
	if err != nil {
		return 0, err
	}
	searchEntriesAsked(ctx, asked)
	return int64(len(removed)), nil
}

// heldLabel is the calendar's items as a count reads.
const heldLabel = "calendar events"

func heldQuery(q retentionBusiness.Query) models.HeldQuery {
	return models.HeldQuery{Text: q.Text, From: q.From, To: q.To}
}

func peopleText(people []uuid.UUID) []string {
	out := make([]string, len(people))
	for i, p := range people {
		out[i] = p.String()
	}
	return out
}

// heldSearch lists people's events matching q, deleted ones included.
func (s *service) heldSearch(ctx context.Context, people []uuid.UUID, q retentionBusiness.Query, limit int) ([]retentionBusiness.Item, error) {
	rows, err := models.HeldEvents(ctx, models.Pool(), peopleText(people), heldQuery(q), limit)
	if err != nil {
		return nil, err
	}
	out := make([]retentionBusiness.Item, 0, len(rows))
	for _, e := range rows {
		it := retentionBusiness.Item{ID: e.ID.String(), Title: titleOf(e, nil), Detail: e.Location,
			Start: e.StartAt, End: e.EndAt, StartDate: e.StartDate, EndDate: e.EndDate, Repeats: e.RRule != "", DeletedAt: e.DeletedAt}
		out = append(out, it)
	}
	return out, nil
}

// heldGuest is one guest in a JSON export.
type heldGuest struct {
	UserID   *uuid.UUID `json:"user_id,omitempty"`
	Email    string     `json:"email,omitempty"`
	Name     string     `json:"name,omitempty"`
	Role     string     `json:"role"`
	Response string     `json:"response"`
}

// heldEvent is one event in a JSON export: everything the row says, with its
// guests and, for a series, its changed instances.
type heldEvent struct {
	ID           uuid.UUID   `json:"id"`
	ICalUID      string      `json:"ical_uid"`
	CalendarID   uuid.UUID   `json:"calendar_id"`
	Kind         string      `json:"kind"`
	Title        string      `json:"title"`
	Notes        string      `json:"notes"`
	Location     string      `json:"location"`
	Start        *time.Time  `json:"start,omitempty"`
	End          *time.Time  `json:"end,omitempty"`
	StartDate    string      `json:"start_date,omitempty"`
	EndDate      string      `json:"end_date,omitempty"`
	TZ           string      `json:"tz"`
	RRule        string      `json:"rrule,omitempty"`
	Until        *time.Time  `json:"until,omitempty"`
	RecurrenceID *time.Time  `json:"recurrence_id,omitempty"`
	Status       string      `json:"status"`
	Visibility   string      `json:"visibility"`
	Organizer    uuid.UUID   `json:"organizer"`
	Guests       []heldGuest `json:"guests"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
	DeletedAt    *time.Time  `json:"deleted_at,omitempty"`
	DeletedBy    *uuid.UUID  `json:"deleted_by,omitempty"`
	Instances    []heldEvent `json:"changed_instances,omitempty"`
}

func asHeld(e *models.Event, guests []models.Attendee) heldEvent {
	out := heldEvent{ID: e.ID, ICalUID: e.ICalUID, CalendarID: e.CalendarID, Kind: e.Kind, Title: titleOf(e, nil),
		Notes: e.Notes, Location: e.Location, Start: e.StartAt, End: e.EndAt, StartDate: e.StartDate, EndDate: e.EndDate,
		TZ: e.TZ, RRule: e.RRule, Until: e.UntilAt, RecurrenceID: e.RecurrenceID, Status: e.Status, Visibility: e.Visibility,
		Organizer: e.CreatedBy, Guests: []heldGuest{}, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		DeletedAt: e.DeletedAt, DeletedBy: e.DeletedBy}
	for _, g := range guests {
		out.Guests = append(out.Guests, heldGuest{UserID: g.UserID, Email: g.Email, Name: g.Name, Role: g.Role, Response: g.Response})
	}
	return out
}

// heldExport writes people's events matching q as one .ics or one JSON file,
// deleted ones included, and counts them (a series is one).
func (s *service) heldExport(ctx context.Context, w io.Writer, people []uuid.UUID, q retentionBusiness.Query, format string, part retentionBusiness.Part) (retentionBusiness.Written, error) {
	hq := heldQuery(q)
	if part.After != "" {
		at, id, err := heldCursorOf(part.After)
		if err != nil {
			return retentionBusiness.Written{}, err
		}
		hq.AfterStart, hq.AfterID = at, id.String()
	}
	// One more than the part holds says whether more remain past it.
	rows, err := models.HeldEvents(ctx, models.Pool(), peopleText(people), hq, part.Size+1)
	if err != nil {
		return retentionBusiness.Written{}, err
	}
	var written retentionBusiness.Written
	if len(rows) > part.Size {
		rows = rows[:part.Size]
		written.Next = heldCursor(rows[len(rows)-1])
	}
	written.N = len(rows)
	note := retentionBusiness.PartNote(heldLabel, part, written)
	if format == "ics" {
		each, err := s.icsOf(ctx, uuid.Nil, rows)
		if err != nil {
			return retentionBusiness.Written{}, err
		}
		var evs []ICS.Event
		for _, one := range each {
			evs = append(evs, one...)
		}
		_, err = w.Write(ICS.WriteMessage(ICS.Message{Method: "PUBLISH", Events: evs, Note: note}))
		return written, err
	}
	var series, ids []uuid.UUID
	for _, e := range rows {
		ids = append(ids, e.ID)
		if e.RRule != "" {
			series = append(series, e.ID)
		}
	}
	overrides, err := s.store.overridesOf(ctx, series, everything)
	if err != nil {
		return retentionBusiness.Written{}, err
	}
	for _, o := range overrides {
		ids = append(ids, o.ID)
	}
	guests, err := s.store.attendees(ctx, ids)
	if err != nil {
		return retentionBusiness.Written{}, err
	}
	bySeries := map[uuid.UUID][]heldEvent{}
	for _, o := range overrides {
		if o.SeriesID != nil {
			bySeries[*o.SeriesID] = append(bySeries[*o.SeriesID], asHeld(o, guests[o.ID]))
		}
	}
	out := make([]heldEvent, 0, len(rows))
	for _, e := range rows {
		h := asHeld(e, guests[e.ID])
		h.Instances = bySeries[e.ID]
		out = append(out, h)
	}
	file := map[string]interface{}{"part": part.Number, "more": written.Next != "", "events": out}
	if note != "" {
		file["note"] = note
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return written, enc.Encode(file)
}

// heldCursor is where the next part of an export starts: after e, by its
// start (an all-day event's at midnight UTC, as HeldEvents orders it) and id.
func heldCursor(e *models.Event) string {
	at := time.Time{}
	if e.StartAt != nil {
		at = *e.StartAt
	} else if d, err := time.Parse(time.DateOnly, e.StartDate); err == nil {
		at = d
	}
	return at.UTC().Format(time.RFC3339Nano) + "_" + e.ID.String()
}

// heldCursorOf reads a heldCursor back; anything else is a bad request.
func heldCursorOf(c string) (time.Time, uuid.UUID, error) {
	at, id, ok := strings.Cut(c, "_")
	t, terr := time.Parse(time.RFC3339Nano, at)
	u, uerr := uuid.Parse(id)
	if !ok || terr != nil || uerr != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: that part isn't one this hold's export offered; export it again from the start", retentionBusiness.ErrBadRequest)
	}
	return t, u, nil
}

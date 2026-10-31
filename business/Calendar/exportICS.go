package Calendar

// Exporting a calendar (calendar A4, SY-18): every event the person is on,
// as one .ics another calendar imports whole, through the one codec
// (business/ICS) and the one way a series is written (seriesEvents):
// series with their skipped, added and changed instances, all-day dates,
// zones, notes in full, places, every guest with their answer, and the
// person's own reminders as alarms. Importing it back here makes nothing
// twice (importICS.go), and importing it elsewhere keeps each UID, so a
// later export updates what an earlier one made.

import (
	"context"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// maxExportEvents is the most events (a series is one) an export writes.
const maxExportEvents = 20000

// everything is a window over every event there is.
var everything = models.Window{
	From: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC),
	FromDate: "1900-01-01", ToDate: "2200-01-01",
}

// Export writes every event who is on as one iCalendar file, and says how
// many events it holds.
func (s *service) Export(ctx context.Context, who *model.UserInfo, to io.Writer) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return 0, ErrEventNotFound
	}
	rows, err := s.store.rangeOf(ctx, me, everything, maxExportEvents)
	if err != nil {
		return 0, err
	}
	// A subscribed calendar's events are its address's, not the person's.
	rows = slices.DeleteFunc(rows, func(e *models.Event) bool { return e.Source == sourceSubscribed })
	each, err := s.icsOf(ctx, me, rows)
	if err != nil {
		return 0, err
	}
	var evs []ICS.Event
	for _, one := range each {
		evs = append(evs, one...)
	}
	_, err = to.Write(ICS.WriteMessage(ICS.Message{Method: "PUBLISH", Events: evs}))
	return len(rows), err
}

// icsOf is each row (a series or a single event) as the codec's events, as
// reader sees them: a series with its skipped, added and changed instances,
// titled as reader reads them (titleOf), notes in full, every guest with
// their answer, reader's own reminders as alarms, and where it's joined. An
// export, a feed and two-way sync all write events through it.
func (s *service) icsOf(ctx context.Context, reader uuid.UUID, rows []*models.Event) ([][]ICS.Event, error) {
	var series, ids []uuid.UUID
	for _, e := range rows {
		ids = append(ids, e.ID)
		if e.RRule != "" {
			series = append(series, e.ID)
		}
	}
	overrides, err := s.store.overridesOf(ctx, series, everything)
	if err != nil {
		return nil, err
	}
	bySeries := map[uuid.UUID][]*models.Event{}
	for _, o := range overrides {
		if o.SeriesID != nil {
			bySeries[*o.SeriesID] = append(bySeries[*o.SeriesID], o)
		}
	}
	guests, err := s.store.attendees(ctx, ids)
	if err != nil {
		return nil, err
	}
	reminders, err := s.rem.remindersFor(ctx, reader, ids)
	if err != nil {
		return nil, err
	}
	people, err := s.peopleNamed(ctx, rows, guests)
	if err != nil {
		return nil, err
	}
	title := s.titler(ctx, accessBusiness.Subject{UserUUID: reader.String()}, append(slices.Clone(rows), overrides...))
	now := s.now()
	out := make([][]ICS.Event, 0, len(rows))
	for _, e := range rows {
		attendees := exportedGuests(guests[e.ID], people)
		alarms := alarmsOf(reminders[e.ID])
		organiser := addressOfPerson(e.CreatedBy, people)
		base := func(row *models.Event) ICS.Event {
			ev := ICS.Event{UID: exportedUID(e.ICalUID), Sequence: row.Sequence, Stamp: now, Summary: title(row),
				Description: notesAsText(row), Location: row.Location, Organizer: organiser, Attendees: attendees,
				Transparent: row.Transparency == transparencyTransparent, Alarms: alarms, Conference: callLinkIn(notesAsText(row))}
			if row.Status == statusCancelled {
				ev.Status = "CANCELLED"
			}
			if row.AllDay() {
				ev.StartDate, ev.EndDate = row.StartDate, row.EndDate
			} else {
				ev.Start, ev.End = *row.StartAt, *row.EndAt
				ev.Zone = row.TZ
			}
			return ev
		}
		out = append(out, seriesEvents(e, bySeries[e.ID], base))
	}
	return out, nil
}

// peopleNamed is every member the events name, organisers and guests, by
// uuid: their names and addresses.
func (s *service) peopleNamed(ctx context.Context, rows []*models.Event, guests map[uuid.UUID][]models.Attendee) (map[string]*dgraphStruct.DgraphUser, error) {
	seen := map[string]bool{}
	var want []string
	add := func(id uuid.UUID) {
		if k := id.String(); !seen[k] {
			seen[k] = true
			want = append(want, k)
		}
	}
	for _, e := range rows {
		add(e.CreatedBy)
		for _, g := range guests[e.ID] {
			if g.UserID != nil {
				add(*g.UserID)
			}
		}
	}
	found, err := s.people(ctx, want)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*dgraphStruct.DgraphUser, len(found))
	for _, p := range found {
		if p != nil {
			out[p.Uuid] = p
		}
	}
	return out, nil
}

// addressOfPerson is a member as a calendar file names them.
func addressOfPerson(id uuid.UUID, people map[string]*dgraphStruct.DgraphUser) ICS.Address {
	p := people[id.String()]
	if p == nil {
		return ICS.Address{}
	}
	return ICS.Address{Name: p.DisplayName(), Email: p.EmailID}
}

// exportedGuests are an event's guests as a calendar file names them, with
// their answers: members by their address, guests by email by theirs. A
// member without an address is left out (a calendar file names people by
// address alone).
func exportedGuests(guests []models.Attendee, people map[string]*dgraphStruct.DgraphUser) []ICS.Attendee {
	out := make([]ICS.Attendee, 0, len(guests))
	for _, g := range guests {
		a := ICS.Attendee{Address: ICS.Address{Name: g.Name, Email: g.Email}, Optional: g.Role == roleOptional, PartStat: partStatOf(g.Response)}
		if g.UserID != nil {
			a.Address = addressOfPerson(*g.UserID, people)
		}
		if a.Email == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// alarmsOf is a person's reminders, minutes before, as alarms on screen.
func alarmsOf(minutes []int) []ICS.Alarm {
	out := make([]ICS.Alarm, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, ICS.Alarm{Before: time.Duration(m) * time.Minute})
	}
	return out
}

// exportedUID is an event's UID as other calendars know it: an imported copy
// of someone else's event (importedUID) is the event it copies.
func exportedUID(uid string) string {
	if base, suffix, ok := strings.Cut(uid, "~"); ok {
		if _, err := uuid.Parse(suffix); err == nil {
			return base
		}
	}
	return uid
}

// callWords are what a call's link has in it: never just any link.
var callWords = regexp.MustCompile(`(?i)meet|zoom|teams|whereby|call|livekit|jitsi`)

var links = regexp.MustCompile(`https?://[^\s<>"')]+`)

// callLinkIn is the first link in notes that goes to a call, as the web's
// Join finds it (lib/calendar/joinLink.ts), or "". Pure.
func callLinkIn(notes string) string {
	for _, l := range links.FindAllString(notes, -1) {
		l = strings.TrimRight(strings.ReplaceAll(l, "&amp;", "&"), ".,;:!?")
		if callWords.MatchString(l) {
			return l
		}
	}
	return ""
}

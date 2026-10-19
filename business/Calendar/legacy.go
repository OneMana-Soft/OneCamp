package Calendar

// The /event routes over the one store (A1). With the one store on (the
// default; not with CALENDAR_STORE=graph) the
// functions the routes call (CreateEvent, UpdateEvent, DeleteEvent,
// LeaveEvent, GetEventsList, GetEvent) answer from here, with the same JSON
// (the graph's Event shape the web reads), the same rules and the same
// errors, until the web moves to the calendar's own API (A7):
//
//   - a write is a whole replace, a guest list left out leaves the guests,
//     focus and away are pointers (left out: unchanged), and there is no
//     sequence to check: the last writer wins, as it always has;
//   - the web sends no zone, so an event is given its organiser's, and an
//     all-day event comes as the sender's midnights (legacyWhen);
//   - an all-day event is answered with its dates and midnight-UTC instants,
//     as Google's all-day events are, so the web puts it on the same date for
//     everyone (lib/calendar/allDay.ts);
//   - a repeating event comes as its instances, each by its own id
//     ("<series id>_<its start>"); a write to an instance is to it alone, one
//     to a series by its own id to all of it.

import (
	"context"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// allDayAt is the instant an all-day event's date is said as, in one shape.
type allDayAt func(date string, e *models.Event) time.Time

// webDates says a date as its UTC midnight, beside the date itself: how the
// web reads an all-day event.
func webDates(date string, _ *models.Event) time.Time { return utcMidnightOf(date) }

// zoneDates says a date as its midnight in the event's zone: the stretch of
// time the event takes, for who is busy and when.
func zoneDates(date string, e *models.Event) time.Time {
	loc, err := loadZone(e.TZ)
	if err != nil {
		loc = time.UTC
	}
	return midnightOf(date, loc)
}

// legacyOf is an event in the /event routes' JSON, for reader. Pure.
func legacyOf(e *models.Event, guests []models.Attendee, names map[string]*dgraphStruct.DgraphUser, reader uuid.UUID, at allDayAt) *dgraphStruct.DgraphEvent {
	ev := &dgraphStruct.DgraphEvent{Uuid: e.ID.String(), Title: e.Title, Description: e.Notes, Hue: e.Hue, Location: e.Location}
	if e.AllDay() {
		start, end := at(e.StartDate, e), at(e.EndDate, e)
		ev.StartTime, ev.EndTime = &start, &end
		ev.AllDay, ev.StartDate, ev.EndDate = true, e.StartDate, e.EndDate
	} else {
		start, end := *e.StartAt, *e.EndAt
		ev.StartTime, ev.EndTime = &start, &end
	}
	ev.CreatedBy = graphPerson(e.CreatedBy, names)
	for _, a := range guestsShown(e, guests, reader) {
		g := dgraphStruct.EventGuest{Role: a.Role, Response: a.Response, Note: a.Note}
		if a.UserID != nil {
			ev.Participants = append(ev.Participants, graphPerson(*a.UserID, names))
			g.UserUUID = a.UserID.String()
		} else {
			g.Email, g.Name = a.Email, a.Name
			if e.CreatedBy == reader {
				g.Invite = a.InviteState
			}
		}
		ev.Guests = append(ev.Guests, g)
	}
	if e.GoogleEventID != "" {
		gid := e.GoogleEventID
		ev.GoogleCalendarEventId = &gid
	}
	focus, away := e.Kind == string(EventFocus), e.Kind == string(EventAway)
	ev.IsFocus, ev.IsAway = &focus, &away
	return ev
}

// graphPerson is someone as the graph's event named them.
func graphPerson(id uuid.UUID, names map[string]*dgraphStruct.DgraphUser) *dgraphStruct.DgraphUser {
	p := &dgraphStruct.DgraphUser{Uuid: id.String()}
	if u := names[p.Uuid]; u != nil {
		p.Uid, p.UserName, p.UserFullName = u.Uid, u.UserName, u.UserFullName
	}
	return p
}

// legacyKind is what an event becomes when a write sets its focus and away
// flags: away beats focus, a flag left out leaves the kind, and a flag turned
// off makes that kind a plain event. Pure.
func legacyKind(cur EventKind, isFocus, isAway *bool) EventKind {
	off := false
	if isAway != nil && *isAway {
		isFocus = &off
	} else if isFocus != nil && *isFocus {
		isAway = &off
	}
	k := cur
	if isAway != nil {
		if *isAway {
			k = EventAway
		} else if k == EventAway {
			k = EventPlain
		}
	}
	if isFocus != nil {
		if *isFocus {
			k = EventFocus
		} else if k == EventFocus {
			k = EventPlain
		}
	}
	return k
}

// legacyWhen puts the times an /event route sent on e. They are dates when
// they are the midnights of whole days in the event's zone, or, for an event
// that was all-day, the midnights of whatever zone the sender is in (the web
// sends its reader's own); otherwise they are instants in the event's zone.
// A zone guessed wrong leaves a timed event, as before the store. Focus time
// is always instants (check). Pure.
func legacyWhen(e *models.Event, start, end time.Time, was *models.Event) {
	loc, err := loadZone(e.TZ)
	if err != nil {
		loc = time.UTC
	}
	var first, after string
	ok := false
	if e.Kind != string(EventFocus) {
		first, after, ok = datesIn(start, end, loc)
		if !ok && was != nil && was.AllDay() {
			first, after, ok = datesAtAnyOffset(start, end, was.StartDate)
		}
	}
	if ok {
		e.StartAt, e.EndAt = nil, nil
		e.StartDate, e.EndDate, e.EndTZ = first, after, ""
		return
	}
	s, en := start.UTC(), end.UTC()
	e.StartAt, e.EndAt = &s, &en
	e.StartDate, e.EndDate = "", ""
}

func legacyTimes(in adapter.CreateOrUpdateEventInput) (time.Time, time.Time, error) {
	start, err := parseEventTime(in.StartTime, "start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := parseEventTime(in.EndTime, "end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, inputError("The end is before the start.")
	}
	return start, end, nil
}

func guestInputs(ids []string) []GuestInput {
	out := make([]GuestInput, 0, len(ids))
	for _, id := range ids {
		out = append(out, GuestInput{UUID: id})
	}
	return out
}

// legacyCreate is CreateEvent over the store.
func (s *service) legacyCreate(ctx context.Context, who *model.UserInfo, in adapter.CreateOrUpdateEventInput) (*adapter.OutputEvent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	start, end, err := legacyTimes(in)
	if err != nil {
		return nil, err
	}
	e := blank(s.zoneOf(ctx, personOf(who)))
	e.Kind = string(legacyKind(EventPlain, in.IsFocus, in.IsAway))
	e.Title, e.Notes = in.Title, in.Description
	legacyWhen(e, start, end, nil)
	created, guests, err := s.create(ctx, who, e, guestInputs(in.Participants), in.SyncToGoogleCalendar)
	if err != nil {
		return nil, err
	}
	return &adapter.OutputEvent{
		EventUuid: created.ID.String(), Title: created.Title, Description: created.Notes,
		StartTime: &start, EndTime: &end, CreatedBy: who.UserDgraphInfo.Uuid,
		Participants: guestIDs(guests),
		IsFocus:      created.Kind == string(EventFocus), IsAway: created.Kind == string(EventAway),
	}, nil
}

// legacyUpdate is UpdateEvent over the store.
func (s *service) legacyUpdate(ctx context.Context, id uuid.UUID, in adapter.CreateOrUpdateEventInput, who *model.UserInfo) error {
	return s.legacyUpdateAt(ctx, id.String(), in, who)
}

// legacyUpdateAt is an /event route's write by any id the calendar answers
// with: to an instance of a series alone (who comes stays the series'), to a
// series by its own id all of it.
func (s *service) legacyUpdateAt(ctx context.Context, ref string, in adapter.CreateOrUpdateEventInput, who *model.UserInfo) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	inst, guests, err := s.resolve(ctx, who, ref)
	if err != nil {
		return err
	}
	owner := inst.owner()
	if err := mayChange(roleOf(owner, guests, personOf(who)), owner); err != nil {
		return err
	}
	start, end, err := legacyTimes(in)
	if err != nil {
		return err
	}
	next := cloneEvent(inst.Event)
	next.Kind = string(legacyKind(EventKind(inst.Kind), in.IsFocus, in.IsAway))
	next.Title = in.Title
	// The web edits notes as the text it was given, so notes keep their
	// format: HTML stays HTML, through the codec again.
	if next.Notes, err = canonicalNotes(in.Description, inst.NotesFormat); err != nil {
		return err
	}
	legacyWhen(next, start, end, inst.Event)
	var wanted *[]GuestInput
	if in.Participants != nil {
		g := guestInputs(in.Participants)
		// The /event routes name members alone: guests invited by email
		// stay on (they come and go through the calendar's own API).
		for _, a := range guests {
			if a.UserID == nil && a.Email != "" {
				g = append(g, GuestInput{Email: a.Email, Name: a.Name})
			}
		}
		wanted = &g
	}
	switch {
	case inst.rid != nil:
		if wanted != nil && !sameGuests(*wanted, owner, guests) {
			return inputError("Change who comes to a repeating event for all of it, from the calendar.")
		}
		_, err = s.updateThis(ctx, who, inst, guests, next, nil)
	case inst.RRule != "":
		oldStart, oldLoc, _ := seriesTime(inst.Event)
		newStart, newLoc, _ := seriesTime(next)
		if err := moveRule(next, oldStart, oldLoc, newStart, newLoc); err != nil {
			return err
		}
		if err := setRule(next, next.RRule); err != nil {
			return err
		}
		_, _, err = s.updateSeries(ctx, who, inst.Event, next, guests, wanted, nil, nil)
	default:
		_, _, err = s.update(ctx, who, inst.Event, next, guests, wanted, nil)
	}
	return err
}

// legacyDelete is DeleteEvent over the store: to the trash.
func (s *service) legacyDelete(ctx context.Context, id uuid.UUID, who *model.UserInfo) error {
	return s.legacyDeleteAt(ctx, id.String(), who)
}

// legacyDeleteAt is an /event route's delete by any id the calendar answers
// with: an instance of a series alone, a series by its own id all of it.
func (s *service) legacyDeleteAt(ctx context.Context, ref string, who *model.UserInfo) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	inst, guests, err := s.resolve(ctx, who, ref)
	if err != nil {
		return err
	}
	if inst.rid != nil {
		return s.deleteThis(ctx, who, inst, guests, nil)
	}
	return s.remove(ctx, who, inst.Event, guests, nil)
}

// legacyLeave is LeaveEvent over the store.
func (s *service) legacyLeave(ctx context.Context, id uuid.UUID, who *model.UserInfo) error {
	return s.legacyLeaveAt(ctx, id.String(), who)
}

// legacyLeaveAt is an /event route's leave by any id the calendar answers
// with: an instance's takes its guest off the whole series.
func (s *service) legacyLeaveAt(ctx context.Context, ref string, who *model.UserInfo) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	inst, guests, err := s.resolve(ctx, who, ref)
	if err != nil {
		return err
	}
	return s.leave(ctx, who, inst.owner(), guests)
}

// legacyOfInstance is one occurrence in the /event routes' JSON, by the id
// the calendar knows it by, with its guests as they stand for it (occ, by
// that id: withAnswers).
func legacyOfInstance(inst instance, occ map[string][]models.Attendee, names map[string]*dgraphStruct.DgraphUser, reader uuid.UUID, at allDayAt) *dgraphStruct.DgraphEvent {
	ev := legacyOf(inst.Event, occ[inst.ref], names, reader, at)
	ev.Uuid = inst.ref
	return ev
}

// legacyRange is the events GetEventsList answers from the store, before it
// adds the reader's Google events and their bookings.
func (s *service) legacyRange(ctx context.Context, who *model.UserInfo, from, to time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, guests, _, err := s.rangeRows(ctx, personOf(who), from, to)
	if err != nil {
		return nil, err
	}
	names := s.namesOf(ctx, rows, guests)
	occ := s.withAnswers(ctx, rows, guests)
	out := make([]*dgraphStruct.DgraphEvent, 0, len(rows))
	for _, inst := range rows {
		out = append(out, legacyOfInstance(inst, occ, names, personOf(who), webDates))
	}
	s.withReminders(ctx, rows, occ, out, personOf(who))
	return out, nil
}

// legacyGet is one event GetEvent answers from the store, by any id the
// calendar answers with.
func (s *service) legacyGet(ctx context.Context, who *model.UserInfo, id string) (*dgraphStruct.DgraphEvent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	all := map[uuid.UUID][]models.Attendee{inst.owner().ID: guests}
	occ := s.withAnswers(ctx, []instance{inst}, all)
	out := []*dgraphStruct.DgraphEvent{legacyOfInstance(inst, occ, s.namesOf(ctx, []instance{inst}, all), personOf(who), webDates)}
	s.withReminders(ctx, []instance{inst}, occ, out, personOf(who))
	return out[0], nil
}

// busyEvents is what makes someone busy between from and to, in the graph's
// shape the busy engine and the status read: their events that show as busy
// and aren't cancelled, all-day ones as the midnights of their zone. Nobody
// is named: only times leave the calendar here.
func (s *service) busyEvents(ctx context.Context, userUUID string, from, to time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	person, err := uuid.Parse(userUUID)
	if err != nil {
		return nil, nil
	}
	rows, guests, _, err := s.rangeRows(ctx, person, from, to)
	if err != nil {
		return nil, err
	}
	occ := s.withAnswers(ctx, rows, guests)
	me := models.GuestKey{UserID: &person}
	out := make([]*dgraphStruct.DgraphEvent, 0, len(rows))
	for _, inst := range rows {
		if inst.Transparency == transparencyTransparent || inst.Status == statusCancelled {
			continue
		}
		// What someone said no to doesn't make them busy.
		if a := answerOf(occ[inst.ref], me); a != nil && a.Response == responseDeclined {
			continue
		}
		out = append(out, legacyOfInstance(inst, occ, nil, person, zoneDates))
	}
	return out, nil
}

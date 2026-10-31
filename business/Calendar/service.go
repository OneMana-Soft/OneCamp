package Calendar

// The Calendar service (PROGRAM.md CAL1): every read and write of an event in
// the one store, for the person asking. A write is validated, written with the
// event's sequence in one transaction, then announced once as a Resource Event
// (event.created, .updated, .deleted, .restored, .rsvp), and only then does
// what follows from it (Google's copy, a booking that follows its event,
// guests taken off told) as effects. Its rules are the /event routes' rules,
// so the routes (legacy.go) and the calendar's API (Events) answer alike:
//
//   - the organiser sees and changes their event; a guest sees it, and changes
//     it only when the organiser lets guests change it; anyone else is told it
//     isn't there, as if it never was (CAL5);
//   - an event in the trash is there for nobody but its organiser's trash.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	recurrence "github.com/akashc777/OneCamp/business/Recurrence"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// StaleError is a write that named a sequence the event has moved past:
// someone changed it since the writer read it. Current is the event as it is
// now, for the writer to look at again.
type StaleError struct{ Current *Event }

func (e *StaleError) Error() string {
	return "This event changed since you opened it. Look at it again, then make your change."
}

// service is the calendar over a store.
type service struct {
	store store
	now   func() time.Time
	newID func() uuid.UUID
	// people names people by uuid, those who have left included.
	people func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error)
	// members is which of these uuids are people who can be invited: active
	// workspace members.
	members func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error)
	emit    func(ctx context.Context, e resourceEventBusiness.Event)
	effects effects
	// tells tells people of changes (notify.go, imip.go).
	tells teller
	// ready is whether the store can be used now (ready.go).
	ready func(ctx context.Context) error
	// rem keeps each person's reminders and when the next fires (reminders.go).
	rem reminderStore
	// ledger is calendar sync's (moveFromGoogle.go); moveNow runs a move
	// before answering, as tests need, rather than in the background.
	ledger  syncLedger
	moveNow bool
}

func newService(s store) *service {
	svc := &service{
		store:   s,
		now:     func() time.Time { return time.Now().UTC() },
		newID:   uuid.New,
		people:  userDomain.GetPeopleByUUIDs,
		members: userDomain.GetActiveDgraphUsersByUUIDsLight,
		emit:    func(ctx context.Context, e resourceEventBusiness.Event) { resourceEventBusiness.Bus.Emit(ctx, e) },
		ready:   storeReady,
		rem:     postgresReminders{},
		ledger:  postgresLedger{},
	}
	svc.effects = liveEffects{svc}
	svc.tells = liveTeller{svc}
	return svc
}

// stored is the calendar in its one store.
var stored = newService(postgresStore{})

// personOf is who's id: the same in both of the user's records.
func personOf(who *model.UserInfo) uuid.UUID {
	if who == nil {
		return uuid.Nil
	}
	if who.UserPostgresInfo.Id != uuid.Nil {
		return who.UserPostgresInfo.Id
	}
	id, _ := uuid.Parse(who.UserDgraphInfo.Uuid)
	return id
}

// role is how someone is on an event.
type role int

const (
	roleNone role = iota
	roleGuest
	roleOrganiser
	// roleReader is the person whose subscribed calendar an event is on
	// (calendar B8): it is theirs to see whole, and changes only where the
	// calendar is published.
	roleReader
)

func roleOf(e *models.Event, guests []models.Attendee, person uuid.UUID) role {
	if person == uuid.Nil {
		return roleNone
	}
	if e.CreatedBy == person {
		if e.Source == sourceSubscribed {
			return roleReader
		}
		return roleOrganiser
	}
	for _, a := range guests {
		if a.UserID != nil && *a.UserID == person {
			return roleGuest
		}
	}
	return roleNone
}

// mayChange is whether someone may change an event: its organiser, or a guest
// when the organiser lets guests change it. The error says why not, in the
// words the /event routes used.
func mayChange(r role, e *models.Event) error {
	switch {
	case r == roleOrganiser, r == roleGuest && e.GuestsCanModify:
		return nil
	case r == roleGuest:
		return ErrOnlyCreatorChanges
	case r == roleReader:
		return ErrSubscribedEvent
	}
	return ErrEventNotFound
}

// mayEdit is mayChange for a change from cur to next with the guest list
// wanted (nil when the list stays). A guest the organiser lets invite others
// but not change the event (Google's "Invite others") may add guests to it,
// and change nothing else: no detail, nobody taken off, nobody's role.
func mayEdit(r role, cur, next *models.Event, curGuests []models.Attendee, wanted *[]GuestInput) error {
	if r == roleGuest && !cur.GuestsCanModify && cur.GuestsCanInvite && onlyInvites(cur, next, curGuests, wanted) {
		return nil
	}
	return mayChange(r, cur)
}

// onlyInvites is whether a change only adds guests: the list wanted names
// every guest already on the event, none in another role, and nothing else
// about the event differs. Pure.
func onlyInvites(cur, next *models.Event, curGuests []models.Attendee, wanted *[]GuestInput) bool {
	if wanted == nil || cur.RRule != next.RRule || len(changedFields(cur, next, false)) > 0 {
		return false
	}
	named := map[string]string{}
	for _, w := range *wanted {
		if k, ok := inputKey(w); ok {
			named[k.String()] = strings.TrimSpace(w.Role)
		}
	}
	for _, a := range curGuests {
		r, ok := named[a.Key().String()]
		if !ok || (r != "" && r != a.Role) {
			return false
		}
	}
	return true
}

// load is a live event and its guests; ErrEventNotFound for one in the trash
// or not there.
func (s *service) load(ctx context.Context, id uuid.UUID) (*models.Event, []models.Attendee, error) {
	e, err := s.store.get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if e == nil || e.DeletedAt != nil {
		return nil, nil, ErrEventNotFound
	}
	guests, err := s.store.attendees(ctx, []uuid.UUID{id})
	if err != nil {
		return nil, nil, err
	}
	return e, guests[id], nil
}

// window is the stretch a read of from to to covers, checked.
func window(from, to time.Time) (models.Window, error) {
	if to.Before(from) {
		return models.Window{}, inputError("The end of the range is before its start.")
	}
	if to.Sub(from) > maxRangeDays*24*time.Hour {
		return models.Window{}, inputError("Ask for 400 days or less at a time.")
	}
	from, to = from.UTC(), to.UTC()
	// A date is a different stretch of time in every zone: the earliest it
	// starts is 14 hours before its UTC midnight, the latest it ends 12 hours
	// after the next one. Any date that could touch the window is read.
	return models.Window{From: from, To: to,
		FromDate: from.Add(-12 * time.Hour).Format(dateLayout),
		ToDate:   to.Add(14 * time.Hour).Format(dateLayout)}, nil
}

// rangeRows is what person is on between from and to, each series' instances
// worked out: at most maxInstances, and whether there was more; with the
// guests of each event and series.
func (s *service) rangeRows(ctx context.Context, person uuid.UUID, from, to time.Time) ([]instance, map[uuid.UUID][]models.Attendee, bool, error) {
	w, err := window(from, to)
	if err != nil {
		return nil, nil, false, err
	}
	rows, err := s.store.rangeOf(ctx, person, w, maxInstances+1)
	if err != nil {
		return nil, nil, false, err
	}
	cut := len(rows) > maxInstances
	if cut {
		rows = rows[:maxInstances]
	}
	inst, err := s.instancesOf(ctx, rows, w, maxInstances+1)
	if err != nil {
		return nil, nil, false, err
	}
	if len(inst) > maxInstances {
		inst, cut = inst[:maxInstances], true
	}
	guests, err := s.store.attendees(ctx, idsOf(rows))
	if err != nil {
		return nil, nil, false, err
	}
	return inst, guests, cut, nil
}

func idsOf(rows []*models.Event) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, e := range rows {
		out = append(out, e.ID)
	}
	return out
}

// guestsShown is the guests someone sees of an event: all of them, unless the
// organiser hides the list from guests, who then see only themselves.
func guestsShown(e *models.Event, guests []models.Attendee, reader uuid.UUID) []models.Attendee {
	if e.GuestsCanSeeGuests || e.CreatedBy == reader {
		return guests
	}
	for _, a := range guests {
		if a.UserID != nil && *a.UserID == reader {
			return []models.Attendee{a}
		}
	}
	return nil
}

// namesOf names everyone on these events in one read. A failed read names
// nobody, and the events still answer, by id.
func (s *service) namesOf(ctx context.Context, rows []instance, guests map[uuid.UUID][]models.Attendee) map[string]*dgraphStruct.DgraphUser {
	seen := map[string]bool{}
	var ids []string
	add := func(id uuid.UUID) {
		if k := id.String(); id != uuid.Nil && !seen[k] {
			seen[k] = true
			ids = append(ids, k)
		}
	}
	for _, e := range rows {
		add(e.CreatedBy)
		for _, a := range guests[e.owner().ID] {
			if a.UserID != nil {
				add(*a.UserID)
			}
		}
	}
	out := map[string]*dgraphStruct.DgraphUser{}
	if len(ids) == 0 {
		return out
	}
	people, err := s.people(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: names of %d people couldn't be read: %v", len(ids), err)
	}
	for _, p := range people {
		if p != nil {
			out[p.Uuid] = p
		}
	}
	return out
}

func personNamed(id uuid.UUID, names map[string]*dgraphStruct.DgraphUser) Person {
	p := Person{UUID: id.String()}
	if u := names[p.UUID]; u != nil {
		p.Name, p.FullName = u.UserName, u.UserFullName
		if u.ProfileKey != nil {
			p.ProfileKey = *u.ProfileKey
		}
	}
	return p
}

// views are events as reader sees them: each occurrence's guests as they
// stand for it (an instance answered on its own says so), and what only the
// organiser is told (whether each invitation by email was sent, their "Email
// me when guests answer") to the organiser alone.
func (s *service) views(ctx context.Context, reader *model.UserInfo, rows []instance, guests map[uuid.UUID][]models.Attendee) []Event {
	names := s.namesOf(ctx, rows, guests)
	occ := s.withAnswers(ctx, rows, guests)
	me := personOf(reader)
	plain := make([]*models.Event, 0, len(rows))
	for _, i := range rows {
		plain = append(plain, i.Event)
	}
	booked := bookingsOf(ctx, reader, plain)
	tasks := s.tasksOf(ctx, subjectOf(reader), plain)
	out := make([]Event, 0, len(rows))
	for _, inst := range rows {
		e := inst.Event
		start, end := whenOf(e)
		var task *dgraphStruct.EventTask
		if e.TaskUUID != nil {
			task = tasks[*e.TaskUUID]
		}
		v := Event{
			ID: inst.ref, CalendarID: e.CalendarID.String(), ReadOnly: e.Source == sourceSubscribed, Kind: EventKind(e.Kind),
			Title: titleOf(e, task), Task: task, Notes: e.Notes, NotesFormat: e.NotesFormat, Location: e.Location, CallRoom: e.CallRoom,
			Start: start, End: end, AllDay: e.AllDay(),
			Transparency: e.Transparency, Visibility: e.Visibility, Hue: e.Hue, Status: e.Status,
			GuestsCanModify: e.GuestsCanModify, GuestsCanInvite: e.GuestsCanInvite, GuestsCanSeeGuests: e.GuestsCanSeeGuests,
			Organiser: personNamed(e.CreatedBy, names), Attendees: []Attendee{},
			Sequence: e.Sequence, ICalUID: e.ICalUID, GoogleEventID: e.GoogleEventID,
			Booking: booked[e.ID.String()], CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, DeletedAt: e.DeletedAt,
		}
		if series := inst.owner(); series.RRule != "" {
			sStart, loc, _ := seriesTime(series)
			text := ""
			if r, err := ruleOf(series); err == nil {
				text = recurrence.Rules.Describe(r, sStart, loc)
			}
			v.Recurrence = &Recurrence{Rule: series.RRule, Text: text}
		}
		if inst.rid != nil {
			// Its start in the series, said as the series says its starts.
			v.SeriesID = inst.owner().ID.String()
			if inst.owner().AllDay() {
				v.RecurrenceID = &When{Date: inst.rid.UTC().Format(dateLayout)}
			} else {
				rid := *inst.rid
				v.RecurrenceID = &When{At: &rid, Zone: inst.owner().TZ}
			}
		}
		organiser := inst.owner().CreatedBy == me
		if organiser {
			v.EmailOnAnswers = inst.owner().EmailOnAnswers
		}
		mayChangeIt := mayChange(roleOf(inst.owner(), occ[inst.ref], me), inst.owner()) == nil
		for _, a := range guestsShown(inst.owner(), occ[inst.ref], me) {
			at := Attendee{Role: a.Role, Response: a.Response, Note: a.Note, RespondedAt: a.RespondedAt,
				Proposal: proposalShown(a, inst.Event, me, mayChangeIt)}
			if a.UserID != nil {
				at.Person = personNamed(*a.UserID, names)
			} else {
				at.Person = Person{Name: a.Name}
				at.Email = a.Email
				if organiser {
					at.Invite = a.InviteState
				}
			}
			v.Attendees = append(v.Attendees, at)
		}
		out = append(out, v)
	}
	return out
}

// blank is a new event's defaults, in zone.
func blank(zone string) *models.Event {
	return &models.Event{Kind: string(EventPlain), NotesFormat: notesText, TZ: zoneOrUTC(zone),
		Transparency: transparencyOpaque, Visibility: visibilityDefault, Status: statusConfirmed,
		GuestsCanInvite: true, GuestsCanSeeGuests: true, Changed: []string{}, Exdates: []time.Time{}, Rdates: []time.Time{}}
}

// canonicalNotes is notes in their one form: HTML through the Content Codec,
// which keeps only what a doc may hold (no scripts, safe links); text as it is.
func canonicalNotes(notes, format string) (string, error) {
	if format != notesHTML || strings.TrimSpace(notes) == "" {
		return notes, nil
	}
	doc, err := contentcodec.FromHTML(notes, contentcodec.HTMLOptions{Foreign: true})
	if err != nil {
		return "", inputError("The notes couldn't be read as HTML.")
	}
	return contentcodec.ToHTML(doc), nil
}

// check is every rule a stored event keeps.
func check(e *models.Event) error {
	if err := checkText(e.Title, e.Notes, e.Location); err != nil {
		return err
	}
	if err := checkKindRules(e); err != nil {
		return err
	}
	// Focus time holds its organiser's notifications while it runs, and the
	// push gate reads instants (models/postgres/UserNotificationPreference
	// FocusStore's Running): whole days would hold nothing.
	if e.Kind == string(EventFocus) && e.AllDay() {
		return inputError("Focus time runs from a time to a time, not whole days.")
	}
	// A series repeats by a rule its kind allows (setRule read it); an
	// instance changed on its own never repeats.
	if e.RRule != "" {
		if !repeatingKinds[EventKind(e.Kind)] {
			return inputError("Focus time, time off, bookings and reminders don't repeat yet.")
		}
		if e.SeriesID != nil {
			return inputError("One instance of a repeating event doesn't repeat on its own.")
		}
	}
	return nil
}

// guestIDs are the people among guests, in order.
func guestIDs(guests []models.Attendee) []string {
	out := make([]string, 0, len(guests))
	for _, a := range guests {
		if a.UserID != nil {
			out = append(out, a.UserID.String())
		}
	}
	return out
}

// announce emits one Resource Event for a change to an event. Data names what
// changed, never its notes or title: events travel to webhooks and the audit.
func (s *service) announce(ctx context.Context, who *model.UserInfo, kind resourceEventBusiness.Kind, e *models.Event, subjects []string, data map[string]string) {
	if data == nil {
		data = map[string]string{}
	}
	data["kind"] = e.Kind
	s.emit(ctx, resourceEventBusiness.Event{
		Kind: kind, ResourceKind: KindEvent, ResourceUUID: e.ID.String(),
		Actor:    resourceEventBusiness.Actor{UserUUID: personOf(who).String()},
		At:       s.now(),
		Subjects: subjects,
		Data:     data,
	})
}

// create writes a new event for who, on their own calendar, with the guests
// that can be invited.
func (s *service) create(ctx context.Context, who *model.UserInfo, e *models.Event, wanted []GuestInput, syncToGoogle bool) (*models.Event, []models.Attendee, error) {
	if err := check(e); err != nil {
		return nil, nil, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return nil, nil, ErrEventNotFound
	}
	invited, err := s.invitable(ctx, me, wanted)
	if err != nil {
		return nil, nil, err
	}
	// On the calendar the writer chose (checked by Create), else their own.
	calendar := e.CalendarID
	if calendar == uuid.Nil {
		if calendar, err = s.store.personalCalendar(ctx, me, e.TZ); err != nil {
			return nil, nil, err
		}
	}
	now := s.now()
	e.ID, e.CalendarID, e.CreatedBy = s.newID(), calendar, me
	e.ICalUID, e.Source, e.Sequence = e.ID.String()+"@onecamp", sourceOneCamp, 0
	e.CreatedAt, e.UpdatedAt, e.DeletedAt, e.DeletedBy = now, now, nil, nil
	guests := make([]models.Attendee, 0, len(invited))
	for _, g := range invited {
		row, err := newGuest(e.ID, g, now)
		if err != nil {
			return nil, nil, err
		}
		guests = append(guests, row)
	}
	if err := s.store.create(ctx, e, guests); err != nil {
		return nil, nil, err
	}
	ids := guestIDs(guests)
	s.announce(ctx, who, resourceEventBusiness.CalendarEventCreated, e, ids, nil)
	s.effects.created(ctx, who, e, ids, syncToGoogle)
	if len(guests) > 0 {
		s.tells.tell(ctx, who, telling{what: toldMade, event: e, now: e, guestsAfter: guests})
	}
	return e, guests, nil
}

// changedFields names what differs between two versions of an event. Pure.
func changedFields(a, b *models.Event, guestsChanged bool) []string {
	var out []string
	add := func(name string, differs bool) {
		if differs {
			out = append(out, name)
		}
	}
	add("kind", a.Kind != b.Kind)
	add("title", a.Title != b.Title)
	add("notes", a.Notes != b.Notes || a.NotesFormat != b.NotesFormat)
	add("location", a.Location != b.Location)
	add("time", !sameTime(a.StartAt, b.StartAt) || !sameTime(a.EndAt, b.EndAt) || a.StartDate != b.StartDate ||
		a.EndDate != b.EndDate || a.TZ != b.TZ || a.EndTZ != b.EndTZ)
	add("show_as", a.Transparency != b.Transparency)
	add("visibility", a.Visibility != b.Visibility)
	add("colour", a.Hue != b.Hue)
	add("guest_permissions", a.GuestsCanModify != b.GuestsCanModify || a.GuestsCanInvite != b.GuestsCanInvite ||
		a.GuestsCanSeeGuests != b.GuestsCanSeeGuests)
	add("email_on_answers", a.EmailOnAnswers != b.EmailOnAnswers)
	add("guests", guestsChanged)
	return out
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// guestChange is what a write does to an event's guests: who comes on, who
// goes off (people by id, guests by email by address), who stays in another
// role, and the list after.
type guestChange struct {
	add          []models.Attendee
	remove       []uuid.UUID
	removeEmails []string
	roles        []models.Attendee
	after        []models.Attendee
}

// writes is the change as the store makes it.
func (g guestChange) writes() guestWrites {
	return guestWrites{add: g.add, remove: g.remove, removeEmails: g.removeEmails, roles: g.roles}
}

// changed is whether the change does anything to the guests.
func (g guestChange) changed() bool {
	return len(g.add)+len(g.remove)+len(g.removeEmails)+len(g.roles) > 0
}

// inputKey is who a guest named in a write is: their id, or their address
// cleaned; and whether it names anyone.
func inputKey(g GuestInput) (models.GuestKey, bool) {
	if id, err := uuid.Parse(strings.TrimSpace(g.UUID)); err == nil {
		return models.GuestKey{UserID: &id}, true
	}
	if strings.TrimSpace(g.Email) == "" {
		return models.GuestKey{}, false
	}
	email, _, err := cleanAddress(g.Email)
	if err != nil {
		return models.GuestKey{}, false
	}
	return models.GuestKey{Email: email}, true
}

// guestsFor is the change wanted (the whole new list; none when nil) makes to
// cur's guests. Guests kept stay as they are, answers and all, even if they
// have left the workspace since, but for the role the list names (required
// or optional; none keeps theirs); new ones must be invitable, and an
// address that is a guest's already (or a member's who is one) keeps them.
func (s *service) guestsFor(ctx context.Context, cur *models.Event, curGuests []models.Attendee, wanted *[]GuestInput, now time.Time) (guestChange, error) {
	g := guestChange{after: curGuests}
	if wanted == nil {
		return g, nil
	}
	keep := map[string]bool{}
	roles := map[string]string{}
	var fresh []GuestInput
	for _, w := range *wanted {
		if strings.TrimSpace(w.UUID) == "" && strings.TrimSpace(w.Email) != "" {
			if _, _, err := cleanAddress(w.Email); err != nil {
				return g, err
			}
		}
		k, ok := guestKeyOf(w, cur.CreatedBy)
		if !ok {
			continue
		}
		if a := answerOf(curGuests, k); a != nil {
			keep[k.String()] = true
			if r := strings.TrimSpace(w.Role); (r == roleRequired || r == roleOptional) && r != a.Role {
				roles[k.String()] = r
			}
			continue
		}
		fresh = append(fresh, w)
	}
	if len(keep)+len(fresh) > maxGuests {
		return g, inputError("An event can have up to 500 guests.")
	}
	invited, err := s.invitable(ctx, cur.CreatedBy, fresh)
	if err != nil {
		return g, err
	}
	var add []GuestInput
	for _, in := range invited {
		k, _ := inputKey(in)
		if answerOf(curGuests, k) != nil {
			keep[k.String()] = true
			continue
		}
		add = append(add, in)
	}
	g.after = nil
	for _, a := range curGuests {
		if keep[a.Key().String()] {
			if r, ok := roles[a.Key().String()]; ok {
				a.Role = r
				g.roles = append(g.roles, a)
			}
			g.after = append(g.after, a)
			continue
		}
		if a.UserID != nil {
			g.remove = append(g.remove, *a.UserID)
		} else {
			g.removeEmails = append(g.removeEmails, strings.ToLower(a.Email))
		}
	}
	for _, in := range add {
		row, err := newGuest(cur.ID, in, now)
		if err != nil {
			return g, err
		}
		g.add = append(g.add, row)
		g.after = append(g.after, row)
	}
	return g, nil
}

// sameGuests is whether wanted names the people on an event already: who an
// instance of a repeating event may be sent with, as it can't change them.
func sameGuests(wanted []GuestInput, e *models.Event, guests []models.Attendee) bool {
	want := map[uuid.UUID]bool{}
	for _, w := range wanted {
		if id, err := uuid.Parse(strings.TrimSpace(w.UUID)); err == nil && id != e.CreatedBy {
			want[id] = true
		}
	}
	have := map[uuid.UUID]bool{}
	for _, a := range guests {
		if a.UserID != nil {
			have[*a.UserID] = true
		}
	}
	if len(want) != len(have) {
		return false
	}
	for id := range want {
		if !have[id] {
			return false
		}
	}
	return true
}

// update writes next over cur for who, with the guest list when wanted is
// not nil (the whole new list), when cur is still at expect (any sequence when
// nil).
func (s *service) update(ctx context.Context, who *model.UserInfo, cur, next *models.Event, curGuests []models.Attendee, wanted *[]GuestInput, expect *int) (*models.Event, []models.Attendee, error) {
	if err := mayEdit(roleOf(cur, curGuests, personOf(who)), cur, next, curGuests, wanted); err != nil {
		return nil, nil, err
	}
	if err := check(next); err != nil {
		return nil, nil, err
	}
	now := s.now()
	g, err := s.guestsFor(ctx, cur, curGuests, wanted, now)
	if err != nil {
		return nil, nil, err
	}
	next.ID, next.UpdatedAt = cur.ID, now
	seq, err := s.store.update(ctx, next, expect, g.writes())
	switch {
	case errors.Is(err, models.ErrStale):
		return nil, nil, s.stale(ctx, who, cur.ID)
	case errors.Is(err, models.ErrNotFound):
		return nil, nil, ErrEventNotFound
	case err != nil:
		return nil, nil, err
	}
	next.Sequence = seq
	removed := helpers.UUIDStrings(g.remove)
	changed := changedFields(cur, next, g.changed())
	if cur.RRule != next.RRule {
		changed = append(changed, "recurrence")
	}
	subjects := append(guestIDs(g.after), removed...)
	s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, next, subjects, map[string]string{"changed": strings.Join(changed, ",")})
	s.effects.updated(ctx, who, cur, next, curGuests, g.after, wanted != nil, removed)
	s.tells.tell(ctx, who, telling{what: toldChanged, event: next, was: cur, now: next, guestsBefore: curGuests, guestsAfter: g.after, changed: changed})
	return next, g.after, nil
}

// stale is the StaleError for an event someone changed meanwhile.
func (s *service) stale(ctx context.Context, who *model.UserInfo, id uuid.UUID) error {
	e, guests, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	return &StaleError{Current: s.view(ctx, who, single(e), guests)}
}

// staleAt is the StaleError for an occurrence someone changed meanwhile, by
// the id the reader knows it by.
func (s *service) staleAt(ctx context.Context, who *model.UserInfo, ref string) error {
	inst, guests, err := s.resolve(ctx, who, ref)
	if err != nil {
		return err
	}
	return &StaleError{Current: s.view(ctx, who, inst, guests)}
}

// atInstance says a failed write to an instance's series as the instance:
// what is current is the instance the reader changed, when it still is one.
func (s *service) atInstance(ctx context.Context, who *model.UserInfo, ref string, err error) error {
	var stale *StaleError
	if !errors.As(err, &stale) {
		return err
	}
	if again := s.staleAt(ctx, who, ref); errors.As(again, &stale) {
		return again
	}
	return err
}

// seriesExpect is the sequence a change to all or following instances checks
// the series against. The reader saw the instance: made from its series, its
// sequence is the series'; an override has its own, checked here, and the
// series is then checked as just read.
func (s *service) seriesExpect(ctx context.Context, who *model.UserInfo, inst instance, expect *int) (*int, error) {
	if !inst.override || expect == nil {
		return expect, nil
	}
	if *expect != inst.Sequence {
		return nil, s.staleAt(ctx, who, inst.ref)
	}
	read := inst.series.Sequence
	return &read, nil
}

// remove puts an event in the trash, for its organiser.
func (s *service) remove(ctx context.Context, who *model.UserInfo, cur *models.Event, guests []models.Attendee, expect *int) error {
	switch roleOf(cur, guests, personOf(who)) {
	case roleOrganiser:
	case roleGuest:
		return ErrNotEventCreator
	case roleReader:
		return ErrSubscribedEvent
	default:
		return ErrEventNotFound
	}
	now, me := s.now(), personOf(who)
	seq, err := s.store.setDeleted(ctx, cur.ID, &now, &me, now, expect)
	switch {
	case errors.Is(err, models.ErrStale):
		return s.stale(ctx, who, cur.ID)
	case errors.Is(err, models.ErrNotFound):
		return ErrEventNotFound
	case err != nil:
		return err
	}
	cur.Sequence, cur.DeletedAt, cur.DeletedBy = seq, &now, &me
	// Its call ends with it (calendar B11): a restored event is given a new one.
	s.endCall(ctx, cur.CallRoom)
	s.announce(ctx, who, resourceEventBusiness.CalendarEventDeleted, cur, guestIDs(guests), nil)
	s.effects.deleted(ctx, who, cur, guests)
	if len(guests) > 0 {
		s.tells.tell(ctx, who, telling{what: toldCancelled, event: cur, now: cur, guestsBefore: guests, guestsAfter: guests})
	}
	return nil
}

// leave takes who off an event they are a guest of.
func (s *service) leave(ctx context.Context, who *model.UserInfo, cur *models.Event, guests []models.Attendee) error {
	me := personOf(who)
	switch roleOf(cur, guests, me) {
	case roleGuest:
	case roleOrganiser:
		return ErrCreatorCannotLeave
	default:
		return ErrEventNotFound
	}
	removed, err := s.store.removeGuest(ctx, cur.ID, me, s.now())
	if err != nil {
		return err
	}
	if !removed {
		return ErrEventNotFound
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventRSVP, cur, []string{me.String()}, map[string]string{"response": string(Declined)})
	s.effects.left(ctx, who, cur)
	return nil
}

// --- The calendar's API (Events) ---------------------------------------

func (s *service) Get(ctx context.Context, who *model.UserInfo, id string) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, who, inst, guests), nil
}

// view is one occurrence as reader sees it.
func (s *service) view(ctx context.Context, reader *model.UserInfo, inst instance, guests []models.Attendee) *Event {
	v := s.views(ctx, reader, []instance{inst}, map[uuid.UUID][]models.Attendee{inst.owner().ID: guests})
	out := &v[0]
	s.markAwayDeclines(ctx, inst, out)
	// An out-of-office's decline rule, to its owner alone (head 5b).
	if owner := inst.owner(); EventKind(owner.Kind) == EventAway && owner.CreatedBy == personOf(reader) {
		if rules, err := s.store.oooRules(ctx, []uuid.UUID{owner.ID}); err == nil {
			if r, ok := rules[owner.ID]; ok {
				out.OutOfOffice = &OOOView{Decline: r.Decline, Message: r.Message}
			}
		}
	}
	return out
}

func (s *service) Range(ctx context.Context, who *model.UserInfo, from, to time.Time) ([]Event, bool, error) {
	if err := s.ready(ctx); err != nil {
		return nil, false, err
	}
	rows, guests, cut, err := s.rangeRows(ctx, personOf(who), from, to)
	if err != nil {
		return nil, false, err
	}
	return s.views(ctx, who, rows, guests), cut, nil
}

func (s *service) Create(ctx context.Context, who *model.UserInfo, in EventInput) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	e := blank(s.zoneOf(ctx, personOf(who)))
	if in.Kind != "" {
		e.Kind = string(in.Kind)
	}
	e.Title, e.Location = strings.TrimSpace(in.Title), strings.TrimSpace(in.Location)
	// A task block is time for a task the writer may read: theirs alone (no
	// guests, keepKind). Given no title, or only its task's name, it keeps
	// none, and is called by its task where it is read (titleOf), following a
	// rename.
	if in.Kind == EventTaskBlock {
		task, name, err := s.blockedTask(ctx, who, in.TaskUUID)
		if err != nil {
			return nil, err
		}
		e.TaskUUID = &task
		if e.Title == name {
			e.Title = ""
		}
	} else if strings.TrimSpace(in.TaskUUID) != "" {
		return nil, inputError("Only a task block is time for a task.")
	}
	if in.NotesFormat != "" {
		e.NotesFormat = in.NotesFormat
	}
	notes, err := canonicalNotes(in.Notes, e.NotesFormat)
	if err != nil {
		return nil, err
	}
	e.Notes = notes
	if in.Transparency != "" {
		e.Transparency = in.Transparency
	}
	if err := keepKind(e, "", personOf(who), nil, &in.Attendees); err != nil {
		return nil, err
	}
	if in.Visibility != "" {
		e.Visibility = in.Visibility
	}
	e.Hue = in.Hue
	setFlag(&e.GuestsCanModify, in.GuestsCanModify)
	setFlag(&e.GuestsCanInvite, in.GuestsCanInvite)
	setFlag(&e.GuestsCanSeeGuests, in.GuestsCanSeeGuests)
	setFlag(&e.EmailOnAnswers, in.EmailOnAnswers)
	if err := setWhen(e, in.Start, in.End); err != nil {
		return nil, err
	}
	if in.CalendarID != "" {
		cal, err := s.writableCalendar(ctx, who, in.CalendarID)
		if err != nil {
			return nil, err
		}
		e.CalendarID = cal
	}
	if in.Recurrence != nil && strings.TrimSpace(in.Recurrence.Rule) != "" {
		if in.SyncToGoogle {
			// Google's copy of a series is B6's: a copy of only its first
			// instance would be wrong, so it isn't made.
			return nil, inputError("Repeating events can't be copied to Google Calendar yet.")
		}
		if err := setRule(e, in.Recurrence.Rule); err != nil {
			return nil, err
		}
	}
	var rule *models.OOORule
	if in.OutOfOffice != nil {
		r, err := oooRuleOf(in.OutOfOffice, EventKind(e.Kind))
		if err != nil {
			return nil, err
		}
		rule = &r
	}
	created, guests, err := s.create(withoutSending(ctx, in.Notify), who, e, in.Attendees, in.SyncToGoogle)
	if err != nil {
		return nil, err
	}
	// The event is written: a rule that can't be kept is logged, not a
	// failure of the write (CR-68 SF).
	if rule != nil {
		if err := s.keepRule(ctx, created, *rule); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar.Create: out-of-office rule for %s not kept: %v", created.ID, err)
		}
	}
	// Guests out of office with a rule decline it for themselves (head 5b).
	s.declineForAway(ctx, created, guests)
	return s.view(ctx, who, single(created), guests), nil
}

func setFlag(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// patched is base with what p sends set on it (CAL2): a field left out stays,
// a field sent is set, "" clears.
func patched(base *models.Event, p EventPatch) (*models.Event, error) {
	next := cloneEvent(base)
	if p.Kind != nil {
		next.Kind = string(*p.Kind)
		// A block made another kind is no longer time for its task.
		if *p.Kind != EventTaskBlock {
			next.TaskUUID = nil
		}
	}
	if p.Title != nil {
		next.Title = strings.TrimSpace(*p.Title)
	}
	if p.NotesFormat != nil {
		next.NotesFormat = *p.NotesFormat
	}
	if p.Notes != nil || p.NotesFormat != nil {
		notes := next.Notes
		if p.Notes != nil {
			notes = *p.Notes
		}
		var err error
		if next.Notes, err = canonicalNotes(notes, next.NotesFormat); err != nil {
			return nil, err
		}
	}
	if p.Location != nil {
		next.Location = strings.TrimSpace(*p.Location)
	}
	if p.Transparency != nil {
		next.Transparency = *p.Transparency
	}
	if p.Visibility != nil {
		next.Visibility = *p.Visibility
	}
	if p.Hue != nil {
		next.Hue = *p.Hue
	}
	setFlag(&next.GuestsCanModify, p.GuestsCanModify)
	setFlag(&next.GuestsCanInvite, p.GuestsCanInvite)
	setFlag(&next.GuestsCanSeeGuests, p.GuestsCanSeeGuests)
	setFlag(&next.EmailOnAnswers, p.EmailOnAnswers)
	if p.Start != nil || p.End != nil {
		start, end := whenOf(base)
		if p.Start != nil {
			start = *p.Start
		}
		if p.End != nil {
			end = *p.End
		}
		if err := setWhen(next, start, end); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func (s *service) Update(ctx context.Context, who *model.UserInfo, id string, p EventPatch) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if p.Sequence == nil {
		return nil, inputError("Say which version of the event you changed (its sequence).")
	}
	ctx = withoutSending(ctx, p.Notify)
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	next, err := patched(inst.Event, p)
	if err != nil {
		return nil, err
	}
	next.Title = s.keptTitle(ctx, who, inst.Event, next)
	if err := keepKind(next, inst.Kind, inst.owner().CreatedBy, guests, p.Attendees); err != nil {
		return nil, err
	}
	// Changing what guests may do, and how the organiser hears of answers,
	// is the organiser's alone.
	if personOf(who) != inst.owner().CreatedBy && (permissionsDiffer(next, inst.Event) || next.EmailOnAnswers != inst.Event.EmailOnAnswers) {
		return nil, ErrOnlyCreatorChanges
	}
	if inst.rid == nil {
		return s.updateRow(ctx, who, inst.Event, next, guests, p)
	}
	return s.updateInstance(ctx, who, inst, guests, next, p)
}

// updateRow is a change to a single event, or to a whole series by its own
// id; a rule makes an event a series, and "" makes a series an event again.
func (s *service) updateRow(ctx context.Context, who *model.UserInfo, cur, next *models.Event, guests []models.Attendee, p EventPatch) (*Event, error) {
	if p.Recurrence != nil {
		if strings.TrimSpace(p.Recurrence.Rule) != "" && cur.RRule == "" && cur.GoogleEventID != "" {
			return nil, inputError("This event has a copy in Google Calendar, so it can't repeat yet.")
		}
		if err := setRule(next, p.Recurrence.Rule); err != nil {
			return nil, err
		}
	} else if next.RRule != "" {
		// The start may have moved: the rule's days and end move with it.
		oldStart, oldLoc, _ := seriesTime(cur)
		newStart, newLoc, _ := seriesTime(next)
		if err := moveRule(next, oldStart, oldLoc, newStart, newLoc); err != nil {
			return nil, err
		}
		if err := setRule(next, next.RRule); err != nil {
			return nil, err
		}
	}
	var rule *models.OOORule
	if p.OutOfOffice != nil {
		r, err := oooRuleOf(p.OutOfOffice, EventKind(next.Kind))
		if err != nil {
			return nil, err
		}
		rule = &r
	}
	var updated *models.Event
	var after []models.Attendee
	var err error
	if cur.RRule == "" {
		updated, after, err = s.update(ctx, who, cur, next, guests, p.Attendees, p.Sequence)
	} else {
		updated, after, err = s.updateSeries(ctx, who, cur, next, guests, p.Attendees, p.Sequence, nil)
	}
	if err != nil {
		return nil, err
	}
	if rule != nil {
		if err := s.keepRule(ctx, updated, *rule); err != nil {
			return nil, err
		}
	}
	s.declineForAway(ctx, updated, newlyOn(guests, after))
	return s.view(ctx, who, single(updated), after), nil
}

// updateInstance is a change to one instance of a series, for the scope it
// says: only what it changes of the instance goes further.
func (s *service) updateInstance(ctx context.Context, who *model.UserInfo, inst instance, guests []models.Attendee, next *models.Event, p EventPatch) (*Event, error) {
	series := inst.series
	switch p.Scope {
	case ScopeThis:
		if p.Recurrence != nil && !sameRule(p.Recurrence.Rule, series.RRule) {
			return nil, inputError("Change how it repeats for this and following events, or for all of them.")
		}
		if p.Attendees != nil && !sameGuests(*p.Attendees, series, guests) {
			return nil, inputError("Change who comes for this and following events, or for all of them.")
		}
		if _, err := s.updateThis(ctx, who, inst, guests, next, p.Sequence); err != nil {
			return nil, err
		}
		got, g, err := s.resolve(ctx, who, inst.ref)
		if err != nil {
			return nil, err
		}
		return s.view(ctx, who, got, g), nil
	case ScopeFollowing, ScopeAll:
	default:
		return nil, inputError("Say whether this is for this event, this and following, or all events.")
	}
	expect, err := s.seriesExpect(ctx, who, inst, p.Sequence)
	if err != nil {
		return nil, err
	}
	changes := changesOf(inst.Event, next)
	edit := &seriesEdit{rid: *inst.rid, release: changes}
	sStart, sLoc, _ := seriesTime(series)
	nStart, nEnd := spanOf(next)
	nLoc := zoneOfRow(next)
	moved := slices.Contains(changes, "time")
	// This and following from the first instance is all of it.
	if p.Scope == ScopeAll || inst.rid.Equal(sStart) {
		whole := cloneEvent(series)
		takeChanges(whole, next, slices.DeleteFunc(slices.Clone(changes), func(n string) bool { return n == "time" }))
		whole.Kind = next.Kind
		whole.GuestsCanModify, whole.GuestsCanInvite, whole.GuestsCanSeeGuests = next.GuestsCanModify, next.GuestsCanInvite, next.GuestsCanSeeGuests
		if moved {
			// Every instance moves as this one did: by as many days, to its
			// new time of day.
			start := rekey(sStart, *inst.rid, sLoc, nStart, nLoc)
			placeAt(whole, start, nEnd.Sub(nStart), next.AllDay())
			whole.TZ, whole.EndTZ = next.TZ, next.EndTZ
		}
		if p.Recurrence != nil && !sameRule(p.Recurrence.Rule, series.RRule) {
			if err := setRule(whole, p.Recurrence.Rule); err != nil {
				return nil, err
			}
		} else if moved {
			if err := moveRule(whole, *inst.rid, sLoc, nStart, nLoc); err != nil {
				return nil, err
			}
			if err := setRule(whole, whole.RRule); err != nil {
				return nil, err
			}
		}
		updated, after, err := s.updateSeries(ctx, who, series, whole, guests, p.Attendees, expect, edit)
		if err != nil {
			return nil, s.atInstance(ctx, who, inst.ref, err)
		}
		return s.view(ctx, who, single(updated), after), nil
	}
	// This and following: a new series from the instance, as the series says
	// it there with this change.
	base := madeInstance(series, *inst.rid).Event
	takeChanges(base, next, changes)
	base.Kind = next.Kind
	base.GuestsCanModify, base.GuestsCanInvite, base.GuestsCanSeeGuests = next.GuestsCanModify, next.GuestsCanInvite, next.GuestsCanSeeGuests
	var rule *string
	if p.Recurrence != nil && !sameRule(p.Recurrence.Rule, series.RRule) {
		r := p.Recurrence.Rule
		rule = &r
	}
	fresh, err := s.splitAt(ctx, who, inst, guests, base, rule, p.Attendees, expect, edit)
	if err != nil {
		return nil, s.atInstance(ctx, who, inst.ref, err)
	}
	got, g, err := s.load(ctx, fresh.ID)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, who, single(got), g), nil
}

// Delete puts an event or a series in the trash, or, for one instance of a
// series, this event, this and following, or all events (scope).
func (s *service) Delete(ctx context.Context, who *model.UserInfo, id, scope string, sequence *int) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return err
	}
	if inst.rid == nil {
		return s.remove(ctx, who, inst.Event, guests, sequence)
	}
	switch scope {
	case ScopeThis:
		return s.deleteThis(ctx, who, inst, guests, sequence)
	case ScopeFollowing, ScopeAll:
	default:
		return inputError("Say whether to delete this event, this and following, or all events.")
	}
	expect, err := s.seriesExpect(ctx, who, inst, sequence)
	if err != nil {
		return err
	}
	if scope == ScopeAll {
		err = s.remove(ctx, who, inst.series, guests, expect)
	} else {
		err = s.deleteFollowing(ctx, who, inst, guests, expect)
	}
	return s.atInstance(ctx, who, inst.ref, err)
}

// Restore brings an event, a series or a deleted instance back from its
// organiser's trash.
func (s *service) Restore(ctx context.Context, who *model.UserInfo, id string) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	eventID, rid, err := parseRef(id)
	if err != nil {
		return nil, err
	}
	e, err := s.store.get(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if e == nil || e.CreatedBy != personOf(who) {
		return nil, ErrEventNotFound
	}
	if rid != nil {
		if e.DeletedAt != nil {
			return nil, inputError("Bring back the repeating event it belongs to first.")
		}
		guests, err := s.store.attendees(ctx, []uuid.UUID{e.ID})
		if err != nil {
			return nil, err
		}
		back, err := s.restoreThis(ctx, who, e, guests[e.ID], *rid)
		if err != nil {
			return nil, err
		}
		if back.SeriesID == nil {
			// Its series no longer has it: an event of its own now.
			restored, g, err := s.load(ctx, back.ID)
			if err != nil {
				return nil, err
			}
			s.announce(ctx, who, resourceEventBusiness.CalendarEventRestored, restored, guestIDs(g), map[string]string{"from": e.ID.String()})
			return s.view(ctx, who, single(restored), g), nil
		}
		s.announce(ctx, who, resourceEventBusiness.CalendarEventRestored, e, guestIDs(guests[e.ID]), map[string]string{"instance": id})
		got, g, err := s.resolve(ctx, who, id)
		if err != nil {
			return nil, err
		}
		return s.view(ctx, who, got, g), nil
	}
	if e.DeletedAt == nil || e.SeriesID != nil {
		return nil, ErrEventNotFound
	}
	if _, err := s.store.setDeleted(ctx, e.ID, nil, nil, s.now(), nil); err != nil {
		return nil, err
	}
	restored, guests, err := s.load(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	s.announce(ctx, who, resourceEventBusiness.CalendarEventRestored, restored, guestIDs(guests), nil)
	return s.view(ctx, who, single(restored), guests), nil
}

// trashPage is how many events one look at the trash gives.
const trashPage = 50

// Trash is who's deleted events, newest first, deleted before before (nil:
// from the newest), a deleted instance of a series by the instance's id.
// Nothing is purged from it (the owner's question O6).
func (s *service) Trash(ctx context.Context, who *model.UserInfo, before *time.Time) ([]Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.store.trashOf(ctx, personOf(who), before, trashPage)
	if err != nil {
		return nil, err
	}
	var seriesIDs []uuid.UUID
	for _, e := range rows {
		if e.SeriesID != nil && !slices.Contains(seriesIDs, *e.SeriesID) {
			seriesIDs = append(seriesIDs, *e.SeriesID)
		}
	}
	series := map[uuid.UUID]*models.Event{}
	if len(seriesIDs) > 0 {
		found, err := s.store.getMany(ctx, seriesIDs)
		if err != nil {
			return nil, err
		}
		for _, e := range found {
			series[e.ID] = e
		}
	}
	guests, err := s.store.attendees(ctx, append(idsOf(rows), seriesIDs...))
	if err != nil {
		return nil, err
	}
	inst := make([]instance, 0, len(rows))
	for _, e := range rows {
		if e.SeriesID == nil {
			inst = append(inst, single(e))
			continue
		}
		owner := series[*e.SeriesID]
		if owner == nil {
			continue
		}
		inst = append(inst, instance{Event: e, ref: instanceRef(owner.ID, *e.RecurrenceID, owner.AllDay()), series: owner, rid: e.RecurrenceID, override: true})
	}
	return s.views(ctx, who, inst, guests), nil
}

// Respond is a guest's answer to an event they are on, for all of it, with no
// note (Answer says more). Saying no keeps them on the guest list.
func (s *service) Respond(ctx context.Context, who *model.UserInfo, id string, answer Answer) error {
	_, err := s.Answer(ctx, who, id, AnswerInput{Answer: answer})
	return err
}

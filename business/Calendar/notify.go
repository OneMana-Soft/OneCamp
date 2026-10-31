package Calendar

// Telling people about a change to an event (CAL6): each change tells the
// people it changes something for, once, live and through their own
// notification rules, and nothing that changes a meeting for someone is
// silent.
//
//   - Members hear in their inbox, and by push and email as their rules let
//     them (business/Notification Tell, under business/Notify's calendar
//     causes): invited; changed, saying what ("Now Thu 16 Oct, 11:00–12:00 ·
//     was 10:00–11:00"); cancelled; and taken off (event.uninvited,
//     CAL-BUG-1), which never names the event.
//   - The organiser hears of each answer in their inbox only (D-CAL-17), and
//     by email too when they asked for it ("Email me when guests answer").
//   - Guests invited by email get iMIP invitations instead (imip.go).
//   - "Tell guests? Don't send" (D-CAL-5) keeps every notice in the inbox:
//     no push, and no email to anyone.
//   - What only its organiser sees (its colour, show as, who may change it)
//     tells nobody, and a bulk write (the demo's seeding) tells nobody at all.
//
// At chat A4's wiring this file is the calendar's one swap: business/Notify's
// Plan takes these audiences, and its Deliver replaces Tell's push and email.

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The calendar's notices, by business/Notify's causes, as the inbox words
// them (the web's lib/activity/notice.ts).
var (
	noticeInvited   = string(notifyBusiness.CauseCalendarInvited)
	noticeUpdated   = string(notifyBusiness.CauseCalendarUpdated)
	noticeCancelled = string(notifyBusiness.CauseCalendarCancelled)
	noticeAnswered  = string(notifyBusiness.CauseCalendarRSVP)
	noticeReminder  = string(notifyBusiness.CauseCalendarReminder)
)

// tellWhat is the kind of change people hear of.
type tellWhat int

const (
	toldMade      tellWhat = iota + 1 // a new event with guests
	toldChanged                       // a change to an event, a series, or one instance of it
	toldCancelled                     // an event or series to the trash, or one instance cancelled
	toldAnswered                      // a guest answered
	toldNudged                        // the organiser asked again for answers
)

// answered is a guest's answer, for the organiser's notice, with the time it
// proposes anew (proposals.go).
type answered struct {
	key      models.GuestKey
	response string
	note     string
	proposal *models.Proposal
}

// telling is one change people may hear of.
type telling struct {
	what tellWhat
	// event is the event or series changed, as it is now: what notices
	// name, and whose guests and organiser they go to.
	event *models.Event
	// was and now are what a notice is about, before and after the change:
	// the event or series, or for "this event" alone that instance (was is
	// nil for something new).
	was, now *models.Event
	// ref is the id now is known by: the event's, or an instance's.
	ref string
	// guestsBefore and guestsAfter are the event's guests.
	guestsBefore, guestsAfter []models.Attendee
	// changed names what changed (changedFields).
	changed []string
	answer  *answered
	// nudged are the guests asked again for an answer.
	nudged []models.Attendee
	// outsidersOnly tells guests by email alone, and splitTail is the new
	// series "this and following" starts: its guests by email taken off it
	// keep the earlier part, which they are sent ending (tellSplit), so
	// nothing is cancelled for them here.
	outsidersOnly bool
	splitTail     bool
}

// about is the occurrence a telling is about, and the id it is known by:
// t.now, or the event itself when nothing narrower was named.
func (t telling) about() (string, *models.Event) {
	now, ref := t.now, t.ref
	if now == nil {
		now = t.event
	}
	if ref == "" {
		ref = t.event.ID.String()
	}
	return ref, now
}

// teller tells people of changes; a test records them instead.
type teller interface {
	tell(ctx context.Context, who *model.UserInfo, t telling)
}

type liveTeller struct{ s *service }

// tell tells, off the request and bounded, as booking notices are told: a
// push service or a mail server that doesn't answer holds nothing for long.
// who is nil for someone outside the workspace answering from their link.
func (l liveTeller) tell(ctx context.Context, who *model.UserInfo, t telling) {
	if helpers.IsBulkImport(ctx) || (who == nil && t.what != toldAnswered) {
		return
	}
	send := sending(ctx)
	var user *model.UserInfo
	if who != nil {
		u := *who
		user = &u
	}
	helpers.GoSafeNamed("business/Calendar/tell", func() {
		ctx, cancel := outbound(ctx)
		defer cancel()
		if !t.outsidersOnly {
			l.s.tellMembers(ctx, user, t, send)
		}
		if user != nil {
			l.s.tellOutsiders(ctx, t, send)
		}
	})
}

// dontSend is the context key of "Tell guests? Don't send".
type dontSend struct{}

// withoutSending is ctx for a write whose writer chose Don't send (D-CAL-5):
// guests are told in their inbox alone.
func withoutSending(ctx context.Context, notify string) context.Context {
	if strings.EqualFold(strings.TrimSpace(notify), notifyNone) {
		return context.WithValue(ctx, dontSend{}, true)
	}
	return ctx
}

// sending is whether guests are emailed and pushed about this write.
func sending(ctx context.Context) bool {
	held, _ := ctx.Value(dontSend{}).(bool)
	return !held
}

// toldAbout are the changes that change a meeting for its guests: what it
// is called, when and where it is (its call too: Add and Remove call), how it
// repeats, its notes. Pure.
func toldAbout(changed []string) bool {
	for _, c := range changed {
		switch c {
		case "title", "time", "location", "recurrence", "notes", "call":
			return true
		}
	}
	return false
}

// membersOf are the people among guests.
func membersOf(guests []models.Attendee) []uuid.UUID {
	var out []uuid.UUID
	for _, a := range guests {
		if a.UserID != nil {
			out = append(out, *a.UserID)
		}
	}
	return out
}

// partition is who of before and after came on, stayed and went. Pure.
func partition(before, after []uuid.UUID) (added, kept, removed []uuid.UUID) {
	for _, id := range after {
		if slices.Contains(before, id) {
			kept = append(kept, id)
		} else {
			added = append(added, id)
		}
	}
	for _, id := range before {
		if !slices.Contains(after, id) {
			removed = append(removed, id)
		}
	}
	return
}

// tellMembers tells the members a change is for, by notice.
func (s *service) tellMembers(ctx context.Context, actor *model.UserInfo, t telling, send bool) {
	ref, ev := t.about()
	inbox := !send
	if actor == nil {
		if t.what == toldAnswered {
			s.answerNotice(ctx, nil, ref, ev, t)
		}
		return
	}
	switch t.what {
	case toldMade:
		s.notice(ctx, actor, noticeInvited, ref, ev, nil, membersOf(t.guestsAfter), inbox, t)
	case toldChanged:
		added, kept, removed := partition(membersOf(t.guestsBefore), membersOf(t.guestsAfter))
		s.notice(ctx, actor, noticeInvited, ref, ev, nil, added, inbox, t)
		if toldAbout(t.changed) {
			s.notice(ctx, actor, noticeUpdated, ref, ev, t.was, kept, inbox, t)
		}
		gone := t.was
		if gone == nil {
			gone = t.event
		}
		s.uninvite(ctx, actor, gone, removed, inbox)
	case toldCancelled:
		s.notice(ctx, actor, noticeCancelled, ref, ev, nil, membersOf(t.guestsAfter), inbox, t)
	case toldNudged:
		s.notice(ctx, actor, noticeInvited, ref, ev, nil, membersOf(t.nudged), false, t)
	case toldAnswered:
		s.answerNotice(ctx, actor, ref, ev, t)
	}
}

// notice tells people of one kind of change to ev (was: ev before it), each
// in their own zone's words for push and email. inbox keeps it to the inbox.
func (s *service) notice(ctx context.Context, actor *model.UserInfo, kind, ref string, ev, was *models.Event, people []uuid.UUID, inbox bool, t telling) {
	me := personOf(actor)
	people = slices.DeleteFunc(slices.Clone(people), func(id uuid.UUID) bool { return id == me })
	if len(people) == 0 {
		return
	}
	zones, err := s.zonesOf(ctx, people)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: zones for a notice couldn't be read, so UTC: %v", err)
		zones = map[uuid.UUID]string{}
	}
	byZone := map[string][]uuid.UUID{}
	for _, id := range people {
		byZone[zoneOrUTC(zones[id])] = append(byZone[zoneOrUTC(zones[id])], id)
	}
	name := actor.UserDgraphInfo.DisplayName()
	data := whenData(ev)
	if ref != t.event.ID.String() {
		data["instance"] = ref
	}
	if was != nil {
		for k, v := range whenData(was) {
			data["was_"+k] = v
		}
		data["changed"] = strings.Join(t.changed, ",")
	}
	if t.what == toldNudged {
		data["again"] = "true"
	}
	key := kind + "|" + t.event.ID.String() + "|" + ref + "|" + strconv.Itoa(t.event.Sequence)
	if t.what == toldNudged {
		key += "|nudge|" + s.now().Format(time.RFC3339)
	}
	email := map[string]notificationBusiness.EventType{
		noticeInvited:   notificationBusiness.EventCalendarInvited,
		noticeUpdated:   notificationBusiness.EventCalendarUpdated,
		noticeCancelled: notificationBusiness.EventCalendarCancelled,
	}[kind]
	for zone, ids := range byZone {
		loc, err := loadZone(zone)
		if err != nil {
			loc = time.UTC
		}
		recipients := make([]notificationBusiness.NoticeRecipient, 0, len(ids))
		for _, id := range ids {
			recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: id.String(), Reason: "guest", InboxOnly: inbox})
		}
		title, body := noticeWords(kind, name, ev, was, t, loc)
		actorUser := actor.UserDgraphInfo
		tellNotice(ctx, notificationBusiness.Notice{
			Kind: kind, ResourceKind: KindEvent, ResourceUUID: t.event.ID.String(), ResourceTitle: ev.Title,
			Actor: &actorUser, Data: data, DedupKey: key,
			Title: title, Body: body, Path: eventPath(ref), Email: email,
			Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: ref, Tag: "calendar:" + ref},
		}, recipients)
	}
}

// uninvite tells people taken off an event that they were, never naming it:
// it is no longer theirs to open (CAL-BUG-1).
func (s *service) uninvite(ctx context.Context, actor *model.UserInfo, e *models.Event, people []uuid.UUID, inbox bool) {
	me := personOf(actor)
	people = slices.DeleteFunc(slices.Clone(people), func(id uuid.UUID) bool { return id == me })
	if len(people) == 0 || e == nil {
		return
	}
	start, _ := spanOf(e)
	recipients := make([]notificationBusiness.NoticeRecipient, 0, len(people))
	for _, id := range people {
		recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: id.String(), Reason: "subject", InboxOnly: inbox})
	}
	user := actor.UserDgraphInfo
	tellNotice(ctx, notificationBusiness.Notice{
		Kind: eventUninvited, ResourceKind: KindEvent, ResourceUUID: e.ID.String(), Actor: &user,
		Data: map[string]string{"start": start.UTC().Format(time.RFC3339)},
		// Each removal is told once, and a later one again.
		DedupKey: eventUninvited + "|" + e.ID.String() + "|" + strconv.FormatInt(s.now().UnixNano(), 10),
		Title:    user.DisplayName() + " took you off an event",
		Path:     "/app/calendar",
		// The calendar itself: the event is no longer theirs to open.
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR},
	}, recipients)
}

// answerNotice tells an event's organiser of a guest's answer: in their
// inbox, never pushed (D-CAL-17), and by email when they asked for it.
func (s *service) answerNotice(ctx context.Context, actor *model.UserInfo, ref string, ev *models.Event, t telling) {
	a := t.answer
	if a == nil || t.event.CreatedBy == uuid.Nil {
		return
	}
	data := map[string]string{"response": a.response}
	if a.note != "" {
		data["note"] = a.note
	}
	if ref != t.event.ID.String() {
		data["instance"] = ref
	}
	for k, v := range whenData(ev) {
		data[k] = v
	}
	var from *dgraphStruct.DgraphUser
	who := ""
	if a.key.UserID != nil && actor != nil {
		u := actor.UserDgraphInfo
		from, who = &u, u.DisplayName()
	} else {
		g := answerOf(t.guestsAfter, a.key)
		who = a.key.Email
		if g != nil && g.Name != "" {
			who = g.Name
		}
		data["guest_name"], data["guest_email"] = who, a.key.Email
	}
	zone := s.zoneOf(ctx, t.event.CreatedBy)
	loc, err := loadZone(zone)
	if err != nil {
		loc = time.UTC
	}
	title := who + " " + answerVerb(a.response) + " " + ev.Title
	body := whenWords(ev, loc)
	if a.note != "" {
		body = "“" + a.note + "”"
	}
	if p := a.proposal; p != nil {
		// The inbox words the time on its reader's own clock, from the data.
		data["proposed_start"], data["proposed_end"] = p.Start.UTC().Format(time.RFC3339), p.End.UTC().Format(time.RFC3339)
		title = who + " proposed a new time for " + ev.Title
		body = spanWords(p.Start, p.End, loc) + " instead of " + whenWords(ev, loc)
		if a.note != "" {
			body += " · “" + a.note + "”"
		}
	}
	recipient := notificationBusiness.NoticeRecipient{UserUUID: t.event.CreatedBy.String(), Reason: "organiser", InboxOnly: !t.event.EmailOnAnswers, NoPush: t.event.EmailOnAnswers}
	n := notificationBusiness.Notice{
		Kind: noticeAnswered, ResourceKind: KindEvent, ResourceUUID: t.event.ID.String(), ResourceTitle: ev.Title,
		Actor: from, Data: data,
		DedupKey: noticeAnswered + "|" + t.event.ID.String() + "|" + ref + "|" + a.key.String() + "|" + strconv.FormatInt(s.now().UnixNano(), 10),
		Title:    title, Body: body, Path: eventPath(ref),
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: ref, From: who},
	}
	if t.event.EmailOnAnswers {
		n.Email = notificationBusiness.EventCalendarAnswered
	}
	tellNotice(ctx, n, []notificationBusiness.NoticeRecipient{recipient})
}

// answerVerb is an answer as a sentence says it ("Jonas is going to …"). Pure.
func answerVerb(response string) string {
	switch response {
	case responseAccepted:
		return "is going to"
	case responseTentative:
		return "might go to"
	case responseDeclined:
		return "can't go to"
	}
	return "answered"
}

// eventPath is where a notice about an event, or one instance of it, opens.
func eventPath(ref string) string { return "/app/calendar/event/" + ref }

// whenData is an event's time for a notice's data, which the inbox words on
// its reader's own clock: instants, or an all-day event's dates. Pure.
func whenData(e *models.Event) map[string]string {
	if e.AllDay() {
		return map[string]string{"date": e.StartDate, "end_date": e.EndDate, "all_day": "true"}
	}
	return map[string]string{"start": e.StartAt.UTC().Format(time.RFC3339), "end": e.EndAt.UTC().Format(time.RFC3339)}
}

// whenWords is an event's time in words, on a clock in loc: "Thu 16 Oct,
// 10:00–11:00", "Thu 16 Oct 22:00 – Fri 17 Oct 01:00", "Thu 16 Oct" for a day,
// "Thu 16 – Sat 18 Oct" for days. Pure.
func whenWords(e *models.Event, loc *time.Location) string {
	if e.AllDay() {
		first, _ := parseDate(e.StartDate)
		after, _ := parseDate(e.EndDate)
		last := after.AddDate(0, 0, -1)
		if !last.After(first) {
			return first.Format(helpers.DayLayout)
		}
		return first.Format(helpers.DayLayout) + " – " + last.Format(helpers.DayLayout)
	}
	return spanWords(*e.StartAt, *e.EndAt, loc)
}

// spanWords is a time from start to end in words, on a clock in loc: "Thu 16
// Oct, 10:00–11:00", or "Thu 16 Oct 22:00 – Fri 17 Oct 01:00" across days. Pure.
func spanWords(start, end time.Time, loc *time.Location) string {
	s, en := start.In(loc), end.In(loc)
	if s.Year() == en.Year() && s.YearDay() == en.YearDay() {
		return s.Format(helpers.DayLayout) + ", " + s.Format(helpers.ClockLayout) + "–" + en.Format(helpers.ClockLayout)
	}
	return s.Format(helpers.DayLayout) + " " + s.Format(helpers.ClockLayout) + " – " + en.Format(helpers.DayLayout) + " " + en.Format(helpers.ClockLayout)
}

// noticeWords is a notice's push and email lines, on a clock in loc. Pure.
func noticeWords(kind, actor string, ev, was *models.Event, t telling, loc *time.Location) (title, body string) {
	when := whenWords(ev, loc)
	switch kind {
	case noticeInvited:
		if t.what == toldNudged {
			return actor + " is waiting for your answer", ev.Title + " · " + when
		}
		return actor + " invited you to " + ev.Title, when
	case noticeUpdated:
		title = actor + " changed " + ev.Title
		if was == nil {
			return title, when
		}
		if slices.Contains(t.changed, "time") {
			return title, "Now " + when + " · was " + whenWords(was, loc)
		}
		if slices.Contains(t.changed, "location") && ev.Location != "" {
			return title, "Now at " + ev.Location + " · " + when
		}
		if slices.Contains(t.changed, "title") && was.Title != ev.Title {
			return title, "It was " + was.Title + " · " + when
		}
		return title, when
	case noticeCancelled:
		return actor + " cancelled " + ev.Title, "It was " + when
	}
	return actor + " changed " + ev.Title, when
}

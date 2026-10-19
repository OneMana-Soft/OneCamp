package Calendar

// The Event Model (calendar programme PROGRAM.md §4.2 #38, A1): an event is a
// row in Postgres's events table (migrations/345_calendar_store), written by
// one service (service.go) in one transaction, and read by everything that
// reads the calendar. It replaces the two stores (calendar_events and the
// Dgraph Event node) unless CALENDAR_STORE=graph keeps them (store.go).
//
// An event's time is zoned or floating, never ambiguous (CAL3): a timed event
// keeps the instants and the zone it was planned in, and an all-day event is
// its dates, the same day for everyone.

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// EventKind is what an event is. Each kind keeps its own behaviour: focus time
// holds its organiser's notifications, out of office takes their working days
// off the workload.
type EventKind string

const (
	EventPlain           EventKind = "event"
	EventFocus           EventKind = "focus"
	EventAway            EventKind = "out_of_office"
	EventWorkingLocation EventKind = "working_location"
	EventTaskBlock       EventKind = "task_block"
	EventBooking         EventKind = "booking"
	EventReminder        EventKind = "reminder"
)

var eventKinds = map[EventKind]bool{EventPlain: true, EventFocus: true, EventAway: true, EventWorkingLocation: true,
	EventTaskBlock: true, EventBooking: true, EventReminder: true}

// The values an event's settings take.
const (
	notesText = "text"
	notesHTML = "html"

	transparencyOpaque      = "opaque"
	transparencyTransparent = "transparent"

	visibilityDefault = "default"
	visibilityPublic  = "public"
	visibilityPrivate = "private"

	statusConfirmed = "confirmed"
	statusCancelled = "cancelled"

	roleRequired = "required"
	roleOptional = "optional"

	responseNeedsAction = "needs_action"
	responseAccepted    = "accepted"
	responseDeclined    = "declined"
	responseTentative   = "tentative"

	sourceOneCamp = "onecamp"
	sourceMoved   = "moved"
)

// Limits on what one event holds (BE-F-15: the web capped titles, the server
// nothing).
const (
	maxTitleRunes = 300
	maxNotesRunes = 100000
	maxPlaceRunes = 1000
	maxGuests     = 500
	// maxAnswerNoteRunes is the most a guest's note with their answer holds.
	maxAnswerNoteRunes = 500
	// maxGuestNameRunes is the most a guest by email's name holds.
	maxGuestNameRunes = 120
	// maxRangeDays is the longest stretch one read of the calendar covers
	// (BE-F-13: a ten-year read walked everything).
	maxRangeDays = 400
	// maxInstances is the most instances one read answers.
	maxInstances = 5000
)

// dateLayout is how a date is written ("2026-10-16").
const dateLayout = "2006-01-02"

// When is when an event starts or ends: an instant and the IANA zone it was
// planned in, or, for an all-day event, a date.
type When struct {
	At   *time.Time `json:"at,omitempty"`
	Zone string     `json:"zone,omitempty"`
	Date string     `json:"date,omitempty"`
}

// Person is someone on an event, named as every list names people. A guest
// invited by email has no uuid: their name, if they were invited with one.
type Person struct {
	UUID       string `json:"user_uuid,omitempty"`
	Name       string `json:"user_name,omitempty"`
	FullName   string `json:"user_full_name,omitempty"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
}

// Attendee is a guest of an event: a member, or someone invited by email
// (Email, and the name they were invited by). Response is their answer for
// the occurrence read (an instance answered on its own says so); Invite, for
// the organiser alone, says whether a guest by email was emailed their
// invitation (sent, not_sent: this server sends no such email, failed).
type Attendee struct {
	Person
	Email       string     `json:"email,omitempty"`
	Role        string     `json:"role"`
	Response    string     `json:"response"`
	Note        string     `json:"note,omitempty"`
	RespondedAt *time.Time `json:"responded_at,omitempty"`
	Invite      string     `json:"invite,omitempty"`
}

// Recurrence is how a series repeats: its RFC 5545 rule and the rule in
// words ("Weekly on Tuesday").
type Recurrence struct {
	Rule string `json:"rule"`
	Text string `json:"text,omitempty"`
}

// Event is an event as the calendar's API sends it to one reader: a single
// event, a series (by its own id), or one instance of a series (by the
// series' id and the instance's start, "<id>_20261016T091500Z").
type Event struct {
	ID         string `json:"id"`
	CalendarID string `json:"calendar_id"`
	// SeriesID and RecurrenceID, for an instance, name its series and the
	// start it has in the series (which a change by hand never moves).
	SeriesID     string `json:"series_id,omitempty"`
	RecurrenceID *When  `json:"recurrence_id,omitempty"`
	// Recurrence is a series' rule, for the series and each instance.
	Recurrence         *Recurrence `json:"recurrence,omitempty"`
	Kind               EventKind   `json:"kind"`
	Title              string      `json:"title"`
	Notes              string      `json:"notes"`
	NotesFormat        string      `json:"notes_format"`
	Location           string      `json:"location"`
	CallRoom           string      `json:"call_room,omitempty"`
	Start              When        `json:"start"`
	End                When        `json:"end"`
	AllDay             bool        `json:"all_day"`
	Transparency       string      `json:"transparency"`
	Visibility         string      `json:"visibility"`
	Hue                string      `json:"hue,omitempty"`
	Status             string      `json:"status"`
	GuestsCanModify    bool        `json:"guests_can_modify"`
	GuestsCanInvite    bool        `json:"guests_can_invite"`
	GuestsCanSeeGuests bool        `json:"guests_can_see_guests"`
	// EmailOnAnswers is the organiser's "Email me when guests answer",
	// said to the organiser alone.
	EmailOnAnswers bool                       `json:"email_on_answers,omitempty"`
	Organiser      Person                     `json:"organiser"`
	Attendees      []Attendee                 `json:"attendees"`
	Sequence       int                        `json:"sequence"`
	ICalUID        string                     `json:"ical_uid"`
	GoogleEventID  string                     `json:"google_event_id,omitempty"`
	Booking        *dgraphStruct.EventBooking `json:"booking,omitempty"`
	CreatedAt      time.Time                  `json:"created_at"`
	UpdatedAt      time.Time                  `json:"updated_at"`
	DeletedAt      *time.Time                 `json:"deleted_at,omitempty"`
}

// GuestInput is a guest named in a write: a workspace member by id, or
// anyone by email (with the name to call them by, if known).
type GuestInput struct {
	UUID  string `json:"user_uuid,omitempty"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
}

// Tell guests? (D-CAL-5): a write's Notify says whether guests are emailed
// and pushed about it; "none" is Don't send. In-app notices always go.
const notifyNone = "none"

// EventInput is a new event.
type EventInput struct {
	Kind               EventKind    `json:"kind"`
	Title              string       `json:"title"`
	Notes              string       `json:"notes"`
	NotesFormat        string       `json:"notes_format"`
	Location           string       `json:"location"`
	Start              When         `json:"start"`
	End                When         `json:"end"`
	Transparency       string       `json:"transparency"`
	Visibility         string       `json:"visibility"`
	Hue                string       `json:"hue"`
	Attendees          []GuestInput `json:"attendees"`
	GuestsCanModify    *bool        `json:"guests_can_modify"`
	GuestsCanInvite    *bool        `json:"guests_can_invite"`
	GuestsCanSeeGuests *bool        `json:"guests_can_see_guests"`
	// Recurrence makes it a series (its rule; the text is ignored).
	Recurrence *Recurrence `json:"recurrence"`
	// SyncToGoogle copies it to the organiser's Google Calendar when linked.
	SyncToGoogle bool `json:"sync_to_google"`
	// Notify is "none" for Don't send (D-CAL-5); empty sends.
	Notify string `json:"notify"`
	// EmailOnAnswers is the organiser's "Email me when guests answer".
	EmailOnAnswers *bool `json:"email_on_answers"`
}

// EventPatch is a change to an event (CAL2): a field left out stays as it
// is, a field sent is set ("" clears it), and a list sent replaces the list.
// Sequence is the one the writer last read: a write that names an older one
// is refused, so nobody's newer change is overwritten unseen.
type EventPatch struct {
	Sequence           *int          `json:"sequence"`
	Kind               *EventKind    `json:"kind"`
	Title              *string       `json:"title"`
	Notes              *string       `json:"notes"`
	NotesFormat        *string       `json:"notes_format"`
	Location           *string       `json:"location"`
	Start              *When         `json:"start"`
	End                *When         `json:"end"`
	Transparency       *string       `json:"transparency"`
	Visibility         *string       `json:"visibility"`
	Hue                *string       `json:"hue"`
	Attendees          *[]GuestInput `json:"attendees"`
	GuestsCanModify    *bool         `json:"guests_can_modify"`
	GuestsCanInvite    *bool         `json:"guests_can_invite"`
	GuestsCanSeeGuests *bool         `json:"guests_can_see_guests"`
	// Recurrence sets how a series repeats (a rule "" stops it), for a
	// series, or an instance's series from it on ("following") or as a whole.
	Recurrence *Recurrence `json:"recurrence"`
	// Scope says, for an instance of a series, what the change is for:
	// "this" event, "following" (this and following) or "all" events.
	Scope string `json:"scope"`
	// Notify is "none" for Don't send (D-CAL-5); empty sends.
	Notify string `json:"notify"`
	// EmailOnAnswers is the organiser's "Email me when guests answer".
	EmailOnAnswers *bool `json:"email_on_answers"`
}

// AnswerInput is a guest's answer: yes, no or maybe, with a note, for a whole
// event or series, or (Scope "this") for one instance of a series alone. An
// answer that leaves its note out keeps the note as it was: the event card
// answers with a tap, and only the panel and the guest's page write a note.
type AnswerInput struct {
	Answer Answer  `json:"answer"`
	Note   *string `json:"note"`
	Scope  string  `json:"scope"`
}

// noteOr is the note an answer sends, trimmed, or kept when it sends none.
func (in AnswerInput) noteOr(kept string) string {
	if in.Note == nil {
		return kept
	}
	return strings.TrimSpace(*in.Note)
}

// noteTooLong is whether the note an answer sends is over its length.
func (in AnswerInput) noteTooLong() bool {
	return in.Note != nil && utf8.RuneCountInString(strings.TrimSpace(*in.Note)) > maxAnswerNoteRunes
}

// inputError is an EventInputError, said in words.
func inputError(msg string) error { return &EventInputError{msg} }

// checkText checks an event's words.
func checkText(title, notes, location string) error {
	if utf8.RuneCountInString(title) > maxTitleRunes {
		return inputError("A title can be up to 300 characters.")
	}
	if utf8.RuneCountInString(notes) > maxNotesRunes {
		return inputError("Notes can be up to 100,000 characters.")
	}
	if utf8.RuneCountInString(location) > maxPlaceRunes {
		return inputError("A location can be up to 1,000 characters.")
	}
	return nil
}

// loadZone is an IANA zone Go can read, or an error said in words. "" is UTC.
func loadZone(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}
	if name == "Local" {
		return nil, inputError("Name a time zone, such as Europe/London.")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, inputError(`"` + name + `" isn't a time zone OneCamp knows.`)
	}
	return loc, nil
}

// parseDate reads "2026-10-16".
func parseDate(s string) (time.Time, bool) {
	d, err := time.Parse(dateLayout, s)
	return d, err == nil && d.Format(dateLayout) == s
}

// setWhen puts start and end on e: both instants (with zones) or both dates.
// Pure.
func setWhen(e *models.Event, start, end When) error {
	switch {
	case start.Date != "" || end.Date != "":
		first, ok1 := parseDate(start.Date)
		after, ok2 := parseDate(end.Date)
		if !ok1 || !ok2 || start.At != nil || end.At != nil {
			return inputError("An all-day event has a start date and an end date, and no times.")
		}
		if !after.After(first) {
			return inputError("An all-day event ends on the day after its last day, so its end date is after its start date.")
		}
		zone := strings.TrimSpace(start.Zone)
		if _, err := loadZone(zone); err != nil {
			return err
		}
		if zone == "" {
			zone = e.TZ
		}
		e.StartAt, e.EndAt = nil, nil
		e.StartDate, e.EndDate = start.Date, end.Date
		e.TZ, e.EndTZ = zoneOrUTC(zone), ""
	case start.At != nil && end.At != nil:
		if end.At.Before(*start.At) {
			return inputError("The end is before the start.")
		}
		startZone, endZone := strings.TrimSpace(start.Zone), strings.TrimSpace(end.Zone)
		if _, err := loadZone(startZone); err != nil {
			return err
		}
		if _, err := loadZone(endZone); err != nil {
			return err
		}
		if startZone == "" {
			startZone = e.TZ
		}
		s, en := start.At.UTC(), end.At.UTC()
		e.StartAt, e.EndAt = &s, &en
		e.StartDate, e.EndDate = "", ""
		e.TZ = zoneOrUTC(startZone)
		e.EndTZ = ""
		if endZone != "" && endZone != e.TZ {
			e.EndTZ = endZone
		}
	default:
		return inputError("An event needs a start and an end.")
	}
	return nil
}

func zoneOrUTC(z string) string {
	if strings.TrimSpace(z) == "" {
		return "UTC"
	}
	return z
}

// whenOf is an event's start and end as the API says them. Pure.
func whenOf(e *models.Event) (start, end When) {
	if e.AllDay() {
		return When{Date: e.StartDate, Zone: e.TZ}, When{Date: e.EndDate}
	}
	endZone := e.EndTZ
	if endZone == "" {
		endZone = e.TZ
	}
	return When{At: e.StartAt, Zone: e.TZ}, When{At: e.EndAt, Zone: endZone}
}

// spanOf is the stretch of time an event takes: its instants, or for an
// all-day event the midnights of its dates in its own zone. Pure.
func spanOf(e *models.Event) (time.Time, time.Time) {
	if !e.AllDay() {
		return *e.StartAt, *e.EndAt
	}
	loc, err := loadZone(e.TZ)
	if err != nil {
		loc = time.UTC
	}
	return midnightOf(e.StartDate, loc), midnightOf(e.EndDate, loc)
}

// midnightOf is the midnight that starts a date in loc. Pure.
func midnightOf(date string, loc *time.Location) time.Time {
	d, _ := time.Parse(dateLayout, date)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).UTC()
}

// utcMidnightOf is a date's midnight in UTC: how an all-day event's instants
// were always sent beside its dates (Google's, since CAL-BUG-2). Pure.
func utcMidnightOf(date string) time.Time {
	d, _ := time.Parse(dateLayout, date)
	return d.UTC()
}

// checkKindRules checks what a kind allows. Pure.
func checkKindRules(e *models.Event) error {
	if !eventKinds[EventKind(e.Kind)] {
		return inputError("That isn't a kind of event.")
	}
	if e.NotesFormat != notesText && e.NotesFormat != notesHTML {
		return inputError("Notes are text or html.")
	}
	if e.Transparency != transparencyOpaque && e.Transparency != transparencyTransparent {
		return inputError("An event shows as busy (opaque) or free (transparent).")
	}
	switch e.Visibility {
	case visibilityDefault, visibilityPublic, visibilityPrivate:
	default:
		return inputError("An event's visibility is default, public or private.")
	}
	// One of the six camp hues (DESIGN_V3 §3.5), or none for its calendar's.
	if e.Hue != "" && !helpers.IsCampHue(e.Hue) {
		return inputError("That isn't one of the event colours.")
	}
	return nil
}

// datesIn is the dates start and end stand for when both are midnights in
// loc, a day or more apart: how an all-day event was written as instants.
// Pure.
func datesIn(start, end time.Time, loc *time.Location) (first, after string, ok bool) {
	s, e := start.In(loc), end.In(loc)
	if !isMidnight(s) || !isMidnight(e) {
		return "", "", false
	}
	first, after = s.Format(dateLayout), e.Format(dateLayout)
	return first, after, after > first
}

func isMidnight(t time.Time) bool {
	return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
}

// The UTC offsets a place can have, in minutes: −12:00 to +14:00.
const (
	minOffsetMinutes = -12 * 60
	maxOffsetMinutes = 14 * 60
)

// datesAtAnyOffset is the dates start and end stand for when each is a
// midnight at some UTC offset a place can have, the end's within an hour of
// the start's (a clock change between them): how the web sends an all-day
// event, at the reader's own midnights, when the reader is somewhere else
// than the event's zone. When two offsets fit the start (+13:45 and −10:15
// both make 10:15 UTC a midnight), the dates nearer near are taken. Pure.
func datesAtAnyOffset(start, end time.Time, near string) (first, after string, ok bool) {
	starts := midnightOffsets(start)
	if len(starts) == 0 {
		return "", "", false
	}
	nearDay, haveNear := parseDate(near)
	best, bestGap := "", 0
	var bestOffset int
	for _, o := range starts {
		d := start.UTC().Add(time.Duration(o) * time.Minute).Format(dateLayout)
		gap := 0
		if haveNear {
			day, _ := parseDate(d)
			gap = int(day.Sub(nearDay).Abs().Hours() / 24)
		}
		if best == "" || gap < bestGap {
			best, bestGap, bestOffset = d, gap, o
		}
	}
	for _, o := range midnightOffsets(end) {
		if diff := o - bestOffset; diff >= -60 && diff <= 60 {
			after = end.UTC().Add(time.Duration(o) * time.Minute).Format(dateLayout)
			if after > best {
				return best, after, true
			}
		}
	}
	return "", "", false
}

// midnightOffsets are the UTC offsets, in minutes, at which t is a midnight.
// Every place's offset is a whole number of quarter hours (+5:45, +12:45,
// −3:30), so no other offset is one.
func midnightOffsets(t time.Time) []int {
	u := t.UTC()
	if u.Second() != 0 || u.Nanosecond() != 0 || u.Minute()%15 != 0 {
		return nil
	}
	minuteOfDay := u.Hour()*60 + u.Minute()
	var out []int
	for _, o := range []int{-minuteOfDay, 24*60 - minuteOfDay} {
		if o >= minOffsetMinutes && o <= maxOffsetMinutes {
			out = append(out, o)
		}
	}
	return out
}

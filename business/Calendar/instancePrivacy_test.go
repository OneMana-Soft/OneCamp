package Calendar

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// V-613: one instance of a repeating event private at its provider (or in
// its file) on its own, its series not, is private here when it first comes
// in as an override: private wins, as for a whole event, on every path a
// remote instance becomes an override (a file's or a new series' import, and
// a sync's instance of a series already here).

// overrideOf is the override of series' instance at rid, failing the test
// when there is none.
func overrideOf(t *testing.T, h *harness, series *models.Event, rid time.Time) *models.Event {
	t.Helper()
	if series == nil {
		t.Fatal("the series didn't come")
	}
	ov, err := h.store.overrideAt(context.Background(), series.ID, rid)
	if err != nil || ov == nil {
		t.Fatalf("the instance at %v didn't come: %v", rid, err)
	}
	return ov
}

// privateHere marks the test failed unless ov is private here, so who may only see
// its calendar sees it as busy.
func privateHere(t *testing.T, ov *models.Event, what string) {
	t.Helper()
	if ov.Visibility != visibilityPrivate {
		t.Errorf("%s: the instance %q is %q here, want private", what, ov.Title, ov.Visibility)
	}
	if !hiddenFrom(ov, nil, uuid.New(), accessBusiness.Decision{Level: accessBusiness.LevelView}) {
		t.Errorf("%s: someone who may only see the calendar sees the private instance %q", what, ov.Title)
	}
}

func TestAnInstancePrivateAtGoogleUnderADefaultSeriesIsPrivateHere(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	instance := func(id, uid, summary string, rid time.Time) calendarSyncBusiness.Change {
		late := rid.Add(time.Hour)
		return calendarSyncBusiness.Change{ExternalID: id + "_" + rid.Format("20060102T150405Z"), SeriesID: id,
			Event: ICS.Event{UID: uid, RecurrenceID: &rid, Summary: summary, Start: late, End: late.Add(15 * time.Minute), Zone: "Europe/London", Class: "PRIVATE"}}
	}
	// A new series with its private instance: through the import.
	g.put(calendarSyncBusiness.Change{ExternalID: "oo", Event: ICS.Event{UID: "oneone@google.com", Summary: "1:1", Start: start, End: start.Add(30 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5"}})
	ridOO := start.AddDate(0, 0, 1)
	g.put(instance("oo", "oneone@google.com", "1:1, about my leave", ridOO))
	g.put(calendarSyncBusiness.Change{ExternalID: "st", Event: ICS.Event{UID: "standup@google.com", Summary: "Standup", Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5"}})
	_, sweep := syncWithGoogle(t, h, sam, g)
	oo := eventFromGoogle(h, "oneone@google.com")
	if oo == nil || oo.Visibility != visibilityDefault {
		t.Fatalf("the series isn't default here: %+v", oo)
	}
	privateHere(t, overrideOf(t, h, oo, ridOO), "a new series' instance")

	// An instance of a series already here, private at Google on its own.
	rid := start.AddDate(0, 0, 2)
	g.put(instance("st", "standup@google.com", "Standup, the review", rid))
	sweep()
	standup := eventFromGoogle(h, "standup@google.com")
	if standup == nil || standup.Visibility != visibilityDefault {
		t.Fatalf("the series isn't default here: %+v", standup)
	}
	privateHere(t, overrideOf(t, h, standup, rid), "a series' new instance")
	if got := g.said(); len(got) != 0 {
		t.Fatalf("in step with Google, sent %v", got)
	}
}

// outlookSeries is a weekly Tuesday noon series of four as Graph sends its
// master, with the instances changed on their own (raw Graph events).
func outlookSeries(id, version, subject string, exceptions ...string) string {
	return fmt.Sprintf(`{"id":%q,"iCalUId":"%s-uid","changeKey":%q,"type":"seriesMaster","subject":%q,"sensitivity":"normal","showAs":"busy",`+
		`"start":{"dateTime":"2026-11-10T12:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-10T12:30:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time",`+
		`"recurrence":{"pattern":{"type":"weekly","interval":1,"daysOfWeek":["tuesday"],"firstDayOfWeek":"sunday"},"range":{"type":"numbered","startDate":"2026-11-10","numberOfOccurrences":4}},`+
		`"exceptionOccurrences":[%s]}`, id, id, version, subject, strings.Join(exceptions, ","))
}

// outlookException is one instance of master's, on 17 November, an hour
// later than the series has it.
func outlookException(master, id, subject, sensitivity string) string {
	return fmt.Sprintf(`{"id":%q,"type":"exception","seriesMasterId":%q,"changeKey":"%s-k","subject":%q,"sensitivity":%q,"showAs":"busy","originalStart":"2026-11-17T12:00:00Z",`+
		`"start":{"dateTime":"2026-11-17T13:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-17T13:30:00","timeZone":"UTC"},"originalStartTimeZone":"GMT Standard Time"}`,
		id, master, id, subject, sensitivity)
}

func TestAnInstancePrivateAtOutlookUnderANormalSeriesIsPrivateHere(t *testing.T) {
	h := newHarness(t)
	maya := h.member("Maya")
	f := newFakeOutlook(t)
	f.put("m1", outlookSeries("m1", "mk1", "1:1", outlookException("m1", "x1", "1:1, about my leave", "private")))
	f.put("m2", outlookSeries("m2", "mk2", "Standup"))
	sweep := syncWithProviders(t, h, map[string]calendarSyncBusiness.Adapter{"microsoft": outlookOn(f)}, map[string]int{})
	move(t, h, maya, "microsoft")
	rid := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	oneOne := eventFromGoogle(h, "m1-uid")
	if oneOne == nil || oneOne.Visibility != visibilityDefault {
		t.Fatalf("the series isn't default here: %+v", oneOne)
	}
	privateHere(t, overrideOf(t, h, oneOne, rid), "a new series' instance")

	// An instance of a series already here, private at Outlook on its own.
	f.put("m2", outlookSeries("m2", "mk2", "Standup", outlookException("m2", "x2", "Standup, the review", "private")))
	sweep()
	standup := eventFromGoogle(h, "m2-uid")
	if standup == nil || standup.Visibility != visibilityDefault {
		t.Fatalf("the series isn't default here: %+v", standup)
	}
	privateHere(t, overrideOf(t, h, standup, rid), "a series' new instance")
	if got := f.sensitivities(); len(got) != 0 {
		t.Fatalf("in step with Outlook, sent %v", got)
	}
}

// A file's instance takes its own CLASS when that is private, and its
// series' when the series is: private wins either way.
func TestAFilesPrivateInstanceIsPrivateHere(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	file := strings.Join([]string{
		"BEGIN:VCALENDAR", "PRODID:-//Example//EN", "VERSION:2.0",
		"BEGIN:VEVENT", "DTSTART:20261110T090000Z", "DTEND:20261110T093000Z", "RRULE:FREQ=DAILY;COUNT=5",
		"UID:oneone@example.com", "SUMMARY:1:1", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261111T100000Z", "DTEND:20261111T103000Z", "RECURRENCE-ID:20261111T090000Z",
		"UID:oneone@example.com", "SUMMARY:1:1\\, about my leave", "CLASS:PRIVATE", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261110T120000Z", "DTEND:20261110T130000Z", "RRULE:FREQ=DAILY;COUNT=5",
		"UID:therapy@example.com", "SUMMARY:Therapy", "CLASS:CONFIDENTIAL", "END:VEVENT",
		"BEGIN:VEVENT", "DTSTART:20261111T130000Z", "DTEND:20261111T140000Z", "RECURRENCE-ID:20261111T120000Z",
		"UID:therapy@example.com", "SUMMARY:Therapy, later", "END:VEVENT",
		"END:VCALENDAR", ""}, "\r\n")
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil || len(made.Created) != 2 || len(made.Skipped) != 0 {
		t.Fatalf("made %+v %v", made, err)
	}
	oneOne, therapy := h.store.events[uuid.MustParse(made.Created[0].UUID)], h.store.events[uuid.MustParse(made.Created[1].UUID)]
	if oneOne.Visibility != visibilityDefault || therapy.Visibility != visibilityPrivate {
		t.Fatalf("the series are %q and %q", oneOne.Visibility, therapy.Visibility)
	}
	privateHere(t, overrideOf(t, h, oneOne, time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)), "a private instance of a default series")
	privateHere(t, overrideOf(t, h, therapy, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)), "an instance of a private series")
}

// readWholeTwoWay is what migration 579 (calendar_instance_privacy_repull)
// does: every Google and Outlook link's calendars lose their cursor, so the
// next sweep reads them whole.
func (m *memLedger) readWholeTwoWay() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.calendars {
		for _, l := range m.links {
			if l.ID == c.LinkID && (l.Provider == "google" || l.Provider == "microsoft") {
				m.calendars[i].Cursor = ""
			}
		}
	}
}

// An override written before V-613, with its series' visibility though its
// instance is private at the provider, isn't read again from a cursor (the
// instance hasn't changed there). The one whole read migration 579 asks for
// makes it private, at Google and at Outlook, and nothing is sent back.
func TestAnInstanceThatCameInWithItsSeriesVisibilityIsPrivateAfterTheWholeRead(t *testing.T) {
	h := newHarness(t)
	sam, maya := h.member("Sam"), h.member("Maya")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "st", Event: ICS.Event{UID: "standup@google.com", Summary: "Standup", Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5"}})
	ridG := start.AddDate(0, 0, 2)
	late := ridG.Add(time.Hour)
	g.put(calendarSyncBusiness.Change{ExternalID: "st_" + ridG.Format("20060102T150405Z"), SeriesID: "st",
		Event: ICS.Event{UID: "standup@google.com", RecurrenceID: &ridG, Summary: "Standup, the review", Start: late, End: late.Add(15 * time.Minute), Zone: "Europe/London", Class: "PRIVATE"}})
	f := newFakeOutlook(t)
	f.put("m1", outlookSeries("m1", "mk1", "1:1", outlookException("m1", "x1", "1:1, about my leave", "private")))
	sweep := syncWithProviders(t, h, map[string]calendarSyncBusiness.Adapter{"google": g, "microsoft": outlookOn(f)}, map[string]int{})
	ledger := h.s.ledger.(*memLedger)
	move(t, h, sam, "google")
	move(t, h, maya, "microsoft")
	ridO := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	standup, oneOne := eventFromGoogle(h, "standup@google.com"), eventFromGoogle(h, "m1-uid")
	before := []*models.Event{overrideOf(t, h, standup, ridG), overrideOf(t, h, oneOne, ridO)}
	// As the code before V-613 wrote them: the series' visibility.
	h.store.mu.Lock()
	for _, ov := range before {
		h.store.events[ov.ID].Visibility = visibilityDefault
	}
	h.store.mu.Unlock()
	sweep()
	if ov := overrideOf(t, h, standup, ridG); ov.Visibility != visibilityDefault {
		t.Fatalf("a read from a cursor read Google's unchanged instance again: %+v", ov)
	}

	ledger.readWholeTwoWay()
	sweep()
	privateHere(t, overrideOf(t, h, standup, ridG), "Google's, read whole")
	privateHere(t, overrideOf(t, h, oneOne, ridO), "Outlook's, read whole")
	if got := g.said(); len(got) != 0 {
		t.Fatalf("read whole, Google was sent %v", got)
	}
	if got := f.sensitivities(); len(got) != 0 {
		t.Fatalf("read whole, Outlook was sent %v", got)
	}
}

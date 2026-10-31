package ICS

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A Google Calendar export: a weekly series in London with one instance
// moved and one skipped, an all-day event, guests with their answers, an
// alarm, a folded description with escapes.
const googleExport = "BEGIN:VCALENDAR\r\n" +
	"PRODID:-//Google Inc//Google Calendar 70.9054//EN\r\n" +
	"VERSION:2.0\r\n" +
	"CALSCALE:GREGORIAN\r\n" +
	"METHOD:PUBLISH\r\n" +
	"X-WR-CALNAME:Jonas Weber\r\n" +
	"X-WR-TIMEZONE:Europe/London\r\n" +
	"BEGIN:VTIMEZONE\r\n" +
	"TZID:Europe/London\r\n" +
	"BEGIN:STANDARD\r\n" +
	"TZOFFSETFROM:+0100\r\n" +
	"TZOFFSETTO:+0000\r\n" +
	"DTSTART:19701025T020000\r\n" +
	"END:STANDARD\r\n" +
	"END:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART;TZID=Europe/London:20261006T091500\r\n" +
	"DTEND;TZID=Europe/London:20261006T100000\r\n" +
	"RRULE:FREQ=WEEKLY;BYDAY=TU;UNTIL=20261215T235959Z\r\n" +
	"EXDATE;TZID=Europe/London:20261020T091500\r\n" +
	"DTSTAMP:20261001T080000Z\r\n" +
	"ORGANIZER;CN=Jonas Weber:mailto:jonas@example.com\r\n" +
	"UID:design-review@google.com\r\n" +
	"ATTENDEE;CUTYPE=INDIVIDUAL;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;CN=Ma\r\n" +
	" ya Chen;X-NUM-GUESTS=0:mailto:maya@example.com\r\n" +
	"ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=TENTATIVE;RSVP=TRUE;CN=\"Lee, Sam\":mailto:sam@example.com\r\n" +
	"DESCRIPTION:Agenda:\\n1. Pricing\\; then\\, the launch\r\n" +
	"LOCATION:Room 4\\, second floor\r\n" +
	"X-GOOGLE-CONFERENCE:https://meet.google.com/abc-defg-hij\r\n" +
	"ATTACH;FILENAME=Pricing deck;FMTTYPE=application/vnd.google-apps.presentation:https://docs.google.com/presentation/d/1x\r\n" +
	"ATTACH;ENCODING=BASE64;VALUE=BINARY:aGVsbG8=\r\n" +
	"SEQUENCE:2\r\n" +
	"STATUS:CONFIRMED\r\n" +
	"SUMMARY:Weekly design review\r\n" +
	"TRANSP:OPAQUE\r\n" +
	"BEGIN:VALARM\r\n" +
	"ACTION:DISPLAY\r\n" +
	"DESCRIPTION:This is an event reminder\r\n" +
	"TRIGGER:-P0DT0H10M0S\r\n" +
	"END:VALARM\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART;TZID=Europe/London:20261028T140000\r\n" +
	"DTEND;TZID=Europe/London:20261028T144500\r\n" +
	"DTSTAMP:20261001T080000Z\r\n" +
	"UID:design-review@google.com\r\n" +
	"RECURRENCE-ID;TZID=Europe/London:20261027T091500\r\n" +
	"SUMMARY:Weekly design review (moved)\r\n" +
	"SEQUENCE:3\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"DTSTART;VALUE=DATE:20261224\r\n" +
	"DTEND;VALUE=DATE:20261227\r\n" +
	"DTSTAMP:20261001T080000Z\r\n" +
	"UID:holiday@google.com\r\n" +
	"SUMMARY:Office closed\r\n" +
	"TRANSP:TRANSPARENT\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestReadsAGoogleExport(t *testing.T) {
	cal, err := Read(strings.NewReader(googleExport))
	if err != nil {
		t.Fatal(err)
	}
	if cal.Name != "Jonas Weber" || cal.Zone != "Europe/London" || cal.Method != "PUBLISH" {
		t.Fatalf("calendar: %+v", cal)
	}
	if len(cal.Events) != 3 || len(cal.Skipped) != 0 {
		t.Fatalf("events %d, skipped %v", len(cal.Events), cal.Skipped)
	}
	series, moved, holiday := cal.Events[0], cal.Events[1], cal.Events[2]
	// 09:15 in London on 6 Oct (summer time) is 08:15 UTC.
	if !series.Start.Equal(time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC)) || series.Zone != "Europe/London" {
		t.Fatalf("series start %v in %q", series.Start, series.Zone)
	}
	if series.End.Sub(series.Start) != 45*time.Minute {
		t.Fatalf("series length %v", series.End.Sub(series.Start))
	}
	if series.RRule != "FREQ=WEEKLY;BYDAY=TU;UNTIL=20261215T235959Z" || len(series.ExDates) != 1 || !series.ExDates[0].Equal(time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("rule %q, skipped %v", series.RRule, series.ExDates)
	}
	if series.Description != "Agenda:\n1. Pricing; then, the launch" || series.Location != "Room 4, second floor" {
		t.Fatalf("text: %q / %q", series.Description, series.Location)
	}
	want := []Attendee{
		{Address: Address{Name: "Maya Chen", Email: "maya@example.com"}, PartStat: Accepted},
		{Address: Address{Name: "Lee, Sam", Email: "sam@example.com"}, PartStat: Tentative, Optional: true, RSVP: true},
	}
	if !reflect.DeepEqual(series.Attendees, want) || series.Organizer.Email != "jonas@example.com" {
		t.Fatalf("guests %+v, organiser %+v", series.Attendees, series.Organizer)
	}
	if series.Conference != "https://meet.google.com/abc-defg-hij" || len(series.Attachments) != 1 || series.Attachments[0] != (Attachment{Name: "Pricing deck", URL: "https://docs.google.com/presentation/d/1x"}) {
		t.Fatalf("call %q, files %+v", series.Conference, series.Attachments)
	}
	if len(series.Alarms) != 1 || series.Alarms[0].Before != 10*time.Minute || series.Alarms[0].Email {
		t.Fatalf("alarms %+v", series.Alarms)
	}
	// 27 Oct is after the clock change: 09:15 in London is 09:15 UTC.
	if moved.RecurrenceID == nil || !moved.RecurrenceID.Equal(time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)) || moved.UID != series.UID {
		t.Fatalf("moved instance %+v", moved)
	}
	if holiday.StartDate != "2026-12-24" || holiday.EndDate != "2026-12-27" || !holiday.Transparent || holiday.Zone != "" {
		t.Fatalf("all-day %+v", holiday)
	}
}

// Outlook writes Windows zone names and LF line ends; Apple quotes zones and
// Mozilla puts a path before them; a floating time takes the calendar's zone.
func TestReadsZonesAsOtherCalendarsWriteThem(t *testing.T) {
	file := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"X-WR-TIMEZONE:Asia/Kolkata",
		"BEGIN:VEVENT", "UID:a", "SUMMARY:Outlook", "DTSTART;TZID=W. Europe Standard Time:20260115T100000", "DTEND;TZID=W. Europe Standard Time:20260115T110000", "END:VEVENT",
		"BEGIN:VEVENT", "UID:b", "SUMMARY:Apple", `DTSTART;TZID="America/New_York":20260115T100000`, "DURATION:PT1H30M", "END:VEVENT",
		"BEGIN:VEVENT", "UID:c", "SUMMARY:Mozilla", "DTSTART;TZID=/mozilla.org/20050126_1/Asia/Tokyo:20260115T100000", "END:VEVENT",
		"BEGIN:VEVENT", "UID:d", "SUMMARY:Floating", "DTSTART:20260115T100000", "DTEND:20260115T103000", "END:VEVENT",
		"END:VCALENDAR", ""}, "\n")
	cal, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Events) != 4 {
		t.Fatalf("events %d, skipped %v", len(cal.Events), cal.Skipped)
	}
	want := []struct {
		zone  string
		start time.Time
		end   time.Time
	}{
		{"Europe/Berlin", time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC), time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)},
		{"America/New_York", time.Date(2026, 1, 15, 15, 0, 0, 0, time.UTC), time.Date(2026, 1, 15, 16, 30, 0, 0, time.UTC)},
		{"Asia/Tokyo", time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC), time.Date(2026, 1, 15, 1, 0, 0, 0, time.UTC)},
		{"Asia/Kolkata", time.Date(2026, 1, 15, 4, 30, 0, 0, time.UTC), time.Date(2026, 1, 15, 5, 0, 0, 0, time.UTC)},
	}
	for i, w := range want {
		ev := cal.Events[i]
		if ev.Zone != w.zone || !ev.Start.Equal(w.start) || !ev.End.Equal(w.end) {
			t.Errorf("%s: %s %v–%v, want %s %v–%v", ev.Summary, ev.Zone, ev.Start, ev.End, w.zone, w.start, w.end)
		}
	}
}

// One bad event is left out with its reason in words; the rest are read.
func TestLeavesOutWhatItCantReadAndSaysWhy(t *testing.T) {
	file := strings.Join([]string{
		"BEGIN:VCALENDAR",
		"BEGIN:VEVENT", "UID:ok", "SUMMARY:Fine", "DTSTART:20260115T100000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:nozone", "SUMMARY:Somewhere", "DTSTART;TZID=Mars/Olympus:20260115T100000", "END:VEVENT",
		"BEGIN:VEVENT", "SUMMARY:No id", "DTSTART:20260115T100000Z", "END:VEVENT",
		"BEGIN:VEVENT", "UID:backwards", "SUMMARY:Backwards", "DTSTART:20260115T100000Z", "DTEND:20260115T090000Z", "END:VEVENT",
		"BEGIN:VTODO", "UID:todo", "SUMMARY:Buy milk", "END:VTODO",
		"END:VCALENDAR", ""}, "\r\n")
	cal, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Events) != 1 || cal.Events[0].UID != "ok" {
		t.Fatalf("events %+v", cal.Events)
	}
	var whys []string
	for _, s := range cal.Skipped {
		whys = append(whys, s.What+": "+s.Why)
	}
	got := strings.Join(whys, "\n")
	for _, want := range []string{`the time zone "Mars/Olympus" isn't known`, "No id on", "It has no UID", "Backwards on 15 Jan 2026: It ends before it starts.", "Buy milk: Only events come across, not to-dos."} {
		if !strings.Contains(got, want) {
			t.Errorf("skipped %q, want it to say %q", got, want)
		}
	}
	if _, err := Read(strings.NewReader("hello")); err == nil {
		t.Fatal("a file that isn't a calendar was read")
	}
}

// What the writer writes, the reader reads back the same (G-ICS): a series in
// its zone with skipped, added and changed instances, all-day dates, guests
// and alarms.
func TestWhatIsWrittenReadsBackTheSame(t *testing.T) {
	stamp := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	moved := time.Date(2026, 10, 27, 9, 15, 0, 0, time.UTC)
	evs := []Event{
		{UID: "s1", Sequence: 4, Stamp: stamp, Start: time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC), End: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), Zone: "Europe/London",
			RRule: "FREQ=WEEKLY;BYDAY=TU;COUNT=10", ExDates: []time.Time{time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)}, RDates: []time.Time{time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC)},
			Summary: "Review; a, b\nc", Description: "Line one\nLine two", Location: "Room 4", Status: "CONFIRMED",
			Organizer:  Address{Name: "Jonas", Email: "jonas@example.com"},
			Attendees:  []Attendee{{Address: Address{Name: "Maya", Email: "maya@example.com"}, PartStat: Accepted}, {Address: Address{Name: "Sam", Email: "sam@example.com"}, PartStat: NeedsAction, Optional: true, RSVP: true}},
			Alarms:     []Alarm{{Before: 10 * time.Minute}, {Before: 24 * time.Hour, Email: true}},
			Conference: "https://meet.google.com/abc-defg-hij", Attachments: []Attachment{{Name: "Deck; v2", URL: "https://example.com/deck"}, {URL: "https://example.com/notes"}}},
		{UID: "s1", Sequence: 5, Stamp: stamp, Start: time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 28, 14, 45, 0, 0, time.UTC), Zone: "Europe/London",
			RecurrenceID: &moved, Summary: "Review (moved)", Status: "CONFIRMED", Organizer: Address{Name: "Jonas", Email: "jonas@example.com"}},
		{UID: "h1", Stamp: stamp, StartDate: "2026-12-24", EndDate: "2026-12-27", Summary: "Office closed", Transparent: true, Status: "CONFIRMED", Organizer: Address{Email: "jonas@example.com"}},
	}
	out := WriteMessage(Message{Method: "PUBLISH", Events: evs})
	cal, err := Read(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(cal.Skipped) != 0 || len(cal.Events) != len(evs) {
		t.Fatalf("read back %d, skipped %v\n%s", len(cal.Events), cal.Skipped, out)
	}
	for i := range evs {
		got, want := cal.Events[i], evs[i]
		got.Stamp, want.Stamp = got.Stamp.UTC(), want.Stamp.UTC()
		if !reflect.DeepEqual(norm(got), norm(want)) {
			t.Errorf("event %d read back as\n%+v\nwant\n%+v", i, norm(got), norm(want))
		}
	}
}

// norm makes times comparable with DeepEqual.
func norm(ev Event) Event {
	u := func(ts []time.Time) []time.Time {
		var out []time.Time
		for _, t := range ts {
			out = append(out, t.UTC())
		}
		return out
	}
	ev.Start, ev.End = ev.Start.UTC(), ev.End.UTC()
	ev.ExDates, ev.RDates = u(ev.ExDates), u(ev.RDates)
	if ev.RecurrenceID != nil {
		r := ev.RecurrenceID.UTC()
		ev.RecurrenceID = &r
	}
	return ev
}

func TestReadsDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"PT1H30M": 90 * time.Minute, "P1D": 24 * time.Hour, "-PT15M": -15 * time.Minute, "P2W": 14 * 24 * time.Hour, "P0DT0H10M0S": 10 * time.Minute, "PT0S": 0} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "P", "1H", "PT1X", "PT"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("%q read as a duration", bad)
		}
	}
}

// Google sends a series' recurrence apart from its event.
func TestReadsARecurrenceSentApart(t *testing.T) {
	rule, ex, r := ReadRecurrence([]string{"RRULE:FREQ=WEEKLY;BYDAY=TU", "EXDATE;TZID=Europe/London:20261020T091500,20261103T091500", "RDATE:20261231T100000Z", "nonsense"}, "Europe/London")
	if rule != "FREQ=WEEKLY;BYDAY=TU" || len(ex) != 2 || !ex[0].Equal(time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)) || !ex[1].Equal(time.Date(2026, 11, 3, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("%q %v", rule, ex)
	}
	if len(r) != 1 || !r[0].Equal(time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("rdates %v", r)
	}
}

// A recurrence written apart reads back as it was.
func TestARecurrenceWrittenApartReadsBack(t *testing.T) {
	ev := Event{Start: time.Date(2026, 10, 6, 8, 15, 0, 0, time.UTC), Zone: "Europe/London", RRule: "FREQ=WEEKLY;BYDAY=TU",
		ExDates: []time.Time{time.Date(2026, 11, 3, 9, 15, 0, 0, time.UTC), time.Date(2026, 10, 20, 8, 15, 0, 0, time.UTC)}}
	lines := RecurrenceLines(ev)
	if len(lines) != 2 || lines[1] != "EXDATE;TZID=Europe/London:20261020T091500,20261103T091500" {
		t.Fatalf("lines %q", lines)
	}
	rule, ex, _ := ReadRecurrence(lines, "")
	if rule != ev.RRule || len(ex) != 2 || !ex[0].Equal(ev.ExDates[1]) {
		t.Fatalf("%q %v", rule, ex)
	}
	if RecurrenceLines(Event{}) != nil {
		t.Fatal("a single event has a recurrence")
	}
}

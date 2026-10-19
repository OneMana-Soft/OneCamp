package ICS

import (
	"strings"
	"testing"
	"time"
)

// Invitations to events (calendar A2): guests with their answers, all-day
// dates, and repeating events in their own zone with that zone's VTIMEZONE.

func unfold(s string) string { return strings.ReplaceAll(s, "\r\n ", "") }

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("zone %s: %v", name, err)
	}
	return loc
}

func hasLines(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, "\r\n"+w+"\r\n") {
			t.Errorf("no line %q in\n%s", w, got)
		}
	}
}

func TestAnInvitationAsksEachGuestForAnAnswer(t *testing.T) {
	start := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	got := unfold(string(WriteMessage(Message{Method: Request, Events: []Event{{
		UID: "e1@onecamp", Sequence: 2, Stamp: start.Add(-time.Hour), Start: start, End: start.Add(time.Hour),
		Summary: "Design review", Location: "Room 4; second floor",
		Organizer: Address{Name: "Maya Chen", Email: "maya@acme.example"},
		Attendees: []Attendee{
			{Address: Address{Name: "Alex Kim", Email: "alex@example.org"}, RSVP: true},
			{Address: Address{Name: "Jo Park", Email: "jo@example.org"}, Optional: true, PartStat: Tentative},
		},
	}}})))
	hasLines(t, got,
		"METHOD:REQUEST", "DTSTART:20261015T090000Z", "DTEND:20261015T100000Z", `LOCATION:Room 4\; second floor`,
		`ATTENDEE;CN="Alex Kim";ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:alex@example.org`,
		`ATTENDEE;CN="Jo Park";ROLE=OPT-PARTICIPANT;PARTSTAT=TENTATIVE;RSVP=FALSE:mailto:jo@example.org`,
		"STATUS:CONFIRMED")
	if strings.Contains(got, "VTIMEZONE") {
		t.Error("an event at instants needs no time zone")
	}
}

func TestAnAllDayEventIsItsDates(t *testing.T) {
	got := unfold(string(Write(Request, Event{UID: "d@onecamp", Stamp: time.Now(), StartDate: "2026-11-02", EndDate: "2026-11-04",
		Summary: "Offsite", Organizer: Address{Email: "maya@acme.example"}})))
	hasLines(t, got, "DTSTART;VALUE=DATE:20261102", "DTEND;VALUE=DATE:20261104")
	if strings.Contains(got, "VTIMEZONE") || strings.Contains(got, "T000000") {
		t.Errorf("an all-day event floats: no zone and no times\n%s", got)
	}
}

// A weekly 09:15 in London stays 09:15 after the clocks go back: its times
// are said in its zone, which travels with it.
func TestARepeatingEventKeepsItsZoneAndChangedInstances(t *testing.T) {
	london := mustZone(t, "Europe/London")
	start := time.Date(2026, 10, 13, 9, 15, 0, 0, london)
	skipped := time.Date(2026, 10, 20, 9, 15, 0, 0, london)
	moved := time.Date(2026, 10, 27, 9, 15, 0, 0, london) // after the clocks change
	got := unfold(string(WriteMessage(Message{Method: Request, Events: []Event{
		{UID: "s@onecamp", Sequence: 4, Stamp: start, Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London",
			RRule: "FREQ=WEEKLY;BYDAY=TU;UNTIL=20261222T091500Z", ExDates: []time.Time{skipped},
			Summary: "Standup", Organizer: Address{Email: "maya@acme.example"}},
		{UID: "s@onecamp", Sequence: 4, Stamp: start, RecurrenceID: &moved, Start: moved.Add(time.Hour), End: moved.Add(75 * time.Minute),
			Zone: "Europe/London", Summary: "Standup (later)", Organizer: Address{Email: "maya@acme.example"}},
	}})))
	hasLines(t, got,
		"BEGIN:VTIMEZONE", "TZID:Europe/London",
		"DTSTART;TZID=Europe/London:20261013T091500", "DTEND;TZID=Europe/London:20261013T093000",
		"RRULE:FREQ=WEEKLY;BYDAY=TU;UNTIL=20261222T091500Z",
		"EXDATE;TZID=Europe/London:20261020T091500",
		"RECURRENCE-ID;TZID=Europe/London:20261027T091500",
		"DTSTART;TZID=Europe/London:20261027T101500")
	if n := strings.Count(got, "BEGIN:VTIMEZONE"); n != 1 {
		t.Errorf("one zone, %d VTIMEZONEs", n)
	}
	if n := strings.Count(got, "BEGIN:VEVENT"); n != 2 {
		t.Errorf("the series and its changed instance are 2 VEVENTs, not %d", n)
	}
}

func TestAnAllDaySeriesSkipsDates(t *testing.T) {
	skip := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	got := unfold(string(Write(Request, Event{UID: "a@onecamp", Stamp: skip, StartDate: "2026-12-04", EndDate: "2026-12-05",
		RRule: "FREQ=WEEKLY;BYDAY=FR", ExDates: []time.Time{skip}, Summary: "No-meeting Friday", Organizer: Address{Email: "m@x.example"}})))
	hasLines(t, got, "DTSTART;VALUE=DATE:20261204", "EXDATE;VALUE=DATE:20261225")
}

func TestOneInstanceIsCancelledByItsRecurrenceID(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	rid := time.Date(2026, 11, 3, 10, 0, 0, 0, ny)
	got := unfold(string(Write(Cancel, Event{UID: "s@onecamp", Sequence: 5, Stamp: rid, RecurrenceID: &rid, Start: rid, End: rid.Add(time.Hour),
		Zone: "America/New_York", Summary: "Sync", Organizer: Address{Email: "m@x.example"}})))
	hasLines(t, got, "METHOD:CANCEL", "RECURRENCE-ID;TZID=America/New_York:20261103T100000", "STATUS:CANCELLED")
}

// vtimezoneOf is the zone's lines for events in 2026 to 2027.
func vtimezoneOf(t *testing.T, name string) string {
	loc := mustZone(t, name)
	return strings.Join(vtimezone(loc, time.Date(2026, 3, 1, 0, 0, 0, 0, loc), time.Date(2027, 6, 1, 0, 0, 0, 0, loc)), "\r\n") + "\r\n"
}

func TestAZonesYearlyChangesAreWrittenAsRules(t *testing.T) {
	cases := []struct {
		zone string
		want []string
	}{
		{"Europe/London", []string{"BEGIN:DAYLIGHT", "TZOFFSETFROM:+0000", "TZOFFSETTO:+0100", "RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU", "TZNAME:BST",
			"RRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU", "TZNAME:GMT"}},
		{"America/New_York", []string{"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU", "RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU", "TZOFFSETTO:-0400", "TZOFFSETTO:-0500"}},
		// +12:45 in its winter, +13:45 in its summer.
		{"Pacific/Chatham", []string{"TZOFFSETFROM:+1245", "TZOFFSETTO:+1345", "RRULE:FREQ=YEARLY;BYMONTH=9;BYDAY=-1SU", "RRULE:FREQ=YEARLY;BYMONTH=4;BYDAY=1SU"}},
		{"America/St_Johns", []string{"TZOFFSETFROM:-0330", "TZOFFSETTO:-0230"}},
	}
	for _, c := range cases {
		got := vtimezoneOf(t, c.zone)
		hasLines(t, "\r\n"+got, append([]string{"BEGIN:VTIMEZONE", "TZID:" + c.zone, "END:VTIMEZONE"}, c.want...)...)
		// The onset of a rule is a wall-clock reading: never in UTC.
		for _, l := range strings.Split(got, "\r\n") {
			if strings.HasPrefix(l, "DTSTART:") && strings.HasSuffix(l, "Z") {
				t.Errorf("%s: an observance starts at a wall-clock time, not %q", c.zone, l)
			}
		}
	}
}

func TestAZoneThatNeverChangesIsOneObservance(t *testing.T) {
	got := vtimezoneOf(t, "Asia/Kolkata")
	hasLines(t, "\r\n"+got, "BEGIN:STANDARD", "DTSTART:19700101T000000", "TZOFFSETFROM:+0530", "TZOFFSETTO:+0530")
	if strings.Contains(got, "DAYLIGHT") || strings.Count(got, "BEGIN:STANDARD") != 1 {
		t.Errorf("India keeps one offset:\n%s", got)
	}
}

// The London rule's onsets: 01:00 GMT in March, 02:00 BST in October, each
// read on the clock before the change.
func TestTheOnsetIsTheClockBeforeTheChange(t *testing.T) {
	got := vtimezoneOf(t, "Europe/London")
	hasLines(t, "\r\n"+got, "DTSTART:20250330T010000", "DTSTART:20251026T020000")
}

func TestUTCOffsets(t *testing.T) {
	for secs, want := range map[int]string{19800: "+0530", -12600: "-0330", 0: "+0000", 45900: "+1245", -18000: "-0500", 3661: "+010101"} {
		if got := utcOffset(secs); got != want {
			t.Errorf("%d: %q, want %q", secs, got, want)
		}
	}
}

// A zone's name goes in a parameter unquoted: nothing in it can end it.
func TestAZoneNameCantChangeTheStructure(t *testing.T) {
	if got := paramText(`Bad;Zone:"x",y` + "\r\n"); got != "BadZonexy" {
		t.Errorf("paramText %q", got)
	}
}

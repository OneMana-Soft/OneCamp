package ICS

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The first fixtures of the calendar's codec, from booking pages' invites
// (CAL-BUG-15), where this writer began.

func anEvent() Event {
	start := time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC)
	return Event{UID: "6f1c2a52@onecamp", Sequence: 3, Stamp: time.Date(2026, 10, 13, 9, 15, 0, 0, time.UTC),
		Start: start, End: start.Add(30 * time.Minute), Summary: "Intro call with Sam Rivera",
		Organizer: Address{Name: "Sam Rivera", Email: "hello@acme.example"},
		Attendee:  Address{Name: "Alex Kim", Email: "alex@example.org"}}
}

func TestAnEventIsWrittenWhole(t *testing.T) {
	want := strings.Join([]string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//OneCamp//Calendar//EN", "CALSCALE:GREGORIAN", "METHOD:REQUEST",
		"BEGIN:VEVENT", "UID:6f1c2a52@onecamp", "SEQUENCE:3", "DTSTAMP:20261013T091500Z",
		"DTSTART:20261014T043000Z", "DTEND:20261014T050000Z", "SUMMARY:Intro call with Sam Rivera",
		`ORGANIZER;CN="Sam Rivera":mailto:hello@acme.example`,
		`ATTENDEE;CN="Alex Kim";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:ma`, // folded at 75 octets
		` ilto:alex@example.org`,
		"STATUS:CONFIRMED", "TRANSP:OPAQUE", "END:VEVENT", "END:VCALENDAR", "",
	}, "\r\n")
	if got := string(Write(Request, anEvent())); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
	if got := string(Write(Cancel, anEvent())); !strings.Contains(got, "\r\nMETHOD:CANCEL\r\n") || !strings.Contains(got, "\r\nSTATUS:CANCELLED\r\n") {
		t.Errorf("a cancel:\n%s", got)
	}
}

func TestFold(t *testing.T) {
	short := strings.Repeat("a", 75)
	if fold(short) != short {
		t.Error("a 75-octet line is not folded")
	}
	long := "DESCRIPTION:" + strings.Repeat("é", 100)
	folded := fold(long)
	if strings.ReplaceAll(folded, "\r\n ", "") != long {
		t.Error("unfolding doesn't restore the line")
	}
	for i, part := range strings.Split(folded, "\r\n") {
		if len(part) > 75 || !utf8.ValidString(strings.TrimPrefix(part, " ")) || (i > 0 && !strings.HasPrefix(part, " ")) {
			t.Errorf("part %d: %q", i, part)
		}
	}
}

func TestTextAndParamsCantChangeTheStructure(t *testing.T) {
	if got := text("a;b,c\\d\r\ne\nf"); got != `a\;b\,c\\d\ne\nf` {
		t.Errorf("text %q", got)
	}
	if got := param("Kim, \"Alex\"\r\nX"); got != `"Kim, AlexX"` {
		t.Errorf("param %q", got)
	}
}

// The sending address may be "Acme <hi@acme.com>" (Admin > Email, or
// SENDER_EMAIL), which made ORGANIZER "mailto:Acme <hi@acme.com>", not a
// cal-address, and a calendar may drop the invite. Only the address is
// written.
func TestACalAddressIsTheAddressAlone(t *testing.T) {
	ev := anEvent()
	ev.Organizer.Email = "Acme Support <hi@acme.example>"
	ev.Attendee.Email = "\"Kim, Alex\" <alex@example.org>"
	got := strings.ReplaceAll(string(Write(Request, ev)), "\r\n ", "")
	for _, want := range []string{`ORGANIZER;CN="Sam Rivera":mailto:hi@acme.example` + "\r\n", ":mailto:alex@example.org\r\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in\n%s", want, got)
		}
	}
}

package Calendar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

func aBookingAt(start time.Time) *bookingModel.Booking {
	return &bookingModel.Booking{Id: uuid.MustParse("6f1c2a52-6a2e-4a4a-9a51-7b0e2c3d4e5f"), GuestName: "Alex Kim",
		GuestEmail: "alex@example.org", StartsAt: start, EndsAt: start.Add(30 * time.Minute), CreatedAt: start.Add(-48 * time.Hour)}
}

// unfold undoes RFC 5545 folding and splits the content lines.
func unfold(t *testing.T, ics []byte) []string {
	t.Helper()
	s := string(ics)
	if !strings.HasSuffix(s, "\r\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Fatalf("lines must end CRLF:\n%q", s)
	}
	for _, l := range strings.Split(strings.TrimSuffix(s, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("a line of %d octets: %q", len(l), l)
		}
		if !utf8.ValidString(l) {
			t.Errorf("a fold split a character: %q", l)
		}
	}
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(s, "\r\n ", ""), "\r\n"), "\r\n")
}

func has(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, l := range lines {
			if l == w {
				found = true
			}
		}
		if !found {
			t.Errorf("no line %q in\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

// CAL-BUG-15: the confirmation promised an invite the email didn't have. The
// email now carries one Gmail and Outlook take: a REQUEST, with the UID the
// page's own .ics uses.
func TestABookingsInviteIsARequestCalendarsTake(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 15, 0, 0, time.UTC)
	b := aBookingAt(time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC))
	iv := bookingInvite(inviteRequest, b, "Intro call", "Sam Rivera", true, now)
	lines := unfold(t, iv.ics())
	if lines[0] != "BEGIN:VCALENDAR" || lines[len(lines)-1] != "END:VCALENDAR" {
		t.Errorf("not a calendar: %v", lines)
	}
	has(t, lines,
		"VERSION:2.0",
		"METHOD:REQUEST",
		"UID:6f1c2a52-6a2e-4a4a-9a51-7b0e2c3d4e5f@onecamp",
		"SEQUENCE:0",
		"DTSTAMP:20261013T091500Z",
		"DTSTART:20261014T043000Z",
		"DTEND:20261014T050000Z",
		"SUMMARY:Intro call with Sam Rivera",
		`ORGANIZER;CN="Sam Rivera":mailto:`+iv.Organizer.Email,
		`ATTENDEE;CN="Alex Kim";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:mailto:alex@example.org`,
		"STATUS:CONFIRMED",
	)
	if !strings.Contains(iv.Organizer.Email, "@") {
		t.Errorf("organizer %q", iv.Organizer.Email)
	}
	a := iv.attachment()
	if a.Filename != "invite.ics" || a.ContentType != "text/calendar; charset=utf-8; method=REQUEST" || string(a.Content) != string(iv.ics()) {
		t.Errorf("attachment %+v", a)
	}
}

// A cancel names the same event, newer than anything sent before.
func TestABookingsCancelIsNewerThanItsRequest(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 15, 0, 0, time.UTC)
	b := aBookingAt(time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC))
	lines := unfold(t, bookingInvite(inviteCancel, b, "Intro call", "Sam Rivera", false, now).ics())
	has(t, lines, "METHOD:CANCEL", "STATUS:CANCELLED", "UID:6f1c2a52-6a2e-4a4a-9a51-7b0e2c3d4e5f@onecamp",
		"SEQUENCE:103500") // 28 h 45 min after it was made
	later := bookingInvite(inviteRequest, b, "Intro call", "Sam Rivera", false, now.Add(time.Minute))
	if later.Sequence <= 103500 {
		t.Errorf("a later invite's sequence %d isn't newer", later.Sequence)
	}
	b.CreatedAt = time.Time{}
	if s := bookingInvite(inviteCancel, b, "Intro call", "Sam Rivera", false, now).Sequence; s != 1 {
		t.Errorf("with no time made, a later invite is 1, got %d", s)
	}
}

// Names are text, never syntax: commas, semicolons, quotes and line breaks
// can't change the invite, and a long line folds between characters.
func TestAnInviteKeepsWhatPeopleTypeAsText(t *testing.T) {
	now := time.Date(2026, 10, 13, 9, 15, 0, 0, time.UTC)
	b := aBookingAt(time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC))
	b.GuestName = "Kim, \"Alex\"\r\nATTENDEE:mailto:evil@example.org"
	title := "Intro; call, über-long " + strings.Repeat("ünïcödé ", 12)
	iv := bookingInvite(inviteRequest, b, title, "Sam \"The\" Rivera", true, now)
	lines := unfold(t, iv.ics())
	has(t, lines,
		`SUMMARY:`+strings.NewReplacer(";", `\;`, ",", `\,`).Replace(title)+` with Sam "The" Rivera`,
		`ORGANIZER;CN="Sam The Rivera":mailto:`+iv.Organizer.Email,
		`ATTENDEE;CN="Kim, AlexATTENDEE:mailto:evil@example.org";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=FALSE:mailto:alex@example.org`,
	)
	attendees := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "ATTENDEE") {
			attendees++
		}
	}
	if attendees != 1 {
		t.Errorf("%d attendees", attendees)
	}
}

// The guest's cancel link is their credential; a calendar event is shared
// far more freely than an email, so the invite never carries it.
func TestAnInviteNeverCarriesTheCancelLink(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef"
	b := aBookingAt(time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC))
	for _, m := range []string{inviteRequest, inviteCancel} {
		ics := string(bookingInvite(m, b, "Intro call", "Sam Rivera", m == inviteRequest, time.Now()).ics())
		if strings.Contains(ics, token) || strings.Contains(ics, "/booking/") {
			t.Errorf("%s carries the link:\n%s", m, ics)
		}
	}
}

// Every email to a guest carries the invite, so their calendar follows it.
func TestMailToAGuestCarriesTheInvite(t *testing.T) {
	oldClient := emailService.SharedHTTPClient
	t.Cleanup(func() {
		emailService.SharedHTTPClient = oldClient
		emailService.UseAPIKeyFrom(func() string { return os.Getenv("RESEND_API_KEY") })
	})
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	emailService.UseAPIKeyFrom(func() string { return "re_test" })
	var sent struct {
		To          []string `json:"to"`
		Attachments []struct {
			Filename    string `json:"filename"`
			Content     string `json:"content"`
			ContentType string `json:"content_type"`
		} `json:"attachments"`
	}
	emailService.SharedHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Errorf("payload %s: %v", raw, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}

	b := aBookingAt(time.Date(2026, 10, 14, 4, 30, 0, 0, time.UTC))
	b.GuestEmail = "alex.kim@outlook.com"
	if !mailGuest(context.Background(), guestMail{to: b.GuestEmail, name: b.GuestName, subject: "Booked", title: "You're booked",
		body: "Wednesday", invite: bookingInvite(inviteRequest, b, "Intro call", "Sam Rivera", true, time.Now())}) {
		t.Fatal("not sent")
	}
	if len(sent.To) != 1 || sent.To[0] != "alex.kim@outlook.com" || len(sent.Attachments) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	a := sent.Attachments[0]
	ics, _ := base64.StdEncoding.DecodeString(a.Content)
	if a.Filename != "invite.ics" || a.ContentType != "text/calendar; charset=utf-8; method=REQUEST" ||
		!strings.Contains(string(ics), "\r\nMETHOD:REQUEST\r\n") || !strings.Contains(string(ics), "UID:"+b.Id.String()+"@onecamp") {
		t.Errorf("attachment %s %s:\n%s", a.Filename, a.ContentType, ics)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

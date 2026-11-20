package Calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

func TestNormaliseSlug(t *testing.T) {
	for in, want := range map[string]string{
		"  Akash's 30-min Chat! ": "akash-s-30-min-chat",
		"intro--call":             "intro-call",
		"---":                     "",
	} {
		if got := NormaliseSlug(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestCheckPage(t *testing.T) {
	good := PageInput{Slug: "Intro Call", Title: " Intro  call ", DurationMinutes: 30, MaxDaysAhead: 30,
		MinNoticeMinutes: 240, Hours: WorkingHours{Days: []int{1, 2}, Start: "09:00", End: "17:00", TZ: "Asia/Kolkata"}, Active: true}
	now := time.Now()
	p, err := CheckPage(good, now)
	if err != nil || p.Slug != "intro-call" || p.Title != "Intro call" || !strings.Contains(string(p.Hours), "Asia/Kolkata") {
		t.Fatalf("good page: %+v %v", p, err)
	}
	bad := []func(*PageInput){
		func(p *PageInput) { p.Slug = "ab" },
		func(p *PageInput) { p.Title = "" },
		func(p *PageInput) { p.DurationMinutes = 5 },
		func(p *PageInput) { p.BufferMinutes = -1 },
		func(p *PageInput) { p.MaxDaysAhead = 0 },
		func(p *PageInput) { p.Hours.TZ = "" },
		func(p *PageInput) { p.Id = "not-a-uuid" },
	}
	for i, mutate := range bad {
		in := good
		mutate(&in)
		if _, err := CheckPage(in, now); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestCheckGuest(t *testing.T) {
	g, err := CheckGuest(BookInput{Name: "  Priya   Shah ", Email: "Priya <priya@example.com>", TZ: "Mars/Base"})
	if err != nil || g.Name != "Priya Shah" || g.Email != "priya@example.com" || g.TZ != "UTC" {
		t.Fatalf("got %+v %v", g, err)
	}
	// "Local" is the server's own zone and nobody's: kept, every email about
	// the booking would say its times in the server's zone, "(Local)".
	for tz, want := range map[string]string{"Local": "UTC", "": "UTC", "Asia/Kolkata": "Asia/Kolkata"} {
		if g, err := CheckGuest(BookInput{Name: "Priya", Email: "priya@example.com", TZ: tz}); err != nil || g.TZ != want {
			t.Errorf("zone %q kept as %q (%v), want %q", tz, g.TZ, err, want)
		}
	}
	for _, in := range []BookInput{{Name: "", Email: "a@b.co"}, {Name: "A", Email: "nope"}, {Name: "A", Email: "a@localhost"},
		{Name: "A", Email: "a@b.co", Note: strings.Repeat("x", 1001)}} {
		if _, err := CheckGuest(in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	// What a guest writes is text: a hidden character (a NUL Postgres
	// refuses, an escape, a reversed run) is a sentence, never a 500.
	for want, in := range map[string]BookInput{
		"Remove the hidden characters from your note.": {Name: "A", Email: "a@b.co", Note: "see\x00you"},
		"Remove the hidden characters from your note":  {Name: "A", Email: "a@b.co", Note: "see \u202Eyou"},
		"Remove the hidden characters from your name.": {Name: "A\x1b[2J", Email: "a@b.co"},
		"A page asks up to 10 questions":               {Name: "A", Email: "a@b.co", Answers: make([]GuestAnswer, 11)},
	} {
		if _, err := CheckGuest(in); !refusedWith(err, want) {
			t.Errorf("%+v: err %v, want %q", in, err, want)
		}
	}
	if g, err := CheckGuest(BookInput{Name: "A", Email: "a@b.co", Note: " one\r\ntwo\tthree "}); err != nil || g.Note != "one\ntwo\tthree" {
		t.Errorf("a note's lines: %q %v", g.Note, err)
	}
}

// A booking's guest reads its times in the zone they booked in, or the
// page's when the booking kept none; never the server's own ("Local").
func TestGuestZone(t *testing.T) {
	page := &bookingModel.Page{Hours: []byte(`{"tz":"Europe/Berlin"}`)}
	for kept, want := range map[string]string{"Asia/Kolkata": "Asia/Kolkata", "": "Europe/Berlin", "Local": "Europe/Berlin", "Mars/Base": "Europe/Berlin"} {
		if got := guestZone(&bookingModel.Booking{GuestTZ: kept}, page); got != want {
			t.Errorf("a booking kept in %q is read in %q, want %q", kept, got, want)
		}
	}
}

func TestFormatWhen(t *testing.T) {
	s := time.Date(2026, 10, 6, 4, 30, 0, 0, time.UTC)
	if got := FormatWhen(s, s.Add(30*time.Minute), "Asia/Kolkata"); got != "Tuesday 6 October 2026, 10:00–10:30 (Asia/Kolkata)" {
		t.Fatalf("got %q", got)
	}
}

// hoursOf reads hours as the editor sends them.
func hoursOf(t *testing.T, raw string) WorkingHours {
	t.Helper()
	var h WorkingHours
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return h
}

// A page keeps each weekday's own hours, and dates of their own: a day off
// ([]) or other hours that day, from today in the page's zone on (gcal-7).
func TestCheckPageKeepsItsWeekAndDates(t *testing.T) {
	// 00:30 on 17 Nov in London.
	now := time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC)
	check := func(in PageInput) (bookingModel.Page, error) { return CheckPage(in, now) }
	page := func(dates string) PageInput {
		return PageInput{Slug: "office-hours", Title: "Office hours", DurationMinutes: 30, MaxDaysAhead: 60, Active: true,
			Hours: hoursOf(t, `{"tz":"Europe/London","week":{"2":[{"start":"09:00","end":"12:00"}],"4":[{"start":"14:00","end":"18:00"}]},"dates":{`+dates+`}}`)}
	}
	p, err := check(page(`"2026-11-17":[{"start":"15:00","end":"16:00"}],"2026-12-01":[],"2026-12-05":[{"start":"13:00","end":"14:00"},{"start":"10:00","end":"12:00"}]`))
	if err != nil {
		t.Fatalf("a week and dates: %v", err)
	}
	var kept WorkingHours
	_ = json.Unmarshal(p.Hours, &kept)
	if fmt.Sprint(kept.Week) != "map[2:[{09:00 12:00}] 4:[{14:00 18:00}]]" {
		t.Errorf("week kept as %v", kept.Week)
	}
	if !strings.Contains(string(p.Hours), `"dates":{"2026-11-17":[{"start":"15:00","end":"16:00"}],"2026-12-01":[],"2026-12-05":[`) {
		t.Errorf("dates kept as %s", p.Hours)
	}
	var sixty, sixtyOne []string
	for i := 0; i <= 60; i++ {
		d := fmt.Sprintf(`%q:[]`, now.AddDate(0, 0, i).Format("2006-01-02"))
		if i < 60 {
			sixty = append(sixty, d)
		}
		sixtyOne = append(sixtyOne, d)
	}
	if _, err := check(page(strings.Join(sixty, ","))); err != nil {
		t.Errorf("sixty dates: %v", err)
	}
	for name, dates := range map[string]string{
		"a date not written YYYY-MM-DD":   `"2026-12-1":[]`,
		"a date that isn't one":           `"next tuesday":[]`,
		"no such day":                     `"2026-02-30":[]`,
		"a range ending before it starts": `"2026-12-05":[{"start":"12:00","end":"10:00"}]`,
		"a time that isn't one":           `"2026-12-05":[{"start":"9am","end":"10:00"}]`,
		"ranges that overlap":             `"2026-12-05":[{"start":"10:00","end":"12:00"},{"start":"11:00","end":"13:00"}]`,
		"a date already past":             `"2026-11-16":[]`,
		"more than sixty dates":           strings.Join(sixtyOne, ","),
	} {
		if _, err := check(page(dates)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A visitor types any address into the public demo's booking page, and the
// demo mails nobody outside the workspace (outsidersMailed): with notification
// email on, a demo server mails a booking's guest nothing, booked or cancelled
// (Book then answers Emailed false, and the page shows the booking), and the
// same server off the demo mails each. Every guest mail goes through
// mailGuest: Book, the guest's cancel, the owner's move or cancel. The mail is
// caught at the email client, past every rule of the server's own, so the
// guest is on a domain that receives mail (one reserved for examples is
// never sent to at all, demo or not).
func TestTheDemoNeverMailsABookingGuest(t *testing.T) {
	oldClient := emailService.SharedHTTPClient
	t.Cleanup(func() {
		emailService.SharedHTTPClient = oldClient
		emailService.UseAPIKeyFrom(func() string { return os.Getenv("RESEND_API_KEY") })
	})
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	emailService.UseAPIKeyFrom(func() string { return "re_not_a_real_key" })
	var mail []string // each sent mail's recipient and subject
	emailService.SharedHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var sent struct {
			To      []string `json:"to"`
			Subject string   `json:"subject"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		mail = append(mail, strings.Join(sent.To, ",")+" "+sent.Subject)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"m1"}`))}, nil
	})}
	now := time.Now()
	start := now.Add(72 * time.Hour).UTC().Truncate(time.Minute)
	b := &bookingModel.Booking{Id: uuid.New(), GuestName: "Alex Kim", GuestEmail: "alex.kim@outlook.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute)}
	toGuest := []guestMail{
		{to: b.GuestEmail, name: b.GuestName, subject: "Booked: Intro call with Sam Rivera",
			invite: bookingInvite(inviteRequest, b, "Intro call", "Sam Rivera", true, now)},
		{to: b.GuestEmail, name: b.GuestName, subject: "Cancelled: Intro call with Sam Rivera",
			invite: bookingInvite(inviteCancel, b, "Intro call", "Sam Rivera", false, now)},
	}
	ctx := context.Background()

	t.Setenv("DEMO_MODE", "true")
	for _, m := range toGuest {
		if mailGuest(ctx, m) {
			t.Errorf("the demo says it mailed %q", m.subject)
		}
	}
	if len(mail) != 0 {
		t.Fatalf("the demo mailed its guest: %q, want nothing", mail)
	}
	t.Setenv("DEMO_MODE", "")
	for _, m := range toGuest {
		if !mailGuest(ctx, m) {
			t.Errorf("off the demo, %q wasn't mailed", m.subject)
		}
	}
	if len(mail) != 2 || mail[0] != "alex.kim@outlook.com Booked: Intro call with Sam Rivera" ||
		mail[1] != "alex.kim@outlook.com Cancelled: Intro call with Sam Rivera" {
		t.Errorf("off the demo: %q, want the booked one and the cancelled one", mail)
	}
}

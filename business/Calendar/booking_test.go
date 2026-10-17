package Calendar

import (
	"strings"
	"testing"
	"time"

	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
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
	p, err := CheckPage(good)
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
		if _, err := CheckPage(in); err == nil {
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

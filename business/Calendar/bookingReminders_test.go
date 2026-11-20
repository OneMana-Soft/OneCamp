package Calendar

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

// A guest is reminded a day and an hour before their booking: only of a time
// still to come, and never sooner than guestReminderFloor from the booking,
// whatever the page's notice (that is for booking, not for reminding).
func TestAGuestIsRemindedADayAndAnHourBefore(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	owner := uuid.New()
	page := &bookingModel.Page{Id: uuid.New(), UserId: owner}
	noticed := &bookingModel.Page{Id: uuid.New(), UserId: owner, MinNoticeMinutes: 6 * 60}
	fourHours := &bookingModel.Page{Id: uuid.New(), UserId: owner, MinNoticeMinutes: 240} // the default notice
	thirtyDays := &bookingModel.Page{Id: uuid.New(), UserId: owner, MinNoticeMinutes: 30 * 24 * 60}
	booking := func(in time.Duration) *bookingModel.Booking {
		return &bookingModel.Booking{Id: uuid.New(), PageId: page.Id, StartsAt: now.Add(in), EndsAt: now.Add(in + 30*time.Minute),
			GuestEmail: "alex@example.com", CancelHash: []byte("hash-of-the-link"), CancelSealed: "sealed-link"}
	}
	for name, c := range map[string]struct {
		b      *bookingModel.Booking
		p      *bookingModel.Page
		before []int // minutes
	}{
		"three days out":                       {booking(72 * time.Hour), page, []int{24 * 60, 60}},
		"two hours out":                        {booking(2 * time.Hour), page, []int{60}},
		"half an hour out":                     {booking(30 * time.Minute), page, nil},
		"an hour's reminder inside the floor":  {booking(90 * time.Minute), page, nil},
		"a day's reminder inside the notice":   {booking(26 * time.Hour), noticed, []int{24 * 60, 60}},
		"three days out on a page with notice": {booking(72 * time.Hour), noticed, []int{24 * 60, 60}},
		"the first slot on a 240-min page":     {booking(240 * time.Minute), fourHours, []int{60}},
		"the first slot on a 30-day page":      {booking(30 * 24 * time.Hour), thirtyDays, []int{24 * 60, 60}},
	} {
		jobs := guestReminderJobs(c.b, c.p, now)
		if len(jobs) != len(c.before) {
			t.Errorf("%s: %d jobs, want %d", name, len(jobs), len(c.before))
			continue
		}
		for i, j := range jobs {
			var r guestReminder
			if err := json.Unmarshal([]byte(j.PayloadJSON), &r); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want := c.b.StartsAt.Add(-time.Duration(c.before[i]) * time.Minute)
			if j.JobType != guestReminderJobType || j.UserUUID != owner || !j.RunAt.Equal(want) ||
				r.BookingID != c.b.Id || !r.Start.Equal(c.b.StartsAt) || r.BeforeMinutes != c.before[i] {
				t.Errorf("%s: job %d is %+v %+v", name, i, j, r)
			}
			if strings.Contains(j.PayloadJSON, "sealed-link") || strings.Contains(j.PayloadJSON, "hash") {
				t.Errorf("%s: the payload carries the link: %s", name, j.PayloadJSON)
			}
		}
	}
}

// A reminder goes once, to a live booking at the start it was planned for,
// with the guest's link as its button: never to a cancelled or moved one, and
// never twice for the same booking, start and lead.
func TestAGuestReminderGoesOnceToALiveBookingAtItsPlannedStart(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "the server's key")
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil  // counted in this process, as when Redis is down
	mail := captureMail(t, true) // the server mails outsiders

	raw, hash, sealed, err := helpers.NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Minute)
	page := &bookingModel.Page{Id: uuid.New(), UserId: uuid.New(), Title: "Intro call"}
	live := &bookingModel.Booking{Id: uuid.New(), PageId: page.Id, GuestName: "Alex Kim", GuestEmail: "alex@example.com",
		GuestTZ: "Asia/Kolkata", StartsAt: start, EndsAt: start.Add(30 * time.Minute), CancelHash: hash, CancelSealed: sealed}
	ctx := context.Background()
	dayBefore := guestReminder{BookingID: live.Id, Start: start, BeforeMinutes: 24 * 60}

	if !remindGuest(ctx, live, page, "Sam Rivera", dayBefore, time.Now()) || len(*mail) != 1 {
		t.Fatalf("a live booking at its planned start: %d mails", len(*mail))
	}
	m := (*mail)[0]
	if m.to != "alex@example.com" || !strings.Contains(m.subject, "Intro call") || !strings.Contains(m.text, "Sam Rivera") ||
		!strings.Contains(m.html, "/booking/"+raw) || !strings.Contains(m.text, "(Asia/Kolkata)") {
		t.Errorf("the reminder: to %s, %q, %q", m.to, m.subject, m.text)
	}
	if remindGuest(ctx, live, page, "Sam Rivera", dayBefore, time.Now()) || len(*mail) != 1 {
		t.Errorf("a second firing of the same reminder mailed again: %d", len(*mail))
	}

	cancelledAt := time.Now()
	cancelled := *live
	cancelled.Id, cancelled.CancelledAt = uuid.New(), &cancelledAt
	moved := *live
	moved.Id, moved.StartsAt = uuid.New(), start.Add(2*time.Hour)
	for name, b := range map[string]*bookingModel.Booking{"cancelled": &cancelled, "moved": &moved} {
		if remindGuest(ctx, b, page, "Sam Rivera", guestReminder{BookingID: b.Id, Start: start, BeforeMinutes: 60}, time.Now()) {
			t.Errorf("a %s booking was reminded", name)
		}
	}
	if len(*mail) != 1 {
		t.Errorf("mails after a cancelled and a moved booking's reminders: %d, want 1", len(*mail))
	}
}

// A reminder found late (the server was down, the scheduler caught up) is
// skipped by the members' rule (reminderLate): one more than that past its
// time mails nothing and claims nothing, so the hour's still goes after the
// day's came late; one a few minutes late goes once; none goes from the start.
func TestALateGuestReminderIsNeverSent(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "the server's key")
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil
	mail := captureMail(t, true) // the server mails outsiders
	_, hash, sealed, err := helpers.NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Minute)
	end := start.Add(30 * time.Minute)
	page := &bookingModel.Page{Id: uuid.New(), UserId: uuid.New(), Title: "Intro call"}
	ctx := context.Background()
	booking := func() *bookingModel.Booking {
		return &bookingModel.Booking{Id: uuid.New(), PageId: page.Id, GuestName: "Alex Kim", GuestEmail: "alex@example.com",
			StartsAt: start, EndsAt: end, CancelHash: hash, CancelSealed: sealed}
	}
	want := 0
	remind := func(what string, b *bookingModel.Booking, before time.Duration, now time.Time, goes bool) {
		t.Helper()
		r := guestReminder{BookingID: b.Id, Start: start, BeforeMinutes: int(before / time.Minute)}
		if goes {
			want++
		}
		if sent := remindGuest(ctx, b, page, "Sam Rivera", r, now); sent != goes || len(*mail) != want {
			t.Errorf("%s: sent %v, %d mails, want %v and %d", what, sent, len(*mail), goes, want)
		}
	}

	hour := booking()
	remind("the hour's reminder found 3 h late", hour, time.Hour, start.Add(-time.Hour).Add(3*time.Hour), false)
	remind("the same reminder on time (the late one claimed nothing)", hour, time.Hour, start.Add(-time.Hour), true)

	day := booking()
	remind("the day's reminder found 2 h late", day, 24*time.Hour, start.Add(-24*time.Hour).Add(2*time.Hour), false)
	remind("the hour's after the day's came late", day, time.Hour, start.Add(-time.Hour), true)

	soon := booking()
	remind("the hour's reminder found 5 min late", soon, time.Hour, start.Add(-time.Hour).Add(5*time.Minute), true)
	remind("the same, fired again", soon, time.Hour, start.Add(-time.Hour).Add(6*time.Minute), false)

	// Five minutes before (no lead of ours is that short): found at the
	// start it is only five minutes late, inside reminderLate, so what stops
	// it is the start.
	last := booking()
	remind("a reminder found at the start", last, 5*time.Minute, start, false)
	remind("a reminder found after the start", last, 5*time.Minute, start.Add(time.Minute), false)
	remind("a reminder found after the end", last, 5*time.Minute, end.Add(time.Minute), false)
	remind("the same reminder on time", last, 5*time.Minute, start.Add(-5*time.Minute), true)
}

// The public demo mails nobody outside the workspace (outsidersMailed): a
// visitor's booking on the demo plans no reminder, and a reminder left
// from before fires with no mail and claims nothing.
func TestTheDemoNeverRemindsAGuest(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("APP_SECRET_KEK", "the server's key")
	old := redisInit.RedisClient
	t.Cleanup(func() { redisInit.RedisClient = old })
	redisInit.RedisClient = nil
	// The outsidersMailed seam stays on, though the server's own rule is off
	// in DEMO_MODE (TestTheDemoNeverMailsABookingGuest): so what stops the
	// reminder here is the reminders' own demo gates, in planning and firing.
	mail := captureMail(t, true)

	_, hash, sealed, err := helpers.NewSealedLinkSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(72 * time.Hour)
	page := &bookingModel.Page{Id: uuid.New(), UserId: uuid.New(), Title: "Intro call with Sam"}
	b := &bookingModel.Booking{Id: uuid.New(), PageId: page.Id, GuestName: "Alex Kim", GuestEmail: "alex@example.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), CancelHash: hash, CancelSealed: sealed}
	if jobs := guestReminderJobs(b, page, now); len(jobs) != 0 {
		t.Errorf("the demo planned %d guest reminders, want none", len(jobs))
	}
	r := guestReminder{BookingID: b.Id, Start: start, BeforeMinutes: 60}
	if remindGuest(context.Background(), b, page, "Sam Rivera", r, now) || len(*mail) != 0 {
		t.Errorf("the demo mailed a guest reminder: %d mails", len(*mail))
	}
	t.Setenv("DEMO_MODE", "")
	if !remindGuest(context.Background(), b, page, "Sam Rivera", r, now) || len(*mail) != 1 {
		t.Errorf("off the demo, the same reminder (its claim untouched by the demo's firing): %d mails, want 1", len(*mail))
	}
}

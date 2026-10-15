//go:build integration
// +build integration

package integration_test

// Booking pages against real Postgres 12, Dgraph and Redis, with Resend and
// Firebase stood in for: a booking emails its guest an invite (CAL-BUG-15)
// and tells its owner by email and by push (CAL-BUG-5); the owner moving or
// deleting a booked meeting moves or cancels the booking, tells the guest and
// frees the slot (CAL-BUG-4); a guest cancelling takes the meeting off both
// calendars; and without notification email the owner still gets the push.
// On this edition a page with bookings still to come can't be deleted
// (CAL-BUG-3): switched off, it keeps them and their guests' links.
//
// Run: go test -tags=integration ./tests/integration/ -run TestBookingPages -v

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

// guestMailbox stands in for Resend: it keeps every message it's sent.
type guestMailbox struct {
	mu   sync.Mutex
	msgs []mailedMessage
}

type mailedMessage struct {
	To          []string `json:"to"`
	Subject     string   `json:"subject"`
	HTML        string   `json:"html"`
	Attachments []struct {
		Filename    string `json:"filename"`
		Content     string `json:"content"`
		ContentType string `json:"content_type"`
	} `json:"attachments"`
}

func (m *guestMailbox) RoundTrip(r *http.Request) (*http.Response, error) {
	var msg mailedMessage
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &msg)
	m.mu.Lock()
	m.msgs = append(m.msgs, msg)
	m.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"sent"}`)), Request: r}, nil
}

// to is everything sent to addr, oldest first.
func (m *guestMailbox) to(addr string) []mailedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mailedMessage
	for _, msg := range m.msgs {
		if len(msg.To) == 1 && msg.To[0] == addr {
			out = append(out, msg)
		}
	}
	return out
}

// count is how many messages were sent in all.
func (m *guestMailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

// invite is the message's calendar invite, unfolded, or "".
func (msg mailedMessage) invite() string {
	for _, a := range msg.Attachments {
		if a.Filename == "invite.ics" && strings.HasPrefix(a.ContentType, "text/calendar") {
			raw, _ := base64.StdEncoding.DecodeString(a.Content)
			return strings.ReplaceAll(string(raw), "\r\n ", "")
		}
	}
	return ""
}

// settled waits for background telling to land before a check that nothing
// more was sent.
func settled() { time.Sleep(300 * time.Millisecond) }

// bookingEventually waits up to ten seconds for cond.
func bookingEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("waited for %s", what)
}

func TestBookingPages(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	// Notification email on, sent to a stand-in for Resend.
	t.Setenv("RESEND_API_KEY", "re_booking_pages_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	mailbox := &guestMailbox{}
	oldClient := emailService.SharedHTTPClient
	emailService.SharedHTTPClient = &http.Client{Transport: mailbox}
	t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })

	var pushMu sync.Mutex
	var pushes []map[string]string
	defer firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		if len(tokens) == 1 && tokens[0] == "tok-sam" {
			pushMu.Lock()
			pushes = append(pushes, data)
			pushMu.Unlock()
		}
	})()
	pushedFor := func(kind, event string) bool {
		pushMu.Lock()
		defer pushMu.Unlock()
		for _, p := range pushes {
			if p[firebaseInit.FIREBASE_PUSH_DATA_TYPE] == "calendar" && p[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] == event &&
				strings.Contains(p[firebaseInit.FIREBASE_PUSH_DATA_TITLE], " "+kind+" ") {
				return true
			}
		}
		return false
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Sam owns the page, every hour of every day in UTC; a phone of theirs takes pushes.
	sam := uuid.New()
	exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'sam@acme.test', 'sam')`, sam)
	exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', 'tok-sam')`, sam)
	exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, 'unsub-sam')`, sam)
	dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam Rivera",
		"user_full_name": "Samuel Rivera", "user_email_id": "sam@acme.test"})
	du, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
	if err != nil || du == nil {
		t.Fatalf("sam: %v", err)
	}
	samInfo := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: sam, EmailID: "sam@acme.test"}, UserDgraphInfo: *du}
	if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
		Slug: "intro-call", Title: "Intro call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
		Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", TZ: "UTC"},
	}); err != nil {
		t.Fatal(err)
	}

	day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	book := func(t *testing.T, h int, name, email string) *calendarBusiness.Booked {
		t.Helper()
		b, err := calendarBusiness.Book(ctx, "intro-call", calendarBusiness.BookInput{Start: at(h), Name: name, Email: email,
			TZ: "Europe/London"}, time.Now())
		if err != nil {
			t.Fatalf("booking %02d:00: %v", h, err)
		}
		return b
	}
	eventOf := func(t *testing.T, b *calendarBusiness.Booked) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := env.PG.QueryRow(`SELECT event_uuid FROM bookings WHERE id = $1`, b.BookingID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	offered := func(t *testing.T, h int) bool {
		t.Helper()
		p, err := calendarBusiness.GetPublicPage(ctx, "intro-call", at(h), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Slots {
			if s.Start.Equal(at(h)) {
				return true
			}
		}
		return false
	}
	booking := func(t *testing.T, b *calendarBusiness.Booked) *calendarBusiness.BookingView {
		t.Helper()
		v, err := calendarBusiness.GetBooking(ctx, b.CancelToken)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	listed := func(t *testing.T, event uuid.UUID) (found bool, booked map[string]any) {
		t.Helper()
		from, to := day.Add(-24*time.Hour), day.Add(48*time.Hour)
		events, err := calendarBusiness.GetEventsList(ctx, samInfo, &from, &to)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Uuid == event.String() {
				raw, _ := json.Marshal(e)
				var shape map[string]any
				_ = json.Unmarshal(raw, &shape)
				b, _ := shape["event_booking"].(map[string]any)
				return true, b
			}
		}
		return false, nil
	}
	// This edition has no inbox item for a booking: the owner is told by email
	// (queued here, sent by the worker) and by push.
	ownerEmails := func(word, guest string) int {
		var n int
		_ = env.PG.QueryRow(`SELECT COUNT(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'calendar.booking'
			AND subject = $2`, sam, guest+" "+word+" Intro call").Scan(&n)
		return n
	}

	var alex *calendarBusiness.Booked
	var alexEvent uuid.UUID
	t.Run("a booking emails its guest an invite, and tells its owner by email and by push", func(t *testing.T) {
		alex = book(t, 10, "Alex Kim", "alex.kim@outlook.com")
		alexEvent = eventOf(t, alex)
		if !alex.Emailed || !alex.OwnerTold || alex.BookingID == "" {
			t.Fatalf("booked %+v", alex)
		}
		sent := mailbox.to("alex.kim@outlook.com")
		if len(sent) != 1 {
			t.Fatalf("emails to the guest: %d", len(sent))
		}
		inv := sent[0].invite()
		for _, want := range []string{"METHOD:REQUEST", "UID:" + alex.BookingID + "@onecamp", "SEQUENCE:0",
			"DTSTART:" + at(10).Format("20060102T150405Z"), "STATUS:CONFIRMED", "mailto:alex.kim@outlook.com"} {
			if !strings.Contains(inv, want) {
				t.Errorf("the invite has no %q:\n%s", want, inv)
			}
		}
		if strings.Contains(inv, alex.CancelToken) {
			t.Error("the invite carries the guest's cancel link")
		}
		if n := ownerEmails("booked", "Alex Kim"); n != 1 {
			t.Errorf("Sam's emails: %d", n)
		}
		bookingEventually(t, "a push to Sam", func() bool { return pushedFor("booked", alexEvent.String()) })
		if found, b := listed(t, alexEvent); !found || b["guest_name"] != "Alex Kim" || b["page_title"] != "Intro call" || b["guest_told"] != true {
			t.Errorf("Sam's calendar: listed %v, booking %v", found, b)
		}
		if offered(t, 10) {
			t.Error("10:00 is still offered")
		}
	})

	t.Run("the owner moves a booked meeting: the booking moves, the guest is told, the old time is free", func(t *testing.T) {
		if err := calendarBusiness.UpdateEvent(ctx, alexEvent, adapter.CreateOrUpdateEventInput{
			Title: "Intro call with Alex Kim", StartTime: at(14).Format(time.RFC3339), EndTime: at(14).Add(30 * time.Minute).Format(time.RFC3339),
		}, samInfo); err != nil {
			t.Fatal(err)
		}
		if v := booking(t, alex); !v.Start.Equal(at(14)) || !v.End.Equal(at(14).Add(30*time.Minute)) || v.Cancelled {
			t.Errorf("the guest's link shows %+v", v)
		}
		bookingEventually(t, "the move's email", func() bool { return len(mailbox.to("alex.kim@outlook.com")) >= 2 })
		sent := mailbox.to("alex.kim@outlook.com")
		if len(sent) != 2 {
			t.Fatalf("emails to the guest: %d", len(sent))
		}
		moved := sent[1]
		if !strings.Contains(moved.Subject, "Moved") || !strings.Contains(moved.HTML, "Sam Rivera moved your meeting") {
			t.Errorf("the move's email: %q", moved.Subject)
		}
		inv := moved.invite()
		if !strings.Contains(inv, "METHOD:REQUEST") || !strings.Contains(inv, "UID:"+alex.BookingID+"@onecamp") ||
			!strings.Contains(inv, "DTSTART:"+at(14).Format("20060102T150405Z")) || strings.Contains(inv, "SEQUENCE:0\r\n") {
			t.Errorf("the move's invite:\n%s", inv)
		}
		// Retitling it without moving it tells nobody.
		if err := calendarBusiness.UpdateEvent(ctx, alexEvent, adapter.CreateOrUpdateEventInput{
			Title: "Intro call (Alex)", StartTime: at(14).Format(time.RFC3339), EndTime: at(14).Add(30 * time.Minute).Format(time.RFC3339),
		}, samInfo); err != nil {
			t.Fatal(err)
		}
		settled()
		if n := len(mailbox.to("alex.kim@outlook.com")); n != 2 {
			t.Errorf("a retitle emailed the guest: %d emails", n)
		}
		if !offered(t, 10) {
			t.Error("10:00 isn't offered again")
		}
		book(t, 10, "Jo Park", "jo.park@outlook.com") // and can be booked again
	})

	t.Run("the owner deletes a booked meeting: it's cancelled by its host, the guest is told, the time is free", func(t *testing.T) {
		if err := calendarBusiness.DeleteEvent(ctx, alexEvent, samInfo); err != nil {
			t.Fatal(err)
		}
		if v := booking(t, alex); !v.Cancelled {
			t.Errorf("the guest's link shows %+v", v)
		}
		bookingEventually(t, "the cancel's email", func() bool { return len(mailbox.to("alex.kim@outlook.com")) >= 3 })
		sent := mailbox.to("alex.kim@outlook.com")
		if len(sent) != 3 {
			t.Fatalf("emails to the guest: %d", len(sent))
		}
		gone := sent[2]
		if !strings.Contains(gone.HTML, "Sam Rivera cancelled your meeting") || !strings.Contains(gone.HTML, "/book/intro-call") {
			t.Errorf("the cancel's email: %q", gone.Subject)
		}
		if inv := gone.invite(); !strings.Contains(inv, "METHOD:CANCEL") || !strings.Contains(inv, "STATUS:CANCELLED") ||
			!strings.Contains(inv, "UID:"+alex.BookingID+"@onecamp") {
			t.Errorf("the cancel's invite:\n%s", inv)
		}
		// The guest cancelling now changes nothing and tells nobody again.
		if _, err := calendarBusiness.CancelBooking(ctx, alex.CancelToken); err != nil {
			t.Fatal(err)
		}
		settled()
		if n := len(mailbox.to("alex.kim@outlook.com")); n != 3 {
			t.Errorf("emails to the guest after cancelling twice: %d", n)
		}
		if !offered(t, 14) {
			t.Error("14:00 isn't offered again")
		}
		book(t, 14, "Ravi Shah", "ravi.shah@outlook.com")
	})

	t.Run("a guest cancels: the owner is told, and the guest's calendar loses it", func(t *testing.T) {
		mo := book(t, 16, "Mo Ali", "mo.ali@outlook.com")
		event := eventOf(t, mo)
		view, err := calendarBusiness.CancelBooking(ctx, mo.CancelToken)
		if err != nil || !view.Cancelled || !view.OwnerTold || !view.PageLive || view.BookingID != mo.BookingID {
			t.Fatalf("cancelled %+v, %v", view, err)
		}
		if n := ownerEmails("cancelled", "Mo Ali"); n != 1 {
			t.Errorf("Sam's emails: %d", n)
		}
		bookingEventually(t, "a push to Sam", func() bool { return pushedFor("cancelled", event.String()) })
		settled()
		sent := mailbox.to("mo.ali@outlook.com")
		if len(sent) != 2 {
			t.Fatalf("emails to the guest: %d (one booked, one cancelled, never the host's too)", len(sent))
		}
		if inv := sent[1].invite(); !strings.Contains(inv, "METHOD:CANCEL") || !strings.Contains(sent[1].HTML, "Your booking is cancelled") {
			t.Errorf("the cancel's email %q:\n%s", sent[1].Subject, inv)
		}
		if found, _ := listed(t, event); found {
			t.Error("the event is still on Sam's calendar")
		}
	})

	t.Run("without notification email the owner still gets the push, and the calendar says the guest won't be told", func(t *testing.T) {
		t.Setenv("RESEND_API_KEY", "")
		before := mailbox.count()
		lee := book(t, 18, "Lee Chan", "lee.chan@outlook.com")
		event := eventOf(t, lee)
		// No inbox here and no email: the guest isn't told the owner was.
		if lee.Emailed || lee.OwnerTold {
			t.Errorf("booked %+v", lee)
		}
		bookingEventually(t, "a push to Sam", func() bool { return pushedFor("booked", event.String()) })
		if found, b := listed(t, event); !found || b["guest_told"] != false {
			t.Errorf("Sam's calendar: listed %v, booking %v", found, b)
		}
		if err := calendarBusiness.DeleteEvent(ctx, event, samInfo); err != nil {
			t.Fatal(err)
		}
		if v := booking(t, lee); !v.Cancelled {
			t.Errorf("the guest's link shows %+v", v)
		}
		settled()
		if n := mailbox.count() - before; n != 0 {
			t.Errorf("%d emails sent with email off", n)
		}
	})

	t.Run("the owner's list counts each page's bookings still to come", func(t *testing.T) {
		if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
			Slug: "quick-chat", Title: "Quick chat", DurationMinutes: 15, MaxDaysAhead: 30, Active: true,
			Hours: calendarBusiness.WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "UTC"},
		}); err != nil {
			t.Fatal(err)
		}
		pages, err := bookingModel.ListPages(sam)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, p := range pages {
			got[p.Slug] = p.UpcomingBookings
		}
		// Jo at 10:00 and Ravi at 14:00; Alex's and Lee's were cancelled by
		// their host, and Mo cancelled.
		if len(got) != 2 || got["intro-call"] != 2 || got["quick-chat"] != 0 {
			t.Errorf("upcoming bookings %v", got)
		}
	})

	t.Run("a page with bookings still to come is kept: switched off, its guests can still cancel", func(t *testing.T) {
		hours := calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", TZ: "UTC"}
		page, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
			Slug: "office-hours", Title: "Office hours", DurationMinutes: 30, MaxDaysAhead: 30, Active: true, Hours: hours})
		if err != nil {
			t.Fatal(err)
		}
		pat, err := calendarBusiness.Book(ctx, "office-hours", calendarBusiness.BookInput{Start: at(20), Name: "Pat Lee",
			Email: "pat.lee@outlook.com", TZ: "UTC"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var held *calendarBusiness.PageHasBookingsError
		if ok, err := calendarBusiness.DeleteBookingPage(ctx, sam, page.Id); ok || !errors.As(err, &held) || held.Upcoming != 1 {
			t.Fatalf("deleting a page with a booking to come: %v, %v", ok, err)
		}
		// Switched off instead: nobody can book it, and Pat's link still works.
		if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{Id: page.Id.String(),
			Slug: "office-hours", Title: "Office hours", DurationMinutes: 30, MaxDaysAhead: 30, Active: false, Hours: hours}); err != nil {
			t.Fatal(err)
		}
		if _, err := calendarBusiness.GetPublicPage(ctx, "office-hours", time.Now(), time.Now()); !errors.Is(err, calendarBusiness.ErrNoPage) {
			t.Errorf("the switched-off page answers %v", err)
		}
		if v := booking(t, pat); v.Cancelled || v.PageLive {
			t.Errorf("the guest's link shows %+v", v)
		}
		view, err := calendarBusiness.CancelBooking(ctx, pat.CancelToken)
		if err != nil || !view.Cancelled {
			t.Fatalf("cancelled %+v, %v", view, err)
		}
		// Nothing still to come: now it goes.
		if ok, err := calendarBusiness.DeleteBookingPage(ctx, sam, page.Id); !ok || err != nil {
			t.Errorf("deleting a page with nothing to come: %v, %v", ok, err)
		}
	})
}

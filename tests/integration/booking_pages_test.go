//go:build integration
// +build integration

package integration_test

// Booking pages against real Postgres 12, Dgraph and Redis, with Resend and
// Firebase stood in for: a booking emails its guest an invite (CAL-BUG-15)
// and tells its owner in the app and by push (CAL-BUG-5); the owner moving or
// deleting a booked meeting moves or cancels the booking, tells the guest and
// frees the slot (CAL-BUG-4); a guest cancelling takes the meeting off both
// calendars; and without notification email the owner is still told. Nobody
// else can change, take or delete someone's page, by its id or its address. On
// beta, a deleted page keeps its bookings and their links (CAL-BUG-3, with
// migration 212 down and up again). A cancel link is kept only as its hash
// and its seal, and a link mailed before that still cancels, through the
// minute the old server runs on the new schema and the new one's start
// (audit A1).
//
// Run: go test -tags=integration ./tests/integration/ -run TestBookingPages -v

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	linkSecretModel "github.com/akashc777/OneCamp/models/postgres/LinkSecret"
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
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
		// Each store's run has databases of its own; the booking pages kept
		// by address for the run before ("office-hours", a minute) aren't
		// this run's.
		calendarBusiness.ForgetBookingPagesForTest()
		t.Cleanup(calendarBusiness.ForgetBookingPagesForTest)
		env := integration.SetupEnv(t)
		if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
			t.Fatal(err)
		}
		integration.SetupRedis(t)
		integration.StubMqttClient(t)
		dg := integration.SetupDgraph(t)
		if helpers.CalendarInPostgres() {
			integration.MarkCalendarMoved(t, env.PG)
		}

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
		// keptInClear is how many bookings hold a cancel link anywhere in
		// their row, where a copy of the table would find it; and whether the
		// booking is found by the link's hash.
		keptInClear := func(t *testing.T, id, link string) (n int, hashed bool) {
			t.Helper()
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM bookings b WHERE strpos(b::text, $1) > 0`, link).Scan(&n); err != nil {
				t.Fatal(err)
			}
			var hash []byte
			err := env.PG.QueryRow(`SELECT cancel_token_hash FROM bookings WHERE id = $1`, id).Scan(&hash)
			return n, err == nil && bytes.Equal(hash, helpers.LinkSecretHash(link))
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
		ownerNotices := func(kind, event string) int {
			var n int
			_ = env.PG.QueryRow(`SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = $2 AND resource_uuid = $3`,
				sam, kind, event).Scan(&n)
			return n
		}

		var alex *calendarBusiness.Booked
		var alexEvent uuid.UUID
		t.Run("a booking emails its guest an invite, and tells its owner in the app and by push", func(t *testing.T) {
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
			if n, hashed := keptInClear(t, alex.BookingID, alex.CancelToken); n != 0 || !hashed {
				t.Errorf("%d bookings keep the guest's cancel link in the clear; found by its hash: %v", n, hashed)
			}
			if n := ownerNotices("booking.created", alexEvent.String()); n != 1 {
				t.Errorf("Sam's inbox items: %d", n)
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
			if !strings.Contains(moved.Subject, "Moved") || !strings.Contains(moved.HTML, "Sam Rivera moved your meeting") ||
				!strings.Contains(moved.HTML, "(Europe/London)") {
				t.Errorf("the move's email %q, in the guest's zone:\n%s", moved.Subject, moved.HTML)
			}
			if !strings.Contains(moved.HTML, "/booking/"+alex.CancelToken) || !strings.Contains(moved.HTML, "with the button above") {
				t.Errorf("the move's email doesn't give the guest their own cancel link again:\n%s", moved.HTML)
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
			if v := booking(t, alex); !v.Cancelled || !v.CancelledByHost {
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
			if v := booking(t, mo); !v.Cancelled || v.CancelledByHost {
				t.Errorf("the guest's link shows %+v", v)
			}
			if n := ownerNotices("booking.cancelled", event.String()); n != 1 {
				t.Errorf("Sam's inbox items: %d", n)
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

		t.Run("without notification email the owner is still told, and the calendar says the guest won't be", func(t *testing.T) {
			t.Setenv("RESEND_API_KEY", "")
			before := mailbox.count()
			lee := book(t, 18, "Lee Chan", "lee.chan@outlook.com")
			event := eventOf(t, lee)
			if lee.Emailed || !lee.OwnerTold {
				t.Errorf("booked %+v", lee)
			}
			if n := ownerNotices("booking.created", event.String()); n != 1 {
				t.Errorf("Sam's inbox items: %d", n)
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

		t.Run("someone else can't change, take or delete one of Sam's pages", func(t *testing.T) {
			maya := uuid.New()
			exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'maya@acme.test', 'maya')`, maya)
			var page uuid.UUID
			if err := env.PG.QueryRow(`SELECT id FROM booking_pages WHERE slug = 'intro-call'`).Scan(&page); err != nil {
				t.Fatal(err)
			}
			hers := calendarBusiness.PageInput{Id: page.String(), Slug: "intro-call", Title: "Mine now", DurationMinutes: 60,
				MaxDaysAhead: 7, Active: true, Hours: calendarBusiness.WorkingHours{Days: []int{1}, Start: "09:00", End: "17:00", TZ: "UTC"}}
			var notHers *calendarBusiness.AvailabilityError
			if _, err := calendarBusiness.SaveBookingPage(ctx, maya, hers); !errors.As(err, &notHers) {
				t.Errorf("Maya changed Sam's page by its id: %v", err)
			}
			hers.Id = ""
			if _, err := calendarBusiness.SaveBookingPage(ctx, maya, hers); !errors.Is(err, helpers.ErrConflict) {
				t.Errorf("Maya took the address of Sam's live page: %v", err)
			}
			if ok, _, err := calendarBusiness.DeleteBookingPage(ctx, maya, page); ok || err != nil {
				t.Errorf("Maya deleted Sam's page: %v %v", ok, err)
			}
			var owner uuid.UUID
			var title string
			var deleted *time.Time
			if err := env.PG.QueryRow(`SELECT user_id, title, deleted_at FROM booking_pages WHERE id = $1`, page).Scan(&owner, &title, &deleted); err != nil {
				t.Fatal(err)
			}
			if owner != sam || title != "Intro call" || deleted != nil {
				t.Errorf("Sam's page is now %v's %q, deleted at %v", owner, title, deleted)
			}
		})

		t.Run("a page whose owner can't be read says to try again, not that there's no such page", func(t *testing.T) {
			calendarBusiness.ForgetBookingPagesForTest()
			dg.Break(t)
			if _, err := calendarBusiness.GetPublicPage(ctx, "intro-call", time.Now(), time.Now()); err == nil || errors.Is(err, calendarBusiness.ErrNoPage) {
				t.Errorf("the owner's read failed and the page answered %v", err)
			}
		})

		t.Run("a deleted page keeps its bookings and their links, and its address", func(t *testing.T) {
			if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
				Slug: "office-hours", Title: "Office hours", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
				Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", TZ: "UTC"},
			}); err != nil {
				t.Fatal(err)
			}
			pat, err := calendarBusiness.Book(ctx, "office-hours", calendarBusiness.BookInput{Start: at(20), Name: "Pat Lee",
				Email: "pat.lee@outlook.com", TZ: "UTC"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var pageID uuid.UUID
			if err := env.PG.QueryRow(`SELECT page_id FROM bookings WHERE id = $1`, pat.BookingID).Scan(&pageID); err != nil {
				t.Fatal(err)
			}
			ok, upcoming, err := calendarBusiness.DeleteBookingPage(ctx, sam, pageID)
			if err != nil || !ok || upcoming != 1 {
				t.Fatalf("deleted %v with %d upcoming: %v", ok, upcoming, err)
			}
			if _, err := calendarBusiness.GetPublicPage(ctx, "office-hours", time.Now(), time.Now()); !errors.Is(err, calendarBusiness.ErrNoPage) {
				t.Errorf("the deleted page answers %v", err)
			}
			if _, err := calendarBusiness.Book(ctx, "office-hours", calendarBusiness.BookInput{Start: at(21), Name: "Kai",
				Email: "kai@outlook.com", TZ: "UTC"}, time.Now()); !errors.Is(err, calendarBusiness.ErrNoPage) {
				t.Errorf("booking the deleted page: %v", err)
			}
			pages, _ := bookingModel.ListPages(sam)
			for _, p := range pages {
				if p.Slug == "office-hours" {
					t.Error("the deleted page is still listed")
				}
			}
			// The guest's link still shows the meeting, without "Book another time".
			v := booking(t, pat)
			if v.Cancelled || v.PageLive || !v.Start.Equal(at(20)) || v.Title != "Office hours" {
				t.Errorf("the guest's link shows %+v", v)
			}
			// Nobody else can take the address, so an old link never lands on their page.
			other := uuid.New()
			exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'kim@acme.test', 'kim')`, other)
			if _, err := calendarBusiness.SaveBookingPage(ctx, other, calendarBusiness.PageInput{
				Slug: "office-hours", Title: "Mine now", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
				Hours: calendarBusiness.WorkingHours{Days: []int{1}, Start: "09:00", End: "17:00", TZ: "UTC"},
			}); !errors.Is(err, helpers.ErrConflict) || !strings.Contains(err.Error(), `already uses the address "office-hours"`) {
				t.Errorf("someone else took the address, or wasn't told which one clashed: %v", err)
			}
			// Moving a page of their own onto the address is the same clash,
			// found by Postgres' unique constraint on the address.
			kims, err := calendarBusiness.SaveBookingPage(ctx, other, calendarBusiness.PageInput{
				Slug: "kims-page", Title: "Kim's page", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
				Hours: calendarBusiness.WorkingHours{Days: []int{1}, Start: "09:00", End: "17:00", TZ: "UTC"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := calendarBusiness.SaveBookingPage(ctx, other, calendarBusiness.PageInput{
				Id: kims.Id.String(), Slug: "office-hours", Title: "Kim's page", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
				Hours: calendarBusiness.WorkingHours{Days: []int{1}, Start: "09:00", End: "17:00", TZ: "UTC"},
			}); !errors.Is(err, helpers.ErrConflict) || !strings.Contains(err.Error(), `"office-hours"`) {
				t.Errorf("moving a page onto a taken address: %v", err)
			}
			// Nor can the page be removed outright under its bookings any more.
			if _, err := env.PG.Exec(`DELETE FROM booking_pages WHERE id = $1`, pageID); err == nil {
				t.Error("a page with bookings was removed outright")
			}
			// The guest can still cancel; Sam is told, and Pat's calendar loses it.
			view, err := calendarBusiness.CancelBooking(ctx, pat.CancelToken)
			if err != nil || !view.Cancelled || !view.OwnerTold || view.PageLive {
				t.Fatalf("cancelled %+v, %v", view, err)
			}
			sent := mailbox.to("pat.lee@outlook.com")
			if len(sent) != 2 || !strings.Contains(sent[1].invite(), "METHOD:CANCEL") || strings.Contains(sent[1].HTML, "/book/office-hours") {
				t.Errorf("emails to the guest: %d", len(sent))
			}
			// Sam making a page at the address again gets the same page back.
			again, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
				Slug: "office-hours", Title: "Office hours again", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
				Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "00:00", End: "24:00", TZ: "UTC"},
			})
			if err != nil || again.Id != pageID {
				t.Fatalf("the address again: %+v, %v", again, err)
			}
			if p, err := calendarBusiness.GetPublicPage(ctx, "office-hours", time.Now(), time.Now()); err != nil || p.Title != "Office hours again" {
				t.Errorf("the page again: %+v, %v", p, err)
			}
		})

		t.Run("links from before hashing keep cancelling, through the old server's last minute and the new one's start", func(t *testing.T) {
			_, file, _, _ := runtime.Caller(0)
			dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
			matches, err := filepath.Glob(filepath.Join(dir, "*_booking_cancel_link_hash.up.sql"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("the migration: %v %v", matches, err)
			}
			n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
			if err != nil {
				t.Fatal(err)
			}
			kim := book(t, 6, "Kim Lee", "kim.lee@outlook.com")
			kimEvent := eventOf(t, kim)
			m, err := migrate.New("file://"+dir, env.DSN)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			for v, _, _ := m.Version(); v >= uint(n); v, _, _ = m.Version() {
				if err := m.Steps(-1); err != nil {
					t.Fatal(err)
				}
			}
			// Kim's link as the server from before mailed it: 48 hex characters, kept as they are.
			mailed := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")[:48]
			if _, err := env.PG.Exec(`UPDATE bookings SET cancel_token = $2 WHERE id = $1`, kim.BookingID, mailed); err != nil {
				t.Fatal(err)
			}
			if err := m.Up(); err != nil {
				t.Fatal(err)
			}

			// The migrations have run and the old server hasn't stopped: it
			// still finds Kim's booking by the link, and takes Lou's booking
			// its own way.
			var found string
			if err := env.PG.QueryRow(`SELECT id FROM bookings WHERE cancel_token = $1`, mailed).Scan(&found); err != nil || found != kim.BookingID {
				t.Fatalf("the old server looking Kim's link up: %q, %v", found, err)
			}
			lou := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")[:48]
			var louID string
			if err := env.PG.QueryRow(`INSERT INTO bookings (page_id, guest_name, guest_email, note, starts_at, ends_at, cancel_token, guest_tz)
				SELECT page_id, 'Lou Park', 'lou.park@outlook.com', '', $2, $3, $4, 'UTC' FROM bookings WHERE id = $1 RETURNING id`,
				kim.BookingID, at(8), at(8).Add(30*time.Minute), lou).Scan(&louID); err != nil {
				t.Fatalf("the old server booking: %v", err)
			}

			// The new server starts.
			if err := linkSecretModel.HashLeftInClear(ctx); err != nil {
				t.Fatal(err)
			}
			for id, link := range map[string]string{kim.BookingID: mailed, louID: lou} {
				if n, hashed := keptInClear(t, id, link); n != 0 || !hashed {
					t.Errorf("%s: %d bookings keep the link in the clear; found by its hash: %v", id, n, hashed)
				}
				if v, err := calendarBusiness.GetBooking(ctx, link); err != nil || v.BookingID != id || v.Cancelled {
					t.Errorf("%s: the link shows %+v, %v", id, v, err)
				}
			}
			if err := linkSecretModel.HashLeftInClear(ctx); err != nil {
				t.Fatalf("starting again: %v", err)
			}
			if v, err := calendarBusiness.GetBooking(ctx, lou); err != nil || v.BookingID != louID {
				t.Errorf("after starting again, Lou's link shows %+v, %v", v, err)
			}

			// Kim's link can't be sealed, so a move points Kim back to the link they have.
			if err := calendarBusiness.UpdateEvent(ctx, kimEvent, adapter.CreateOrUpdateEventInput{
				Title: "Intro call with Kim Lee", StartTime: at(7).Format(time.RFC3339), EndTime: at(7).Add(30 * time.Minute).Format(time.RFC3339),
			}, samInfo); err != nil {
				t.Fatal(err)
			}
			bookingEventually(t, "the move's email", func() bool { return len(mailbox.to("kim.lee@outlook.com")) >= 2 })
			moved := mailbox.to("kim.lee@outlook.com")[1]
			if strings.Contains(moved.HTML, "/booking/") || !strings.Contains(moved.HTML, "with the link you got when you booked") {
				t.Errorf("the move's email for a link from before:\n%s", moved.HTML)
			}
			if v, err := calendarBusiness.CancelBooking(ctx, mailed); err != nil || !v.Cancelled || !v.Start.Equal(at(7)) {
				t.Errorf("cancelling by the mailed link: %+v, %v", v, err)
			}
		})

		t.Run("migration 212 goes down and up again", func(t *testing.T) {
			_, file, _, _ := runtime.Caller(0)
			dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
			matches, err := filepath.Glob(filepath.Join(dir, "*_booking_pages_soft_delete.up.sql"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("the migration: %v %v", matches, err)
			}
			n, err := strconv.Atoi(strings.SplitN(filepath.Base(matches[0]), "_", 2)[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d_booking_pages_soft_delete.down.sql", n))); err != nil {
				t.Fatal(err)
			}
			shape := func() (deletedAt int, onDelete string) {
				_ = env.PG.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
				WHERE table_name = 'booking_pages' AND column_name = 'deleted_at'`).Scan(&deletedAt)
				_ = env.PG.QueryRow(`SELECT confdeltype FROM pg_constraint WHERE conname = 'bookings_page_id_fkey'`).Scan(&onDelete)
				return
			}
			if cols, del := shape(); cols != 1 || del != "r" {
				t.Fatalf("before: deleted_at %d, on delete %q", cols, del)
			}
			m, err := migrate.New("file://"+dir, env.DSN)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			for v, _, _ := m.Version(); v >= uint(n); v, _, _ = m.Version() {
				if err := m.Steps(-1); err != nil {
					t.Fatal(err)
				}
			}
			if cols, del := shape(); cols != 0 || del != "c" {
				t.Errorf("down: deleted_at %d, on delete %q", cols, del)
			}
			var bookings int
			_ = env.PG.QueryRow(`SELECT COUNT(*) FROM bookings`).Scan(&bookings)
			if bookings < 6 {
				t.Errorf("down kept %d bookings", bookings)
			}
			if err := m.Up(); err != nil {
				t.Fatal(err)
			}
			if cols, del := shape(); cols != 1 || del != "r" {
				t.Errorf("up again: deleted_at %d, on delete %q", cols, del)
			}
		})
	})
}

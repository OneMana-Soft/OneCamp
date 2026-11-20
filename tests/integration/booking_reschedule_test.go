//go:build integration
// +build integration

package integration_test

// A guest moves their booking by its link (gcal-7 H2a), against real
// Postgres 12, Dgraph and Redis, with Resend stood in for. The move is the
// page owner's own: their one meeting moves, the booking follows it, the old
// time is free again and the new one taken, and the guest gets one updated
// invite (REQUEST, the booking's UID, a SEQUENCE that grows). A busy time, a
// time out of the page's hours or in the past is refused; a link that names
// no booking, a cancelled or ended one, or one whose page is closed gets the
// one answer a missing booking gets. The page's lock is held from the slot
// check to the move, so a guest booking that time meanwhile is refused.
//
// Run: PKGS=./tests/integration/ ... -run TestBookingReschedule
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	bookingDomain "github.com/akashc777/OneCamp/domain/Booking"
	scheduledJobDomain "github.com/akashc777/OneCamp/domain/ScheduledJob"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

// inviteSequence is an invite's SEQUENCE, or -1 without one.
func inviteSequence(inv string) int {
	_, rest, ok := strings.Cut(inv, "SEQUENCE:")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]))
	if err != nil {
		return -1
	}
	return n
}

func TestBookingReschedule(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
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
		t.Setenv("RESEND_API_KEY", "re_booking_reschedule_test")
		t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
		mailbox := &guestMailbox{}
		oldClient := emailService.SharedHTTPClient
		emailService.SharedHTTPClient = &http.Client{Transport: mailbox}
		t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })
		emailService.LetReservedNamesThroughForTest(t)
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := env.PG.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}

		// Sam's page books half hours, 09:00 to 17:00 UTC, every day.
		sam := uuid.New()
		exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'sam@example.com', 'sam')`, sam)
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam Rivera",
			"user_full_name": "Samuel Rivera", "user_email_id": "sam@example.com"})
		du, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
		if err != nil || du == nil {
			t.Fatalf("sam: %v", err)
		}
		samInfo := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: sam, EmailID: "sam@example.com"}, UserDgraphInfo: *du}
		if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
			Slug: "move-call", Title: "Move call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
			Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"},
		}); err != nil {
			t.Fatal(err)
		}
		day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
		book := func(h int, name, email string) *calendarBusiness.Booked {
			t.Helper()
			b, err := calendarBusiness.Book(ctx, "move-call", calendarBusiness.BookInput{Start: at(h), Name: name, Email: email,
				TZ: "UTC", Note: "private note of " + name}, time.Now())
			if err != nil {
				t.Fatalf("booking %02d:00: %v", h, err)
			}
			return b
		}
		offered := func(h int) bool {
			t.Helper()
			p, err := calendarBusiness.GetPublicPage(ctx, "move-call", at(h), time.Now())
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
		row := func(id string) (time.Time, uuid.UUID) {
			t.Helper()
			var start time.Time
			var event uuid.UUID
			if err := env.PG.QueryRow(`SELECT starts_at, event_uuid FROM bookings WHERE id = $1`, id).Scan(&start, &event); err != nil {
				t.Fatal(err)
			}
			return start.UTC(), event
		}
		// The owner's own calendar: the meetings with a guest, by title.
		meetingsWith := func(name string) []time.Time {
			t.Helper()
			from, to := day.Add(-24*time.Hour), day.Add(48*time.Hour)
			evs, err := calendarBusiness.GetEventsList(ctx, samInfo, &from, &to)
			if err != nil {
				t.Fatal(err)
			}
			var out []time.Time
			for _, e := range evs {
				if e != nil && e.StartTime != nil && strings.Contains(e.Title, "with "+name) {
					out = append(out, e.StartTime.UTC())
				}
			}
			return out
		}
		r := chi.NewRouter()
		r.Post("/public/booking/{token}/reschedule", eventController.ReschedulePublicBooking)
		move := func(token, body string) (int, string) {
			t.Helper()
			req := httptest.NewRequest(http.MethodPost, "/public/booking/"+token+"/reschedule", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			return rec.Code, rec.Body.String()
		}
		startAt := func(ts time.Time) string { return `{"start":"` + ts.Format(time.RFC3339) + `"}` }
		mailsTo := func(addr string) int { return len(mailbox.to(addr)) }
		// The owner's inbox items of a guest's move, to the time given or
		// (zero) any.
		movedNotices := func(event uuid.UUID, to time.Time) int {
			var n int
			q := `SELECT COUNT(*) FROM resource_notices WHERE user_uuid = $1 AND kind = 'booking.moved' AND resource_uuid = $2`
			args := []any{sam, event.String()}
			if !to.IsZero() {
				q += ` AND data->>'starts_at' = $3`
				args = append(args, to.UTC().Format(time.RFC3339))
			}
			if err := env.PG.QueryRow(q, args...).Scan(&n); err != nil {
				t.Errorf("the owner's notices: %v", err)
			}
			return n
		}
		// The guest's own move says so; the owner's says the owner moved it.
		guestCopy := func(msg mailedMessage) bool {
			return strings.Contains(msg.HTML, "You moved your meeting") && !strings.Contains(msg.HTML, "Sam Rivera moved your meeting")
		}

		alex := book(10, "Alex Kim", "alex@example.com")
		bo := book(14, "Bo Lin", "bo@example.com")
		_, alexEvent := row(alex.BookingID)
		bookingEventually(t, "the booking's email", func() bool { return mailsTo("alex@example.com") >= 1 })
		settled()
		if mailsTo("alex@example.com") != 1 {
			t.Fatalf("the booking's email: %d", mailsTo("alex@example.com"))
		}

		// 1. Alex moves to 11:00. The body's other fields are ignored: the
		// guest never sets the title, the people or the owner.
		code, body := move(alex.CancelToken, `{"start":"`+at(11).Format(time.RFC3339)+
			`","title":"Hijacked","participants":["`+uuid.NewString()+`"],"owner":"`+uuid.NewString()+`"}`)
		if code != http.StatusOK {
			t.Fatalf("the move: %d %s", code, body)
		}
		var got struct {
			Data struct {
				BookingID string    `json:"booking_id"`
				Start     time.Time `json:"start"`
				End       time.Time `json:"end"`
				Cancelled bool      `json:"cancelled"`
				OwnerTold bool      `json:"owner_told"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil || got.Data.BookingID != alex.BookingID ||
			!got.Data.Start.Equal(at(11)) || !got.Data.End.Equal(at(11).Add(30*time.Minute)) || got.Data.Cancelled ||
			!got.Data.OwnerTold {
			t.Errorf("the move's answer %s (%v)", body, err)
		}
		for _, private := range []string{"Bo Lin", "bo@example.com", "private note", "sam@example.com"} {
			if strings.Contains(body, private) {
				t.Errorf("the guest's answer shows %q: %s", private, body)
			}
		}
		if start, ev := row(alex.BookingID); !start.Equal(at(11)) || ev != alexEvent {
			t.Errorf("the booking is at %v on event %s, want 11:00 on %s", start, ev, alexEvent)
		}
		ev, err := calendarBusiness.GetEvent(ctx, samInfo, alexEvent.String())
		if err != nil || ev == nil || ev.StartTime == nil || !ev.StartTime.Equal(at(11)) || ev.Title != "Move call with Alex Kim" ||
			!strings.Contains(ev.Description, "private note of Alex Kim") {
			t.Errorf("the owner's meeting after the move: %+v %v", ev, err)
		}
		if m := meetingsWith("Alex Kim"); len(m) != 1 || !m[0].Equal(at(11)) {
			t.Errorf("the owner's calendar has Alex's meetings at %v, want one at 11:00", m)
		}
		if !offered(10) || offered(11) {
			t.Errorf("after the move 10:00 offered %v (want true), 11:00 offered %v (want false)", offered(10), offered(11))
		}
		bookingEventually(t, "the move's email", func() bool { return mailsTo("alex@example.com") >= 2 })
		settled()
		sent := mailbox.to("alex@example.com")
		if len(sent) != 2 {
			t.Fatalf("emails to the guest after one move: %d, want 2", len(sent))
		}
		inv := sent[1].invite()
		seq1 := inviteSequence(inv)
		if !strings.Contains(inv, "METHOD:REQUEST") || !strings.Contains(inv, "UID:"+alex.BookingID+"@onecamp") ||
			!strings.Contains(inv, "DTSTART:"+at(11).Format("20060102T150405Z")) || seq1 < 1 {
			t.Errorf("the move's invite (SEQUENCE %d):\n%s", seq1, inv)
		}
		if !guestCopy(sent[1]) {
			t.Errorf("the guest's own move is mailed as someone else's: %s", sent[1].HTML)
		}
		// The owner is told of the move, once, with the new time.
		if n, at11 := movedNotices(alexEvent, time.Time{}), movedNotices(alexEvent, at(11)); n != 1 || at11 != 1 {
			t.Errorf("the owner's notices of the move: %d (%d at 11:00), want 1", n, at11)
		}

		// 2. And again, from the new time: both times it had are free. The
		// move reads its meeting and its page once each, and reads the
		// calendars (Google's over the network) before it takes the page's
		// lock, never holding the lock, its connection and every guest's
		// booking of the page meanwhile (gcal-7 H2, CR-394 F1, F2).
		var pageID uuid.UUID
		if err := env.PG.QueryRow(`SELECT id FROM booking_pages WHERE slug = 'move-call'`).Scan(&pageID); err != nil {
			t.Fatal(err)
		}
		var checks, underLock atomic.Int64
		restoreCheck := calendarBusiness.WhileSlotCheckForTest(func() {
			checks.Add(1)
			var free bool
			tx, err := env.PG.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback() //nolint:errcheck // only asked, never kept
			if err := tx.QueryRow(`SELECT pg_try_advisory_xact_lock(hashtext($1))`, bookingDomain.PageLockKey(pageID)).Scan(&free); err != nil {
				t.Error(err)
			} else if !free {
				underLock.Add(1)
			}
		})
		eventReads, restoreEvents := calendarBusiness.CountEventReadsForTest()
		pageReads, restorePages := bookingModel.CountPageReadsForTest()
		code, body = move(alex.CancelToken, startAt(at(12)))
		restoreCheck()
		restoreEvents()
		restorePages()
		if code != http.StatusOK {
			t.Fatalf("the second move: %d %s", code, body)
		}
		if n := eventReads(); n != 1 {
			t.Errorf("the move read its meeting %d times, want 1", n)
		}
		if n := pageReads(); n != 1 {
			t.Errorf("the move read its page %d times, want 1", n)
		}
		if checks.Load() != 1 || underLock.Load() != 0 {
			t.Errorf("the move's slot checks: %d, %d of them holding the page's lock; want 1, none under it",
				checks.Load(), underLock.Load())
		}
		if !offered(10) || !offered(11) || offered(12) {
			t.Errorf("after two moves: 10:00 %v, 11:00 %v (want both offered), 12:00 %v (want taken)",
				offered(10), offered(11), offered(12))
		}
		if m := meetingsWith("Alex Kim"); len(m) != 1 || !m[0].Equal(at(12)) {
			t.Errorf("after two moves the owner's calendar has Alex's meetings at %v, want one at 12:00", m)
		}
		bookingEventually(t, "the second move's email", func() bool { return mailsTo("alex@example.com") >= 3 })
		settled()
		sent = mailbox.to("alex@example.com")
		if len(sent) != 3 {
			t.Fatalf("emails to the guest after two moves: %d, want 3", len(sent))
		}
		inv = sent[2].invite()
		if seq2 := inviteSequence(inv); !strings.Contains(inv, "DTSTART:"+at(12).Format("20060102T150405Z")) || seq2 < seq1 {
			t.Errorf("the second move's invite (SEQUENCE %d after %d):\n%s", seq2, seq1, inv)
		}
		if !guestCopy(sent[2]) {
			t.Errorf("the guest's second move is mailed as someone else's: %s", sent[2].HTML)
		}
		if n, at12 := movedNotices(alexEvent, time.Time{}), movedNotices(alexEvent, at(12)); n != 2 || at12 != 1 {
			t.Errorf("the owner's notices after two moves: %d (%d at 12:00), want 2", n, at12)
		}

		// 3. Refused, and nothing moves or is mailed: Bo's time, out of the
		// page's hours, in the past, past the page's horizon, no time at all.
		for name, req := range map[string]string{
			"a taken slot":     startAt(at(14)),
			"out of hours":     startAt(at(20)),
			"in the past":      startAt(at(10).Add(-72 * time.Hour)),
			"past the horizon": startAt(at(10).Add(40 * 24 * time.Hour)),
			"no time":          `{}`,
		} {
			if code, body := move(alex.CancelToken, req); code != http.StatusBadRequest {
				t.Errorf("%s: %d %s, want 400", name, code, body)
			}
		}
		if start, _ := row(alex.BookingID); !start.Equal(at(12)) {
			t.Errorf("a refused move moved the booking to %v", start)
		}
		if start, _ := row(bo.BookingID); !start.Equal(at(14)) {
			t.Errorf("Bo's booking is at %v", start)
		}
		// The time it has already: no change, nobody mailed.
		if code, body := move(alex.CancelToken, startAt(at(12))); code != http.StatusOK {
			t.Errorf("moving to the time it has: %d %s", code, body)
		}
		settled()
		if n := mailsTo("alex@example.com"); n != 3 {
			t.Errorf("emails to the guest after refused moves: %d, want 3", n)
		}
		if n := movedNotices(alexEvent, time.Time{}); n != 2 {
			t.Errorf("the owner's notices after refused moves: %d, want 2", n)
		}

		// The owner moving the meeting themselves: the guest reads the
		// owner's copy, and the owner isn't told of their own move.
		if err := calendarBusiness.UpdateEvent(ctx, alexEvent, adapter.CreateOrUpdateEventInput{
			Title: ev.Title, Description: ev.Description,
			StartTime: at(13).Format(time.RFC3339), EndTime: at(13).Add(30 * time.Minute).Format(time.RFC3339),
		}, samInfo); err != nil {
			t.Fatalf("the owner's move: %v", err)
		}
		bookingEventually(t, "the owner's move's email", func() bool { return mailsTo("alex@example.com") >= 4 })
		settled()
		sent = mailbox.to("alex@example.com")
		if len(sent) != 4 || !strings.Contains(sent[3].HTML, "Sam Rivera moved your meeting") || guestCopy(sent[3]) ||
			!strings.Contains(sent[3].invite(), "DTSTART:"+at(13).Format("20060102T150405Z")) {
			t.Errorf("the owner's move mailed the guest %d emails, the last %v", len(sent), sent[len(sent)-1].Subject)
		}
		if n := movedNotices(alexEvent, time.Time{}); n != 2 {
			t.Errorf("the owner was told of their own move: %d notices, want 2", n)
		}

		// 4. The page's lock: a guest booking 15:00 while Alex's move to it
		// holds the lock waits for it, then finds the time taken.
		var eveErr error
		var eveDone sync.WaitGroup
		restore := calendarBusiness.WhileRescheduleHeldForTest(func() {
			eveDone.Add(1)
			go func() {
				defer eveDone.Done()
				_, eveErr = calendarBusiness.Book(ctx, "move-call", calendarBusiness.BookInput{Start: at(15), Name: "Eve Park",
					Email: "eve@example.com", TZ: "UTC"}, time.Now())
			}()
			time.Sleep(time.Second)
		})
		code, body = move(alex.CancelToken, startAt(at(15)))
		restore()
		eveDone.Wait()
		var ae *calendarBusiness.AvailabilityError
		if code != http.StatusOK || !errors.As(eveErr, &ae) {
			t.Errorf("Alex's move to 15:00: %d %s; Eve's booking of it meanwhile: %v (want refused)", code, body, eveErr)
		}
		var at15 int
		if err := env.PG.QueryRow(`SELECT count(*) FROM bookings WHERE starts_at = $1 AND cancelled_at IS NULL`, at(15)).Scan(&at15); err != nil || at15 != 1 {
			t.Errorf("active bookings at 15:00: %d (%v), want 1", at15, err)
		}

		// 4b. A link moves its booking three times a day (V-1194 N1): Alex's
		// two moves and the one to 15:00 told Sam once each (the owner's own
		// move never counts), and neither the move to the time it had nor the
		// refused ones above were charged. The fourth, to a free 13:00, is
		// refused with a wait, tells nobody and moves nothing.
		settled()
		if n, at15 := movedNotices(alexEvent, time.Time{}), movedNotices(alexEvent, at(15)); n != 3 || at15 != 1 {
			t.Errorf("the owner's notices after three moves: %d (%d at 15:00), want 3", n, at15)
		}
		code, body = move(alex.CancelToken, startAt(at(13)))
		if code != http.StatusTooManyRequests || !strings.Contains(body, "This booking has been moved too often today. Try again tomorrow.") {
			t.Errorf("a fourth move in a day: %d %s, want 429 and why", code, body)
		}
		settled()
		if n := movedNotices(alexEvent, time.Time{}); n != 3 {
			t.Errorf("the owner's notices after a refused fourth move: %d, want 3", n)
		}
		if start, _ := row(alex.BookingID); !start.Equal(at(15)) {
			t.Errorf("a refused fourth move moved the booking to %v", start)
		}
		// Counted by the booking, never by its link (a secret kept out of Redis).
		if n, err := redisInit.RedisClient.Get(ctx, registry.BookingMoveRate.Build(alex.BookingID)).Int(); err != nil || n != 4 {
			t.Errorf("the booking's moves today: %d (%v), want 4: three moves and the refused one", n, err)
		}

		// 4c. A time the page doesn't take is refused before the owner's
		// calendars are read (V-1194 N4, N8): inside the page's notice, or
		// past. A booking of such a time is refused the same way.
		if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
			Slug: "notice-call", Title: "Notice call", DurationMinutes: 30, MaxDaysAhead: 30, MinNoticeMinutes: 3 * 24 * 60, Active: true,
			Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"},
		}); err != nil {
			t.Fatal(err)
		}
		fay, err := calendarBusiness.Book(ctx, "notice-call", calendarBusiness.BookInput{Start: at(82), Name: "Fay Moss",
			Email: "fay@example.com", TZ: "UTC"}, time.Now())
		if err != nil {
			t.Fatalf("booking Fay three days on: %v", err)
		}
		_, fayEvent := row(fay.BookingID)
		var reads atomic.Int64
		restoreReads := calendarBusiness.WhileSlotCheckForTest(func() { reads.Add(1) })
		for name, ts := range map[string]time.Time{"inside the notice": at(13), "in the past": at(10).Add(-72 * time.Hour)} {
			reads.Store(0)
			if code, body := move(fay.CancelToken, startAt(ts)); code != http.StatusBadRequest || reads.Load() != 0 {
				t.Errorf("a move %s: %d %s after %d calendar reads, want 400 before any", name, code, body, reads.Load())
			}
		}
		reads.Store(0)
		_, err = calendarBusiness.Book(ctx, "notice-call", calendarBusiness.BookInput{Start: at(13), Name: "Gus Hale",
			Email: "gus@example.org", TZ: "UTC"}, time.Now())
		var notice *calendarBusiness.AvailabilityError
		if !errors.As(err, &notice) || reads.Load() != 0 {
			t.Errorf("a booking inside the notice: %v after %d calendar reads, want refused before any", err, reads.Load())
		}
		restoreReads()
		settled()
		if start, _ := row(fay.BookingID); !start.Equal(at(82)) {
			t.Errorf("a refused move moved Fay's booking to %v", start)
		}
		if n := movedNotices(fayEvent, time.Time{}); n != 0 {
			t.Errorf("the owner's notices of Fay's refused moves: %d, want 0", n)
		}

		// 5. The owner moves Bo's meeting to Room 4 and writes its notes while
		// Bo's move to 13:00 is in its slot check, after the move read the
		// meeting: the move writes over no change it didn't read. It's refused
		// in words, Bo's booking and meeting keep their time, the meeting its
		// room and notes, and the owner is told of no move (gcal-7 H2, CR-393
		// round 2 F1, CR-394 round 2).
		if helpers.CalendarInPostgres() {
			_, boEvent := row(bo.BookingID)
			room, notes := "Room 4", "Bring the launch numbers."
			restoreCheck := calendarBusiness.WhileSlotCheckForTest(func() {
				cur, err := calendarBusiness.Events.Get(ctx, samInfo, boEvent.String())
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := calendarBusiness.Events.Update(ctx, samInfo, boEvent.String(), calendarBusiness.EventPatch{
					Sequence: &cur.Sequence, Location: &room, Notes: &notes, Notify: "none"}); err != nil {
					t.Errorf("the owner's change of the room and notes: %v", err)
				}
			})
			code, body = move(bo.CancelToken, startAt(at(13)))
			restoreCheck()
			if code != http.StatusBadRequest || !strings.Contains(body, "Your booking just changed. Try again.") {
				t.Errorf("Bo's move while the owner changed the meeting: %d %s, want 400 \"Your booking just changed. Try again.\"", code, body)
			}
			if start, _ := row(bo.BookingID); !start.Equal(at(14)) {
				t.Errorf("Bo's booking moved to %v while the owner changed the meeting, want 14:00", start)
			}
			if m := meetingsWith("Bo Lin"); len(m) != 1 || !m[0].Equal(at(14)) {
				t.Errorf("Bo's meetings on the owner's calendar: %v, want one at 14:00", m)
			}
			if ev, err := calendarBusiness.Events.Get(ctx, samInfo, boEvent.String()); err != nil {
				t.Error(err)
			} else if ev.Location != room || !strings.Contains(ev.Notes, notes) {
				t.Errorf("the meeting's room and notes after Bo's refused move: %q, %q; want %q, %q", ev.Location, ev.Notes, room, notes)
			}
			if n := movedNotices(boEvent, time.Time{}); n != 0 {
				t.Errorf("the owner's notices of Bo's refused move: %d, want none", n)
			}
		}

		// 6. One answer for every link that can't move a booking: a bad
		// token, a well-formed one of no booking, a cancelled booking's, an
		// ended booking's, and a booking's whose page is closed.
		cy := book(9, "Cy Moss", "cy@example.com")
		if _, err := calendarBusiness.CancelBooking(ctx, cy.CancelToken); err != nil {
			t.Fatal(err)
		}
		dee := book(16, "Dee Ray", "dee@example.com")
		exec(`UPDATE bookings SET starts_at = NOW() - interval '2 hours', ends_at = NOW() - interval '90 minutes' WHERE id = $1`, dee.BookingID)
		unknown, _, _, err := helpers.NewSealedLinkSecret()
		if err != nil {
			t.Fatal(err)
		}
		missCode, missBody := move("not-a-token", startAt(at(13)))
		if missCode != http.StatusNotFound {
			t.Errorf("a bad token: %d %s, want 404", missCode, missBody)
		}
		for name, token := range map[string]string{"unknown": unknown, "cancelled": cy.CancelToken, "ended": dee.CancelToken} {
			if code, body := move(token, startAt(at(13))); code != missCode || body != missBody {
				t.Errorf("%s booking's link: %d %s, want the bad token's %d %s", name, code, body, missCode, missBody)
			}
		}
		exec(`UPDATE booking_pages SET active = false WHERE slug = 'move-call'`)
		calendarBusiness.ForgetBookingPagesForTest()
		if code, body := move(alex.CancelToken, startAt(at(13))); code != missCode || body != missBody {
			t.Errorf("a closed page's booking: %d %s, want the bad token's %d %s", code, body, missCode, missBody)
		}
		if start, _ := row(alex.BookingID); !start.Equal(at(15)) {
			t.Errorf("a closed page's booking moved to %v", start)
		}
	})
}

// A booking's guest is reminded of it by email a day and an hour before (gcal-7
// H2b): the reminders are the page owner's scheduled jobs, carrying no link;
// a due one mails the guest once, with their link as its button; a guest's
// move plans the new start's only, and a cancelled booking's send nothing.
func TestBookingGuestReminders(t *testing.T) {
	integration.ForEachCalendarStore(t, func(t *testing.T) {
		ctx := context.Background()
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
		t.Setenv("RESEND_API_KEY", "re_booking_reminders_test")
		t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
		mailbox := &guestMailbox{}
		oldClient := emailService.SharedHTTPClient
		emailService.SharedHTTPClient = &http.Client{Transport: mailbox}
		t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })
		emailService.LetReservedNamesThroughForTest(t)
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := env.PG.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}

		sam := uuid.New()
		exec(`INSERT INTO users (id, email_id, username) VALUES ($1, 'sam@example.com', 'sam')`, sam)
		dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": sam.String(), "user_name": "Sam Rivera",
			"user_full_name": "Samuel Rivera", "user_email_id": "sam@example.com"})
		du, err := userDomain.GetDgraphUserInfoByUUID(ctx, sam.String())
		if err != nil || du == nil {
			t.Fatalf("sam: %v", err)
		}
		samInfo := &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: sam, EmailID: "sam@example.com"}, UserDgraphInfo: *du}
		if _, err := calendarBusiness.SaveBookingPage(ctx, sam, calendarBusiness.PageInput{
			Slug: "remind-call", Title: "Remind call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
			Hours: calendarBusiness.WorkingHours{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "09:00", End: "17:00", TZ: "UTC"},
		}); err != nil {
			t.Fatal(err)
		}
		day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
		at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
		mailsTo := func(addr string) []mailedMessage { return mailbox.to(addr) }

		// The booking's pending reminders, soonest first.
		type job struct {
			id      uuid.UUID
			runAt   time.Time
			payload string
		}
		pending := func(booking string) []job {
			t.Helper()
			rows, err := env.PG.Query(`SELECT id, run_at, payload::text FROM scheduled_jobs WHERE job_type = 'booking.guest_reminder'
				AND user_uuid = $1 AND status = 'pending' AND payload->>'booking_id' = $2 ORDER BY run_at`, sam, booking)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []job
			for rows.Next() {
				var j job
				if err := rows.Scan(&j.id, &j.runAt, &j.payload); err != nil {
					t.Fatal(err)
				}
				out = append(out, j)
			}
			return out
		}
		// fire runs a reminder as its time comes: its clock is moved to now.
		fire := func(id uuid.UUID) {
			t.Helper()
			exec(`UPDATE scheduled_jobs SET run_at = NOW() - interval '1 second' WHERE id = $1`, id)
			claimed, err := jobModel.ClaimUserJob(ctx, id, sam, "booking-reminders-test")
			if err != nil || claimed == nil {
				t.Fatalf("claiming reminder %s: %v", id, err)
			}
			schedulerBusiness.RunClaimed(ctx, claimed)
			settled()
		}
		plannedFor := func(jobs []job, start time.Time) bool {
			if len(jobs) != 2 {
				return false
			}
			for i, before := range []time.Duration{24 * time.Hour, time.Hour} {
				if !jobs[i].runAt.Equal(start.Add(-before)) {
					return false
				}
			}
			return true
		}
		// cancelled is how many of jobs are cancelled, softly.
		cancelled := func(jobs []job) int {
			t.Helper()
			n := 0
			for _, j := range jobs {
				var one int
				if err := env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE id = $1 AND status = 'cancelled' AND deleted_at IS NULL`,
					j.id).Scan(&one); err != nil {
					t.Fatal(err)
				}
				n += one
			}
			return n
		}

		alex, err := calendarBusiness.Book(ctx, "remind-call", calendarBusiness.BookInput{Start: at(10), Name: "Alex Kim",
			Email: "alex@example.com", TZ: "Asia/Kolkata"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		bookingEventually(t, "the booking's email", func() bool { return len(mailsTo("alex@example.com")) >= 1 })
		jobs := pending(alex.BookingID)
		if !plannedFor(jobs, at(10)) {
			t.Fatalf("the booking's reminders: %+v", jobs)
		}
		for _, j := range jobs {
			if strings.Contains(j.payload, alex.CancelToken) {
				t.Errorf("a reminder's payload carries the guest's link: %s", j.payload)
			}
		}

		// The hour's reminder comes due: one email, the guest's link its button.
		fire(jobs[1].id)
		sent := mailsTo("alex@example.com")
		if len(sent) != 2 {
			t.Fatalf("emails to the guest after the hour's reminder: %d, want 2", len(sent))
		}
		if last := sent[1]; !strings.Contains(last.Subject, "Remind call") || !strings.Contains(last.HTML, "/booking/"+alex.CancelToken) ||
			!strings.Contains(last.HTML, "Sam Rivera") || !strings.Contains(last.HTML, "Asia/Kolkata") {
			t.Errorf("the reminder: %q", last.Subject)
		}

		// The guest moves it: the new start's reminders only, the old one cancelled.
		if _, err := calendarBusiness.RescheduleBooking(ctx, alex.CancelToken, at(12), time.Now()); err != nil {
			t.Fatal(err)
		}
		bookingEventually(t, "the move's email", func() bool { return len(mailsTo("alex@example.com")) >= 3 })
		moved := pending(alex.BookingID)
		if !plannedFor(moved, at(12)) {
			t.Fatalf("the moved booking's reminders: %+v", moved)
		}
		if n := cancelled(jobs[:1]); n != 1 {
			t.Errorf("the old start's day reminder is still live: %d cancelled", n)
		}

		// Cancelled by its guest, the booking's reminders are cancelled too.
		if _, err := calendarBusiness.CancelBooking(ctx, alex.CancelToken); err != nil {
			t.Fatal(err)
		}
		bookingEventually(t, "the cancel's email", func() bool { return len(mailsTo("alex@example.com")) >= 4 })
		if left, n := pending(alex.BookingID), cancelled(moved); len(left) != 0 || n != 2 {
			t.Errorf("after the guest's cancel: %d reminders pending, %d of the two cancelled", len(left), n)
		}
		settled()
		if n := len(mailsTo("alex@example.com")); n != 4 {
			t.Errorf("emails to the guest after a cancelled booking: %d, want 4", n)
		}

		// The host moves a booking: the old start's reminders are cancelled
		// and the new start's two planned. Then the host deletes the meeting:
		// its reminders are cancelled with it.
		bo, err := calendarBusiness.Book(ctx, "remind-call", calendarBusiness.BookInput{Start: at(14), Name: "Bo Chen",
			Email: "bo@example.com", TZ: "UTC"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		bookingEventually(t, "Bo's booking email", func() bool { return len(mailsTo("bo@example.com")) >= 1 })
		first := pending(bo.BookingID)
		if !plannedFor(first, at(14)) {
			t.Fatalf("Bo's reminders: %+v", first)
		}
		var boEvent uuid.UUID
		if err := env.PG.QueryRow(`SELECT event_uuid FROM bookings WHERE id = $1`, bo.BookingID).Scan(&boEvent); err != nil {
			t.Fatal(err)
		}
		ev, err := calendarBusiness.GetEvent(ctx, samInfo, boEvent.String())
		if err != nil || ev == nil {
			t.Fatalf("Bo's meeting: %v", err)
		}
		if err := calendarBusiness.UpdateEvent(ctx, boEvent, adapter.CreateOrUpdateEventInput{
			Title: ev.Title, Description: ev.Description,
			StartTime: at(15).Format(time.RFC3339), EndTime: at(15).Add(30 * time.Minute).Format(time.RFC3339),
		}, samInfo); err != nil {
			t.Fatalf("the host's move: %v", err)
		}
		bookingEventually(t, "the host's move's email", func() bool { return len(mailsTo("bo@example.com")) >= 2 })
		again := pending(bo.BookingID)
		if !plannedFor(again, at(15)) || cancelled(first) != 2 {
			t.Errorf("after the host's move: pending %+v, %d of the old two cancelled", again, cancelled(first))
		}
		if err := calendarBusiness.DeleteEvent(ctx, boEvent, samInfo); err != nil {
			t.Fatalf("the host's cancel: %v", err)
		}
		bookingEventually(t, "the host's cancel's email", func() bool { return len(mailsTo("bo@example.com")) >= 3 })
		if left, n := pending(bo.BookingID), cancelled(again); len(left) != 0 || n != 2 {
			t.Errorf("after the host's cancel: %d reminders pending, %d of the two cancelled", len(left), n)
		}

		// CancelPendingFor counts what it cancelled: a booking's two pending
		// reminders, then none, as nothing is left pending.
		cy, err := calendarBusiness.Book(ctx, "remind-call", calendarBusiness.BookInput{Start: at(16), Name: "Cy Park",
			Email: "cy@example.com", TZ: "UTC"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		bookingEventually(t, "Cy's booking email", func() bool { return len(mailsTo("cy@example.com")) >= 1 })
		for i, want := range []int{2, 0} {
			if n, err := scheduledJobDomain.CancelPendingFor(ctx, sam, "booking.guest_reminder", "booking_id", cy.BookingID); err != nil || n != want {
				t.Errorf("CancelPendingFor, call %d: %d, %v; want %d", i+1, n, err, want)
			}
		}
		if left := pending(cy.BookingID); len(left) != 0 {
			t.Errorf("Cy's reminders after CancelPendingFor: %d pending", len(left))
		}
	})
}

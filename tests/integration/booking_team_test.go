//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/akashc777/OneCamp/tests/integration"
)

// gcal-7 H4a: a collective page is booked with its owner and every host who
// accepted. Over HTTP: the owner names two members; one accepts; both see the
// page in /hosting with their state; the accepted host's event takes a slot,
// the invited one's doesn't; a guest's booking lands with the accepted host as
// a participant and tells both; a move keeps them; nobody acts on another's
// host row or edits the page; a host who leaves stops counting.
func TestBookingTeamPages(t *testing.T) {
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
		t.Setenv("RESEND_API_KEY", "re_booking_team_test")
		t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
		mailbox := &guestMailbox{}
		oldClient := emailService.SharedHTTPClient
		emailService.SharedHTTPClient = &http.Client{Transport: mailbox}
		t.Cleanup(func() { emailService.SharedHTTPClient = oldClient })
		emailService.LetReservedNamesThroughForTest(t)

		person := func(name, full string, bot bool) *userModels.UserInfo {
			id := uuid.New()
			email := name + "@acme.test"
			if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, is_bot) VALUES ($1, $2, $3, $4)`, id, email, name, bot); err != nil {
				t.Fatal(err)
			}
			dg.Mutate(t, map[string]any{"dgraph.type": "User", "user_uuid": id.String(), "user_name": full,
				"user_full_name": full, "user_email_id": email})
			du, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
			if err != nil || du == nil {
				t.Fatalf("%s: %v", name, err)
			}
			return &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email}, UserDgraphInfo: *du}
		}
		sam, ana, ben := person("sam", "Sam Rivera", false), person("ana", "Ana Ortiz", false), person("ben", "Ben Okafor", false)
		eve, bot := person("eve", "Eve Stone", false), person("helper", "Helper Bot", true)

		r := chi.NewRouter()
		r.Get("/bookingPages", eventController.GetMyBookingPages)
		r.Post("/bookingPages", eventController.SaveMyBookingPage)
		r.Post("/bookingPages/delete", eventController.DeleteMyBookingPage)
		r.Get("/bookingPages/hosting", eventController.GetHostingBookingPages)
		r.Post("/bookingPages/hosting/accept", eventController.AcceptBookingHost)
		r.Post("/bookingPages/hosting/leave", eventController.LeaveBookingHost)
		r.Get("/public/book/{slug}", eventController.GetPublicBookingPage)
		r.Post("/public/book/{slug}", eventController.BookPublicSlot)
		r.Post("/public/booking/{token}/reschedule", eventController.ReschedulePublicBooking)
		r.Get("/user/notificationPreferences", notificationController.GetMyNotificationPreferences)
		call := func(t *testing.T, who *userModels.UserInfo, method, path string, body any, out any) (int, string) {
			t.Helper()
			var buf bytes.Buffer
			if body != nil {
				if err := json.NewEncoder(&buf).Encode(body); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(method, path, &buf)
			req.Header.Set("Content-Type", "application/json")
			if who != nil {
				req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, *who))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			var env struct {
				Data json.RawMessage `json:"data"`
				Msg  string          `json:"msg"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &env)
			if out != nil && rec.Code == http.StatusOK {
				if err := json.Unmarshal(env.Data, out); err != nil {
					t.Fatalf("%s %s: %v in %s", method, path, err, rec.Body.String())
				}
			}
			return rec.Code, env.Msg
		}

		type host struct {
			UserId string `json:"user_id"`
			Name   string `json:"name"`
			State  string `json:"state"`
		}
		type page struct {
			Id       string `json:"id"`
			TeamMode string `json:"team_mode"`
			Hosts    []host `json:"hosts"`
		}
		input := func(id string, hosts ...*userModels.UserInfo) map[string]any {
			ids := []string{}
			for _, h := range hosts {
				ids = append(ids, h.UserPostgresInfo.Id.String())
			}
			return map[string]any{"id": id, "slug": "team-call", "title": "Team call", "duration_minutes": 30,
				"max_days_ahead": 30, "active": true, "team_mode": "collective", "hosts": ids,
				"hours": map[string]any{"days": []int{0, 1, 2, 3, 4, 5, 6}, "start": "00:00", "end": "24:00", "tz": "UTC"}}
		}

		// Sam's and Ana's preference rows, so their notice mail goes.
		for _, who := range []*userModels.UserInfo{sam, ana} {
			if code, msg := call(t, who, http.MethodGet, "/user/notificationPreferences", nil, nil); code != http.StatusOK {
				t.Fatalf("preferences: %d %s", code, msg)
			}
		}

		// Only active human members host: a bot, a guest (an external
		// account) or a stranger is refused.
		guest := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, is_external) VALUES ($1, $2, $3, true)`,
			guest, "gina@example.com", "gina"); err != nil {
			t.Fatal(err)
		}
		const notAHost = "Only active members of this workspace can host a page: not bots, guests or people who have left."
		for what, h := range map[string]*userModels.UserInfo{"a bot": bot,
			"a guest":    {UserPostgresInfo: userModels.User{Id: guest}},
			"a stranger": {UserPostgresInfo: userModels.User{Id: uuid.New()}}} {
			if code, msg := call(t, sam, http.MethodPost, "/bookingPages", input("", h), nil); code != http.StatusBadRequest || msg != notAHost {
				t.Fatalf("%s as host: %d %q; want a 400 %q", what, code, msg, notAHost)
			}
		}
		var saved page
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", input("", ana, ben), &saved); code != http.StatusOK {
			t.Fatalf("save: %d %s", code, msg)
		}
		// Notice mail is queued for its sender (cmd/server's worker, which no
		// test runs), so who was told is who has a queued row; the mailbox
		// sees only mail sent straight away, as a guest's invite is.
		queued := func(who *userModels.UserInfo, subject string) int {
			t.Helper()
			var n int
			if err := env.PG.QueryRow(`SELECT COUNT(*) FROM notification_email_queue WHERE user_id = $1 AND subject LIKE $2`,
				who.UserPostgresInfo.Id, "%"+subject+"%").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		bookingEventually(t, "Ana told she's invited", func() bool { return queued(ana, "asked you to host") > 0 })

		pageID := map[string]string{"page_id": saved.Id}
		// Eve can't accept or leave Ana's row, nor edit Sam's page.
		if code, _ := call(t, eve, http.MethodPost, "/bookingPages/hosting/accept", pageID, nil); code != http.StatusNotFound {
			t.Fatalf("Eve accepts a page she isn't invited to: %d; want 404", code)
		}
		if code, _ := call(t, eve, http.MethodPost, "/bookingPages", input(saved.Id, eve), nil); code == http.StatusOK {
			t.Fatal("Eve edited Sam's page")
		}
		if code, msg := call(t, ana, http.MethodPost, "/bookingPages/hosting/accept", pageID, nil); code != http.StatusOK {
			t.Fatalf("Ana accepts: %d %s", code, msg)
		}
		if code, _ := call(t, eve, http.MethodPost, "/bookingPages/hosting/leave", pageID, nil); code != http.StatusNotFound {
			t.Fatalf("Eve leaves a page she doesn't host: %d; want 404", code)
		}

		type hosted struct {
			PageId    string `json:"page_id"`
			Title     string `json:"title"`
			OwnerName string `json:"owner_name"`
			State     string `json:"state"`
		}
		for who, want := range map[*userModels.UserInfo]string{ana: "accepted", ben: "invited"} {
			var list []hosted
			if code, msg := call(t, who, http.MethodGet, "/bookingPages/hosting", nil, &list); code != http.StatusOK {
				t.Fatalf("hosting: %d %s", code, msg)
			}
			if len(list) != 1 || list[0].PageId != saved.Id || list[0].State != want || list[0].OwnerName != "Sam Rivera" || list[0].Title != "Team call" {
				t.Fatalf("%s's hosting: %+v; want the team page, %s, by Sam Rivera", who.UserPostgresInfo.EmailID, list, want)
			}
		}
		var mine []page
		if call(t, sam, http.MethodGet, "/bookingPages", nil, &mine); len(mine) != 1 || len(mine[0].Hosts) != 2 || mine[0].TeamMode != "collective" {
			t.Fatalf("Sam's pages: %+v; want the team page with two hosts", mine)
		}
		states := map[string]string{}
		for _, h := range mine[0].Hosts {
			states[h.Name] = h.State
		}
		if states["Ana Ortiz"] != "accepted" || states["Ben Okafor"] != "invited" {
			t.Fatalf("Sam's hosts: %v; want Ana accepted, Ben invited", states)
		}

		// The web's editor sends neither team_mode nor hosts: a save of the
		// title alone keeps the team page and every host's row as it was,
		// with nobody invited again or let go.
		hostRows := func() string {
			t.Helper()
			var s string
			if err := env.PG.QueryRow(`SELECT string_agg(user_id::text || ' ' || invited_at::text || ' ' ||
				COALESCE(accepted_at::text, '-') || ' ' || COALESCE(left_at::text, '-'), ',' ORDER BY user_id)
				FROM booking_page_hosts WHERE page_id = $1`, saved.Id).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		hostMail := func() int { return queued(ana, "asked you to host") + queued(ben, "asked you to host") }
		rowsBefore, mailBefore := hostRows(), hostMail()
		webSave := input(saved.Id)
		delete(webSave, "team_mode")
		delete(webSave, "hosts")
		webSave["title"] = "Team call, renamed"
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", webSave, nil); code != http.StatusOK {
			t.Fatalf("a save without team_mode or hosts: %d %s", code, msg)
		}
		if rows := hostRows(); rows != rowsBefore {
			t.Fatalf("host rows after a save without hosts: %s; want them as they were: %s", rows, rowsBefore)
		}
		if call(t, sam, http.MethodGet, "/bookingPages", nil, &mine); len(mine) != 1 || mine[0].TeamMode != "collective" || len(mine[0].Hosts) != 2 {
			t.Fatalf("Sam's pages after a save without team_mode: %+v; want the team page with two hosts", mine)
		}
		if n := hostMail(); n != mailBefore {
			t.Fatalf("host mail after a save without hosts: %d; want %d, nobody invited again", n, mailBefore)
		}

		// Ana's own event takes 10:00; Ben's 11:00 doesn't count.
		day := helpers.DayStart(time.Now().UTC(), time.UTC).AddDate(0, 0, 2)
		at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
		ownEvent := func(who *userModels.UserInfo, h int) {
			t.Helper()
			if _, err := calendarBusiness.CreateEvent(ctx, who, adapter.CreateOrUpdateEventInput{Title: "Dentist",
				StartTime: at(h).Format(time.RFC3339), EndTime: at(h).Add(30 * time.Minute).Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
		}
		ownEvent(ana, 10)
		ownEvent(ben, 11)
		type public struct {
			HostNames []string `json:"host_names"`
			Slots     []struct {
				Start time.Time `json:"start"`
			} `json:"slots"`
		}
		offered := func(h int) (bool, public) {
			t.Helper()
			var p public
			if code, msg := call(t, nil, http.MethodGet, "/public/book/team-call?from="+day.Format(time.RFC3339), nil, &p); code != http.StatusOK {
				t.Fatalf("public page: %d %s", code, msg)
			}
			for _, s := range p.Slots {
				if s.Start.Equal(at(h)) {
					return true, p
				}
			}
			return false, p
		}
		if ok, p := offered(10); ok || len(p.HostNames) != 1 || p.HostNames[0] != "Ana Ortiz" {
			t.Fatalf("10:00 offered %v, hosts %v; want Ana's 10:00 taken, Ana named alone", ok, p.HostNames)
		}
		if ok, _ := offered(11); !ok {
			t.Fatal("11:00 not offered: Ben (invited) counted")
		}

		// A guest books 12:00: the event carries Ana, and both are told.
		var booked struct {
			CancelToken string `json:"cancel_token"`
		}
		if code, msg := call(t, nil, http.MethodPost, "/public/book/team-call", map[string]any{"start": at(12),
			"name": "Alex Kim", "email": "alex@guest.test", "tz": "UTC"}, &booked); code != http.StatusOK {
			t.Fatalf("book: %d %s", code, msg)
		}
		busyAt := func(who *userModels.UserInfo, h int) bool {
			t.Helper()
			b, err := calendarBusiness.ReadBusy(ctx, who.UserPostgresInfo.Id.String(), day, day.Add(24*time.Hour), calendarBusiness.BusyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range b.Intervals {
				if i.Start.Equal(at(h)) {
					return true
				}
			}
			return false
		}
		if !busyAt(ana, 12) || busyAt(ben, 12) {
			t.Fatalf("12:00 busy for Ana %v, Ben %v; want Ana alone", busyAt(ana, 12), busyAt(ben, 12))
		}
		for _, who := range []*userModels.UserInfo{sam, ana} {
			bookingEventually(t, who.UserPostgresInfo.EmailID+" told of the booking", func() bool {
				return queued(who, "booked") > 0
			})
		}
		// The guest moves it to 13:00: Ana goes with it.
		if code, msg := call(t, nil, http.MethodPost, "/public/booking/"+booked.CancelToken+"/reschedule",
			map[string]any{"start": at(13)}, nil); code != http.StatusOK {
			t.Fatalf("move: %d %s", code, msg)
		}
		if !busyAt(ana, 13) || busyAt(ana, 12) {
			t.Fatal("after the move: Ana isn't at 13:00, or still at 12:00")
		}

		// Ana leaves: her 10:00 no longer takes the slot.
		if code, msg := call(t, ana, http.MethodPost, "/bookingPages/hosting/leave", pageID, nil); code != http.StatusOK {
			t.Fatalf("Ana leaves: %d %s", code, msg)
		}
		if ok, p := offered(10); !ok || len(p.HostNames) != 0 {
			t.Fatalf("after Ana left: 10:00 offered %v, hosts %v; want offered, nobody named", ok, p.HostNames)
		}
		var rows int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM booking_page_hosts WHERE page_id = $1`, saved.Id).Scan(&rows); err != nil || rows != 2 {
			t.Fatalf("host rows %d %v; want both kept (a leave is soft)", rows, err)
		}

		// An explicit empty list still takes every host off; the team mode,
		// left out, stays.
		noHosts := input(saved.Id)
		delete(noHosts, "team_mode")
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", noHosts, nil); code != http.StatusOK {
			t.Fatalf("a save with no hosts: %d %s", code, msg)
		}
		var live int
		if err := env.PG.QueryRow(`SELECT COUNT(*) FROM booking_page_hosts WHERE page_id = $1 AND left_at IS NULL`, saved.Id).Scan(&live); err != nil || live != 0 {
			t.Fatalf("live host rows after hosts: [] %d %v; want none", live, err)
		}
		if call(t, sam, http.MethodGet, "/bookingPages", nil, &mine); len(mine) != 1 || mine[0].TeamMode != "collective" {
			t.Fatalf("Sam's pages after hosts: []: %+v; want the team page kept", mine)
		}

		// Eve sends Sam's solo page with hosts and no team_mode, to borrow
		// the stored mode: she hears only that it isn't hers, never what the
		// page is (pageKeptFrom's owner match), and nothing changes.
		var solo page
		soloIn := input("")
		soloIn["slug"], soloIn["title"], soloIn["team_mode"] = "sam-solo", "Sam alone", "solo"
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", soloIn, &solo); code != http.StatusOK {
			t.Fatalf("Sam's solo page: %d %s", code, msg)
		}
		soloRow := func() string {
			t.Helper()
			var s string
			if err := env.PG.QueryRow(`SELECT p.user_id::text || ' ' || p.title || ' ' || COALESCE(p.team_mode, '') || ' ' ||
				p.updated_at::text || ' ' || (SELECT COUNT(*) FROM booking_page_hosts h WHERE h.page_id = p.id)::text
				FROM booking_pages p WHERE p.id = $1`, solo.Id).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		soloBefore := soloRow()
		borrow := input(solo.Id, ana)
		delete(borrow, "team_mode")
		borrow["slug"], borrow["title"] = "sam-solo", "Eve's now"
		if code, msg := call(t, eve, http.MethodPost, "/bookingPages", borrow, nil); code != http.StatusBadRequest || msg != "That isn't one of your pages." {
			t.Fatalf("Eve saves Sam's solo page with hosts, no team_mode: %d %q; want a 400 \"That isn't one of your pages.\"", code, msg)
		}
		if after := soloRow(); after != soloBefore {
			t.Fatalf("Sam's solo page after Eve's save: %s; want it as it was: %s", after, soloBefore)
		}

		// Once Sam deletes a team page, its host answers like a stranger:
		// accept and leave are 404s, and /hosting drops it.
		var two page
		twoIn := input("", ben)
		twoIn["slug"], twoIn["title"] = "team-two", "Team two"
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", twoIn, &two); code != http.StatusOK {
			t.Fatalf("Sam's second team page: %d %s", code, msg)
		}
		benHosts := func() []hosted {
			t.Helper()
			var list []hosted
			if code, msg := call(t, ben, http.MethodGet, "/bookingPages/hosting", nil, &list); code != http.StatusOK {
				t.Fatalf("Ben's hosting: %d %s", code, msg)
			}
			return list
		}
		if list := benHosts(); len(list) != 1 || list[0].PageId != two.Id || list[0].State != "invited" {
			t.Fatalf("Ben's hosting before the delete: %+v; want team two, invited", list)
		}
		twoID := map[string]string{"page_id": two.Id}
		if code, msg := call(t, ben, http.MethodPost, "/bookingPages/hosting/accept", twoID, nil); code != http.StatusOK {
			t.Fatalf("Ben accepts team two: %d %s", code, msg)
		}
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages/delete", map[string]string{"id": two.Id}, nil); code != http.StatusOK {
			t.Fatalf("Sam deletes team two: %d %s", code, msg)
		}
		for _, verb := range []string{"accept", "leave"} {
			if code, _ := call(t, ben, http.MethodPost, "/bookingPages/hosting/"+verb, twoID, nil); code != http.StatusNotFound {
				t.Fatalf("Ben's %s on a deleted page: %d; want 404", verb, code)
			}
		}
		if list := benHosts(); len(list) != 0 {
			t.Fatalf("Ben's hosting after the delete: %+v; want nothing", list)
		}

		// CR-452 F2: the delete let Ben go, so a page made again on the
		// address asks him afresh: invited, not accepted, with a new invite.
		benRow := func() string {
			t.Helper()
			var s string
			if err := env.PG.QueryRow(`SELECT (h.accepted_at IS NOT NULL)::text || ' ' || (h.left_at IS NOT NULL)::text
				FROM booking_page_hosts h JOIN booking_pages p ON p.id = h.page_id
				WHERE p.slug = 'team-two' AND p.deleted_at IS NULL AND h.user_id = $1`, ben.UserPostgresInfo.Id).Scan(&s); err != nil {
				t.Fatal(err)
			}
			return s
		}
		// The fresh invite shows as a new invited_at: its mail is queued
		// under the first invite's key, still pending here (no worker drains
		// the queue in a test), so the queue keeps one row.
		benInvitedAt := func() time.Time {
			t.Helper()
			var at time.Time
			if err := env.PG.QueryRow(`SELECT h.invited_at FROM booking_page_hosts h JOIN booking_pages p ON p.id = h.page_id
				WHERE p.slug = 'team-two' AND h.user_id = $1`, ben.UserPostgresInfo.Id).Scan(&at); err != nil {
				t.Fatal(err)
			}
			return at
		}
		invitedBefore := benInvitedAt()
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", twoIn, &two); code != http.StatusOK {
			t.Fatalf("Sam makes team two again: %d %s", code, msg)
		}
		if row := benRow(); row != "false false" {
			t.Fatalf("Ben on team two made again (accepted, left): %s; want invited afresh: false false", row)
		}
		if at := benInvitedAt(); !at.After(invitedBefore) {
			t.Fatalf("Ben's invite on team two made again: %v; want after the first, %v", at, invitedBefore)
		}

		// CR-452 F1: a host who leaves the workspace isn't a host: the public
		// page neither names him nor counts his busy time.
		twoID = map[string]string{"page_id": two.Id}
		if code, msg := call(t, ben, http.MethodPost, "/bookingPages/hosting/accept", twoID, nil); code != http.StatusOK {
			t.Fatalf("Ben accepts team two again: %d %s", code, msg)
		}
		offeredOn := func(slug string, h int) (bool, public) {
			t.Helper()
			var p public
			if code, msg := call(t, nil, http.MethodGet, "/public/book/"+slug+"?from="+day.Format(time.RFC3339), nil, &p); code != http.StatusOK {
				t.Fatalf("public page %s: %d %s", slug, code, msg)
			}
			for _, s := range p.Slots {
				if s.Start.Equal(at(h)) {
					return true, p
				}
			}
			return false, p
		}
		if ok, p := offeredOn("team-two", 11); ok || len(p.HostNames) != 1 || p.HostNames[0] != "Ben Okafor" {
			t.Fatalf("team two at 11:00 offered %v, hosts %v; want Ben's 11:00 taken, Ben named", ok, p.HostNames)
		}
		if _, err := env.PG.Exec(`UPDATE users SET deleted_at = now() WHERE id = $1`, ben.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
		calendarBusiness.ForgetBookingPagesForTest()
		if ok, p := offeredOn("team-two", 11); !ok || len(p.HostNames) != 0 {
			t.Fatalf("after Ben left the workspace: 11:00 offered %v, hosts %v; want offered, nobody named", ok, p.HostNames)
		}

		// CR-452 F3: a closed team page answers exactly as a missing one.
		closeIn := input(two.Id)
		delete(closeIn, "team_mode")
		delete(closeIn, "hosts")
		closeIn["slug"], closeIn["title"], closeIn["active"] = "team-two", "Team two", false
		if code, msg := call(t, sam, http.MethodPost, "/bookingPages", closeIn, nil); code != http.StatusOK {
			t.Fatalf("Sam closes team two: %d %s", code, msg)
		}
		raw := func(slug string) string {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/book/"+slug+"?from="+day.Format(time.RFC3339), nil))
			return fmt.Sprint(rec.Code, " ", rec.Body.String())
		}
		if closed, missing := raw("team-two"), raw("no-such-team"); closed != missing {
			t.Fatalf("closed team page: %s; want it as a missing one: %s", closed, missing)
		}
	})
}

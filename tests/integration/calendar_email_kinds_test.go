//go:build integration
// +build integration

package integration_test

// Calendar head E (gcal-5): each person chooses calendar emails by kind
// (invitations, changes, replies to their events), as Google lets them, over
// HTTP and Postgres through the real dispatcher; and migration 578 up, down
// and up.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCalendarEmailsByKindOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	t.Setenv("RESEND_API_KEY", "re_calendar_email_kinds_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")

	// Each reads only their own switches; everyone starts with every kind on.
	kinds := func(who *userModels.UserInfo) map[string]any {
		t.Helper()
		rec := call(t, notificationController.GetMyNotificationPreferences, http.MethodGet, "/user/notificationPreferences", who, "", nil)
		var out struct {
			Data map[string]any `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("%s's preferences: %d %s", who.UserDgraphInfo.UserName, rec.Code, rec.Body.String())
		}
		return out.Data
	}
	want := func(who *userModels.UserInfo, invites, changes, answers bool) {
		t.Helper()
		got := kinds(who)
		if got["email_calendar_invites"] != invites || got["email_calendar_changes"] != changes || got["email_calendar_answers"] != answers {
			t.Errorf("%s's calendar emails: invites %v, changes %v, answers %v; want %v %v %v", who.UserDgraphInfo.UserName,
				got["email_calendar_invites"], got["email_calendar_changes"], got["email_calendar_answers"], invites, changes, answers)
		}
	}
	for _, who := range []*userModels.UserInfo{sam, maya, jonas} {
		want(who, true, true, true)
	}
	set := func(who *userModels.UserInfo, body string) {
		t.Helper()
		if rec := call(t, notificationController.UpdateMyNotificationPreferences, http.MethodPost, "/user/notificationPreferences", who, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s sets %s: %d %s", who.UserDgraphInfo.UserName, body, rec.Code, rec.Body.String())
		}
	}
	// Jonas wants invitations and replies, not changes; Maya keeps them all.
	set(jonas, `{"email_calendar_changes":false}`)
	want(jonas, true, false, true)
	want(maya, true, true, true)

	// A change is told off the request (business/Calendar liveTeller): what
	// it emailed is counted once the telling has settled.
	emails := func(who *userModels.UserInfo, kind string) int {
		t.Helper()
		if running := helpers.SettleBackground(30 * time.Second); len(running) > 0 {
			t.Logf("still running after 30s: %v", running)
		}
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = $2`,
			who.UserPostgresInfo.Id, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Sam invites both, and asks to hear their answers by email.
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+rfc(start)+`","zone":"Europe/London"},"end":{"at":"`+rfc(start.Add(time.Hour))+`"},`+
			`"email_on_answers":true,"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"},{"user_uuid":"`+jonas.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	if m, j := emails(maya, "calendar.invited"), emails(jonas, "calendar.invited"); m != 1 || j != 1 {
		t.Errorf("invitations emailed: Maya %d, Jonas %d; want 1 each", m, j)
	}

	// Sam moves it an hour later: Maya is emailed; Jonas, changes off, isn't.
	code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+e.ID, sam,
		`{"sequence":`+itoa(e.Sequence)+`,"start":{"at":"`+rfc(start.Add(time.Hour))+`","zone":"Europe/London"},"end":{"at":"`+rfc(start.Add(2*time.Hour))+`"}}`, e.ID)
	if code != http.StatusOK {
		t.Fatalf("move: %d %v", code, body)
	}
	if m, j := emails(maya, "calendar.updated"), emails(jonas, "calendar.updated"); m != 1 || j != 0 {
		t.Errorf("the move emailed Maya %d, Jonas %d; want 1 and 0", m, j)
	}

	// Replies: Maya's reaches Sam by email; once Sam turns replies off,
	// Jonas's doesn't (it is in his Inbox all the same).
	answer := func(who *userModels.UserInfo) {
		t.Helper()
		if code, body := v2(t, eventController.AnswerCalendarEvent, http.MethodPost, "/calendar/events/"+e.ID+"/answer", who, `{"answer":"accepted"}`, e.ID); code != http.StatusOK {
			t.Fatalf("%s's answer: %d %v", who.UserDgraphInfo.UserName, code, body)
		}
	}
	answer(maya)
	if n := emails(sam, "calendar.rsvp"); n != 1 {
		t.Errorf("Maya's answer emailed Sam %d times; want 1", n)
	}
	set(sam, `{"email_calendar_answers":false}`)
	want(sam, true, true, false)
	answer(jonas)
	if n := emails(sam, "calendar.rsvp"); n != 1 {
		t.Errorf("with replies off, Sam was emailed %d replies; want still 1", n)
	}

	// Sam cancels it: Maya is emailed; Jonas, changes off, isn't.
	if code, body := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+e.ID, sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("cancel: %d %v", code, body)
	}
	if m, j := emails(maya, "calendar.cancelled"), emails(jonas, "calendar.cancelled"); m != 1 || j != 0 {
		t.Errorf("the cancellation emailed Maya %d, Jonas %d; want 1 and 0", m, j)
	}

	// A person turning everything back on hears of changes again; email off
	// still stops every kind.
	set(jonas, `{"email_calendar_changes":true,"email_enabled":false}`)
	want(jonas, true, true, true)
	code, body = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Retro","start":{"at":"`+rfc(start.Add(4*time.Hour))+`"},"end":{"at":"`+rfc(start.Add(5*time.Hour))+`"},`+
			`"attendees":[{"user_uuid":"`+jonas.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("retro: %d %v", code, body)
	}
	if n := emails(jonas, "calendar.invited"); n != 1 {
		t.Errorf("with email off, Jonas was emailed %d invitations; want still 1", n)
	}
}

func TestCalendarEmailKindsMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "578_calendar_email_kinds."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	// A person who was there before keeps every calendar email, as before.
	if _, err := env.PG.Exec(down); err != nil {
		t.Fatal(err)
	}
	before := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'before-' || $2 || '@example.test', 'before' || $2, 'Before', NOW(), NOW())`, before, before.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, before, "unsub-"+before.String()); err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		sql  string
		want int
	}{{up, 3}, {down, 0}, {up, 3}, {down, 0}, {up, 3}} {
		if _, err := env.PG.Exec(step.sql); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		var columns int
		if err := env.PG.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users_notification_preferences'
			AND column_name IN ('email_calendar_invites', 'email_calendar_changes', 'email_calendar_answers')
			AND is_nullable = 'NO' AND column_default = 'true'`).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != step.want {
			t.Errorf("step %d: %d calendar email columns, want %d", i, columns, step.want)
		}
	}
	var invites, changes, answers bool
	if err := env.PG.QueryRow(`SELECT email_calendar_invites, email_calendar_changes, email_calendar_answers
		FROM users_notification_preferences WHERE user_id = $1`, before).Scan(&invites, &changes, &answers); err != nil ||
		!invites || !changes || !answers {
		t.Errorf("a person from before 578: %v %v %v %v, want every calendar email on", invites, changes, answers, err)
	}
}

//go:build integration
// +build integration

package integration_test

// Calendar head A (gcal-5): a reminder by email and defaults per calendar,
// over HTTP and Postgres, and migration 573 up, down and up.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	preferencesController "github.com/akashc777/OneCamp/controllers/Preferences"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/lib/pq"
)

func TestRemindersByEmailAndDefaultsPerCalendarOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	samID := sam.UserPostgresInfo.Id

	start := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Minute)
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+rfc(start)+`","zone":"Europe/London"},"end":{"at":"`+rfc(start.Add(time.Hour))+`"}}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	// The create's reminders sink runs after it returns (V-730): settled, it
	// can't undo the reminders and the replan asserted below.
	if !resourceEventBusiness.Sinks.Settle(30 * time.Second) {
		t.Fatal("the design review's sinks didn't settle")
	}
	var rem struct {
		Minutes []int `json:"minutes"`
		Email   []int `json:"email"`
		Default bool  `json:"default"`
	}
	put := func(value string) (int, map[string]json.RawMessage) {
		return v2(t, eventController.SetCalendarReminders, http.MethodPut, "/calendar/events/"+e.ID+"/reminders", sam, value, e.ID)
	}
	payloadOf := func() map[string]any {
		var raw string
		if err := env.PG.QueryRow(`SELECT payload FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending'
			AND user_uuid = $1 AND payload->>'event_id' = $2 AND payload->>'kind' = 'next'`, samID, e.ID).Scan(&raw); err != nil {
			t.Fatalf("Sam's waiting reminder: %v", err)
		}
		var p map[string]any
		_ = json.Unmarshal([]byte(raw), &p)
		return p
	}

	// The web's list carries them as the card shows them: Sam's own, or his
	// default, and which of them come by email (one read for the list).
	listed := func(step string, minutes, email []int, def bool) {
		t.Helper()
		q := url.Values{"startDate": {rfc(start.Add(-time.Hour))}, "endDate": {rfc(start.Add(2 * time.Hour))}}
		rec := call(t, eventController.GetEventsListController, http.MethodGet, "/event/getEvents?"+q.Encode(), sam, "", nil)
		var list struct {
			Data []struct {
				ID      string `json:"event_uuid"`
				Minutes []int  `json:"event_reminders"`
				Email   []int  `json:"event_reminders_email"`
				Default bool   `json:"event_reminders_default"`
			} `json:"data"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
			t.Fatalf("%s, the list: %d %s", step, rec.Code, rec.Body.String())
		}
		for _, got := range list.Data {
			if got.ID == e.ID && (!slices.Equal(got.Minutes, minutes) || !slices.Equal(got.Email, email) || got.Default != def) {
				t.Errorf("%s, the list: %+v", step, got)
			}
			if got.ID == e.ID {
				return
			}
		}
		t.Errorf("%s: the event isn't listed", step)
	}

	// The hour's reminder by email: kept, read back, and planned so.
	code, body = put(`{"minutes":[10,60],"email":[60]}`)
	if code != 200 || json.Unmarshal(body["data"], &rem) != nil || !slices.Equal(rem.Minutes, []int{10, 60}) || !slices.Equal(rem.Email, []int{60}) {
		t.Fatalf("set: %d %v", code, body)
	}
	var stored pq.Int64Array
	if err := env.PG.QueryRow(`SELECT email_minutes FROM event_reminders WHERE event_id = $1 AND user_id = $2`, e.ID, samID).Scan(&stored); err != nil ||
		len(stored) != 1 || stored[0] != 60 {
		t.Errorf("email_minutes: %v %v", stored, err)
	}
	if p := payloadOf(); p["minutes"] != float64(60) || p["email"] != true {
		t.Errorf("the hour's reminder is planned by email: %v", p)
	}
	listed("own", []int{10, 60}, []int{60}, false)
	if code, body := put(`{"minutes":[10],"email":[20]}`); code != http.StatusBadRequest {
		t.Errorf("an email reminder that isn't a reminder: %d %v", code, body)
	}
	// Postgres holds the line too.
	if _, err := env.PG.Exec(`UPDATE event_reminders SET email_minutes = '{20}' WHERE event_id = $1`, e.ID); err == nil {
		t.Error("event_reminders_email_check let an email reminder outside minutes in")
	}

	// Back to the default, then a default of mine: half an hour, by email.
	if code, body := put(`{"minutes":null}`); code != 200 {
		t.Fatalf("back to the default: %d %v", code, body)
	}
	setPref := func(value string) int {
		rec := call(t, preferencesController.SetMine, http.MethodPut, "/user/preferences/calendar.default_reminders", sam,
			`{"value":`+value+`}`, map[string]string{"key": "calendar.default_reminders"})
		return rec.Code
	}
	if code := setPref(`{"mine":{"timed":{"minutes":[10],"email":[20]}}}`); code == 200 {
		t.Error("a default with an email reminder outside its minutes was kept")
	}
	before := time.Now()
	if code := setPref(`{"mine":{"timed":{"minutes":[30],"email":[30]}}}`); code != 200 {
		t.Fatalf("default: %d", code)
	}
	// The change plans Sam's coming events again, now: one wake-up for this one.
	var runAt time.Time
	if err := env.PG.QueryRow(`SELECT run_at FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending'
		AND user_uuid = $1 AND payload->>'event_id' = $2 AND payload->>'kind' = 'next' AND payload->>'replan' = 'true'`, samID, e.ID).Scan(&runAt); err != nil ||
		runAt.After(time.Now().Add(time.Second)) || runAt.Before(before.Add(-time.Minute)) {
		t.Errorf("replanned now: %v %v", runAt, err)
	}
	code, body = v2(t, eventController.GetCalendarReminders, http.MethodGet, "/calendar/events/"+e.ID+"/reminders", sam, "", e.ID)
	if code != 200 || json.Unmarshal(body["data"], &rem) != nil || !rem.Default || !slices.Equal(rem.Minutes, []int{30}) || !slices.Equal(rem.Email, []int{30}) {
		t.Errorf("the default as read: %d %v", code, body)
	}
	listed("default", []int{30}, []int{30}, true)

	// The replan is part of the change (V-532): when Sam's coming reminders
	// can't be planned again, his new default isn't kept either, and he is
	// told in words to try again, never a default his reminders don't follow.
	refuse := "v532_replan_" + strings.ReplaceAll(samID.String(), "-", "")
	t.Cleanup(func() {
		_, _ = env.PG.Exec(`DROP TRIGGER IF EXISTS ` + refuse + ` ON scheduled_jobs`)
		_, _ = env.PG.Exec(`DROP FUNCTION IF EXISTS ` + refuse + `()`)
	})
	for _, q := range []string{
		`CREATE FUNCTION ` + refuse + `() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.user_uuid = '` + samID.String() + `'::uuid AND NEW.job_type = 'calendar_reminder' THEN
				RAISE EXCEPTION 'test: Sam''s reminders can''t be planned';
			END IF;
			RETURN NEW;
		END $$`,
		`CREATE TRIGGER ` + refuse + ` BEFORE INSERT OR UPDATE ON scheduled_jobs FOR EACH ROW EXECUTE PROCEDURE ` + refuse + `()`,
	} {
		if _, err := env.PG.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	kept := string(prefs(t, sam)["calendar.default_reminders"])
	rec := call(t, preferencesController.SetMine, http.MethodPut, "/user/preferences/calendar.default_reminders", sam,
		`{"value":{"mine":{"timed":{"minutes":[45]}}}}`, map[string]string{"key": "calendar.default_reminders"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Couldn't save that. Try again in a moment.") {
		t.Errorf("a default whose replan fails: %d %s, want 503 and words to try again", rec.Code, rec.Body.String())
	}
	if got := string(prefs(t, sam)["calendar.default_reminders"]); got != kept {
		t.Errorf("a default whose replan failed was kept: %s, want %s as before", got, kept)
	}
	if _, err := env.PG.Exec(`DROP TRIGGER ` + refuse + ` ON scheduled_jobs`); err != nil {
		t.Fatal(err)
	}

	// A member guest's default reaches the events she is on: Maya, a guest of
	// Sam's standup, sets one, and her wake-up for it is planned now; Sam's
	// design review, which she isn't on, gets none for her.
	code, body = v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Standup","start":{"at":"`+rfc(start.Add(2*time.Hour))+`"},"end":{"at":"`+rfc(start.Add(150*time.Minute))+`"},"notify":"none",`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("Sam's standup with Maya: %d %v", code, body)
	}
	standup := dataEvent(t, body)
	// Settled before Maya's change: run after it, the standup's sink would
	// replace her replan (V-730).
	if !resourceEventBusiness.Sinks.Settle(30 * time.Second) {
		t.Fatal("the standup's sinks didn't settle")
	}
	mayaID, beforeMaya := maya.UserPostgresInfo.Id, time.Now()
	if rec := call(t, preferencesController.SetMine, http.MethodPut, "/user/preferences/calendar.default_reminders", maya,
		`{"value":{"mine":{"timed":{"minutes":[15]}}}}`, map[string]string{"key": "calendar.default_reminders"}); rec.Code != 200 {
		t.Fatalf("Maya's default: %d", rec.Code)
	}
	replannedFor := func(person any, event string) int {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE job_type = 'calendar_reminder' AND status = 'pending'
			AND user_uuid = $1 AND payload->>'event_id' = $2 AND payload->>'replan' = 'true' AND run_at >= $3`,
			person, event, beforeMaya.Add(-time.Minute)).Scan(&n); err != nil {
			t.Fatalf("replanned: %v", err)
		}
		return n
	}
	if n := replannedFor(mayaID, standup.ID); n != 1 {
		t.Errorf("Maya's default didn't replan the standup she's a guest of: %d", n)
	}
	if n := replannedFor(mayaID, e.ID); n != 0 {
		t.Errorf("Maya's default replanned Sam's design review, which she isn't on: %d", n)
	}

	// The email itself: asked for, it is queued for Maya though she never
	// turned email on; told as any other notice, it isn't.
	t.Setenv("RESEND_API_KEY", "re_calendar_reminder_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	evt := notificationBusiness.Event{Type: notificationBusiness.EventCalendarReminder, Title: "Design review in 1 hour", Body: "at 10:15",
		CTAURL: notificationBusiness.AppURL("/app/calendar/event/" + e.ID), DedupKeyParts: []string{"reminder-test-" + e.ID},
		Recipients: []notificationBusiness.Recipient{{UserUUID: maya.UserPostgresInfo.Id}}, SkipOnlineCheck: true}
	if n := notificationBusiness.Dispatch(context.Background(), evt); n != 0 {
		t.Errorf("an email Maya never asked for was queued: %d", n)
	}
	evt.Requested = true
	if n := notificationBusiness.Dispatch(context.Background(), evt); n != 1 {
		t.Fatalf("the reminder Maya asked for by email: %d queued", n)
	}
	var queued int
	if err := env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'calendar.reminder' AND status = 'pending'`,
		maya.UserPostgresInfo.Id).Scan(&queued); err != nil || queued != 1 {
		t.Errorf("queued for Maya: %d %v", queued, err)
	}

	// Sam has a preference row, so an unsubscribe token: a reminder he asked
	// for by email still carries no unsubscribe link, since that unsubscribe
	// never stops it, and says where to stop it instead.
	if _, err := prefModels.EnsureForUser(samID, "unsub-token-sam"); err != nil {
		t.Fatalf("Sam's preference row: %v", err)
	}
	evt.Recipients = []notificationBusiness.Recipient{{UserUUID: samID}}
	evt.DedupKeyParts = []string{"reminder-test-sam-" + e.ID}
	evt.Why = "To stop it, change or remove the reminder on the event."
	if n := notificationBusiness.Dispatch(context.Background(), evt); n != 1 {
		t.Fatalf("the reminder Sam asked for by email: %d queued", n)
	}
	var meta, html, text string
	if err := env.PG.QueryRow(`SELECT coalesce(metadata::text, '{}'), html_body, text_body FROM notification_email_queue
		WHERE user_id = $1 AND event_type = 'calendar.reminder'`, samID).Scan(&meta, &html, &text); err != nil {
		t.Fatalf("Sam's queued reminder: %v", err)
	}
	var queuedMeta map[string]string
	_ = json.Unmarshal([]byte(meta), &queuedMeta)
	if queuedMeta["unsubscribe_token"] != "" || strings.Contains(strings.ToLower(html+text), "unsubscribe") {
		t.Errorf("a reminder Sam asked for carries an unsubscribe link that never stops it: %v", queuedMeta)
	}
	if !strings.Contains(text, evt.Why) || !strings.Contains(html, "change or remove the reminder on the event") {
		t.Errorf("Sam's reminder doesn't say where to stop it: %q", text)
	}
}

func TestCalendarReminderEmailMigrationUpDownUp(t *testing.T) {
	read := func(dir string) string {
		t.Helper()
		_, here, _, _ := runtime.Caller(0)
		b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "573_calendar_reminder_email."+dir+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("up"), read("down")
	env := integration.SetupEnv(t)
	for i, step := range []struct {
		sql  string
		want bool
	}{{"", true}, {down, false}, {up, true}, {down, false}, {up, true}} {
		if step.sql != "" {
			if _, err := env.PG.Exec(step.sql); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		var column, check bool
		if err := env.PG.QueryRow(`SELECT
			EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'event_reminders' AND column_name = 'email_minutes'),
			EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'event_reminders_email_check')`).Scan(&column, &check); err != nil {
			t.Fatal(err)
		}
		if column != step.want || check != step.want {
			t.Errorf("step %d: column %v, check %v, want %v", i, column, check, step.want)
		}
	}
}

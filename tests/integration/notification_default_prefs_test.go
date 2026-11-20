//go:build integration
// +build integration

package integration_test

// A person who never opened their notification settings is emailed as the
// settings say by default (V-1241): every kind is on in the columns' defaults,
// so the dispatcher makes their preference row itself, once, with an
// unsubscribe token the first email carries. The public demo emails no one.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAPersonWhoNeverOpenedSettingsIsEmailedEveryKind' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	notification "github.com/akashc777/OneCamp/business/Notification"
	notificationController "github.com/akashc777/OneCamp/controllers/Notification"
	"github.com/akashc777/OneCamp/helpers"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	"github.com/akashc777/OneCamp/tests/integration"
)

// defaultKinds is one kind for each email switch a person has, and one that
// no switch holds (a doc shared with them): what a default row lets through.
var defaultKinds = []notification.EventType{
	notification.EventChatDM, notification.EventChannelMention, notification.EventTaskAssigned,
	notification.EventTaskStatus, notification.EventTaskComment, notification.EventChannelCall,
	notification.EventCalendarInvited, notification.EventCalendarUpdated, notification.EventCalendarAnswered,
	notification.EventCalendarRecap, notification.EventDocShared,
}

// notifyEmailOn is notification email as an install with a sending key has it,
// off the demo: nothing is sent, the queue is read.
func notifyEmailOn(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_ESSENTIAL_ONLY", "")
	t.Setenv("DEMO_MODE", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
}

// tell is one notification of kind from actor to the recipients, through
// Dispatch, and how many it queued.
func tell(actor string, kind notification.EventType, to ...uuid.UUID) int {
	evt := notification.Event{Type: kind, ActorUUID: actor, ActorName: "ana", Title: "about " + string(kind), DedupKeyParts: []string{uuid.NewString()}}
	for _, id := range to {
		evt.Recipients = append(evt.Recipients, notification.Recipient{UserUUID: id})
	}
	return notification.Dispatch(context.Background(), evt)
}

func (w *workWorld) count(q string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.env.PG.QueryRow(q, args...).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// prefRows is how many preference rows the person has (0 or 1).
func (w *workWorld) prefRows(id uuid.UUID) int {
	return w.count(`SELECT count(*) FROM users_notification_preferences WHERE user_id = $1`, id)
}

// queuedFor is how many emails are queued for the person, of kind ("" any).
func (w *workWorld) queuedFor(id uuid.UUID, kind notification.EventType) int {
	return w.count(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND ($2 = '' OR event_type = $2)`, id, string(kind))
}

// emailSettingsOf is what the person's settings card is told (GET), which
// makes their row if they have none, as opening the card does.
func (w *workWorld) emailSettingsOf(name string) map[string]any {
	w.t.Helper()
	who := w.account(name)
	rec := call(w.t, notificationController.GetMyNotificationPreferences, http.MethodGet, "/user/notificationPreferences", &who, "", nil)
	var out struct {
		Data map[string]any `json:"data"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		w.t.Fatalf("%s's settings: %d %s", name, rec.Code, rec.Body.String())
	}
	return out.Data
}

// switchesOf is the person's row without what is theirs alone (ids, token,
// times): the switches and gates, as JSON.
func (w *workWorld) switchesOf(id uuid.UUID) string {
	w.t.Helper()
	var s string
	if err := w.env.PG.QueryRow(`SELECT (to_jsonb(p) - 'id' - 'user_id' - 'unsubscribe_token' - 'created_at' - 'updated_at')::text
		FROM users_notification_preferences p WHERE user_id = $1`, id).Scan(&s); err != nil {
		w.t.Fatalf("switches of %s: %v", id, err)
	}
	return s
}

func TestAPersonWhoNeverOpenedSettingsIsEmailedEveryKind(t *testing.T) {
	w := seedWorkWorld(t)
	notifyEmailOn(t)
	ana, ben, cy, dee, eve := w.people["ana"], w.people["ben"], w.people["cy"], w.people["dee"], w.people["eve"]
	if w.prefRows(ben) != 0 {
		t.Fatal("ben starts with no preference row")
	}

	for _, kind := range defaultKinds {
		if n := tell(ana.String(), kind, ben); n != 1 || w.queuedFor(ben, kind) != 1 {
			t.Errorf("%s to ben, who never opened settings: Dispatch queued %d, the queue holds %d; want 1", kind, n, w.queuedFor(ben, kind))
		}
	}
	if n := w.prefRows(ben); n != 1 {
		t.Fatalf("ben's preference rows after %d emails: %d, want 1", len(defaultKinds), n)
	}

	// Every email carries the token of the row Dispatch made, in its link and
	// for the List-Unsubscribe header (metadata).
	var token string
	if err := w.env.PG.QueryRow(`SELECT unsubscribe_token FROM users_notification_preferences WHERE user_id = $1`, ben).Scan(&token); err != nil || len(token) < 32 {
		t.Fatalf("ben's token: %q %v", token, err)
	}
	if n := w.count(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1
		AND metadata->>'unsubscribe_token' = $2 AND html_body LIKE '%' || $2 || '%'`, ben, token); n != len(defaultKinds) {
		t.Errorf("emails carrying ben's token in metadata and link: %d of %d", n, len(defaultKinds))
	}

	// The row is the columns' defaults: the same as one made with nothing
	// but a person and a token.
	w.exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2)`, cy, "tok-"+cy.String())
	if got, want := w.switchesOf(ben), w.switchesOf(cy); got != want {
		t.Errorf("the row Dispatch made isn't the defaults:\n got %s\nwant %s", got, want)
	}

	// Rows are made only for whom an email goes to: not for someone whose
	// address is suppressed, nor for a removed person, nor for the actor.
	w.exec(`INSERT INTO notification_email_suppressions (email, reason, details) VALUES ('dee@acme.test', 'bounced', 'test')`)
	w.exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, eve)
	if n := tell(ana.String(), notification.EventChatDM, ben, dee, eve, ana); n != 1 {
		t.Errorf("a DM to ben, suppressed dee, removed eve and its sender: %d queued, want 1 (ben)", n)
	}
	for name, id := range map[string]uuid.UUID{"dee (suppressed)": dee, "eve (removed)": eve, "ana (the sender)": ana} {
		if w.prefRows(id) != 0 || w.queuedFor(id, "") != 0 {
			t.Errorf("%s got a preference row (%d) or an email (%d)", name, w.prefRows(id), w.queuedFor(id, ""))
		}
	}
	if n := w.prefRows(ben); n != 1 {
		t.Errorf("ben's rows after another DM: %d, want 1", n)
	}
}

func TestTheFirstMailsUnsubscribeLinkStopsTheNext(t *testing.T) {
	w := seedWorkWorld(t)
	notifyEmailOn(t)
	ana, ben := w.people["ana"], w.people["ben"]
	if n := tell(ana.String(), notification.EventChatDM, ben); n != 1 {
		t.Fatalf("ben's first DM email: %d queued", n)
	}
	var token string
	if err := w.env.PG.QueryRow(`SELECT metadata->>'unsubscribe_token' FROM notification_email_queue WHERE user_id = $1`, ben).Scan(&token); err != nil || token == "" {
		t.Fatalf("the first email's token: %q %v", token, err)
	}

	// The link's one-click POST (List-Unsubscribe-Post), as a mail client sends it.
	rec := httptest.NewRecorder()
	notificationController.PublicUnsubscribe(rec, httptest.NewRequest(http.MethodPost, "/public/notifications/unsubscribe?token="+token, strings.NewReader("List-Unsubscribe=One-Click")))
	if rec.Code != http.StatusOK {
		t.Fatalf("unsubscribe with the first email's token: %d %s", rec.Code, rec.Body.String())
	}
	for _, kind := range defaultKinds {
		if n := tell(ana.String(), kind, ben); n != 0 {
			t.Errorf("%s after ben unsubscribed: %d queued, want 0", kind, n)
		}
	}
	if n := w.queuedFor(ben, ""); n != 1 {
		t.Errorf("ben's emails: %d, want only the first", n)
	}
	if got := w.emailSettingsOf("ben"); got["email_supported"] != true || got["email_enabled"] != false {
		t.Errorf("ben's card after the unsubscribe: email_supported %v, email_enabled %v; want true, false", got["email_supported"], got["email_enabled"])
	}
}

// onTheDemo is the public demo with email configured: fay is its shared
// visitor, everyone who opens it.
func onTheDemo(t *testing.T) {
	notifyEmailOn(t)
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "fay@acme.test")
}

// allOn is a row with every email on, sent whether or not they're online.
func (w *workWorld) allOn(id uuid.UUID) {
	w.exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token, email_only_when_offline, email_digest_frequency)
		VALUES ($1, $2, false, 'daily')`, id, "tok-"+id.String())
}

func TestTheDemoQueuesNoMailForAnyKind(t *testing.T) {
	w := seedWorkWorld(t)
	onTheDemo(t)
	ana, ben, cy := w.people["ana"], w.people["ben"], w.people["cy"]
	// Ben runs the demo and signs in as himself, a real address with every
	// email on; Cy has never opened settings.
	w.allOn(ben)
	for _, kind := range defaultKinds {
		if n := tell(ana.String(), kind, ben, cy); n != 0 {
			t.Errorf("%s on the demo: %d queued, want 0", kind, n)
		}
	}
	evt := notification.Event{Type: notification.EventCalendarReminder, Title: "Stand-up in 10 minutes", Requested: true, SkipOnlineCheck: true,
		Recipients: []notification.Recipient{{UserUUID: ben}, {UserUUID: cy}}, DedupKeyParts: []string{uuid.NewString()}}
	if n := notification.Dispatch(context.Background(), evt); n != 0 {
		t.Errorf("a reminder asked for by email, on the demo: %d queued, want 0", n)
	}
	if q, r := w.queuedFor(ben, "")+w.queuedFor(cy, ""), w.prefRows(cy); q != 0 || r != 0 {
		t.Errorf("on the demo: %d emails queued, %d rows made for cy; want 0, 0", q, r)
	}
	// The card offers no email where none is sent.
	if got := w.emailSettingsOf("ben"); got["email_supported"] != false {
		t.Errorf("ben's card on the demo: email_supported %v, want false", got["email_supported"])
	}
}

// A visitor to the demo (a guest: everyone who opens it is fay) messaging or
// assigning work to an account with a real address emails it nothing, through
// the paths the app takes.
func TestOnTheDemoAGuestsActionMailsNoRealAccount(t *testing.T) {
	w := seedWorkWorld(t)
	onTheDemo(t)
	fay, ben := w.people["fay"].String(), w.people["ben"]
	w.allOn(ben)
	notification.DispatchChatDM(fay, "fay", "", helpers.GetGroupingId(fay, ben.String()), uuid.NewString(), "hello from the demo", []string{ben.String()}, false)
	notification.DispatchTaskAssignment(fay, "fay", "", w.mine, "Send the newsletter", "Website", ben.String())
	if running := helpers.SettleBackground(30 * time.Second); len(running) > 0 {
		t.Logf("still running after 30s: %v", running)
	}
	if n := w.queuedFor(ben, ""); n != 0 {
		t.Errorf("a demo visitor's DM and assignment queued %d emails to ben's real address, want 0", n)
	}
}

// A Dispatch to people who all have a preference row makes none: of what it
// asks Postgres on its own goroutine, one read is of the users, one of the
// suppression list and one of the rows, whatever the rows say, and no INSERT
// of a row runs (CR-449 N3: the "only when someone lacks a row" guard). The
// taps count the server's calls; other goroutines' (what earlier tests left
// running) are not the Dispatch's, so only its own are read.
func TestADispatchToPeopleWithRowsMakesNone(t *testing.T) {
	env := integration.SetupEnv(t)
	integration.SetupRedis(t)
	dg := integration.SetupDgraph(t)
	search := integration.SetupOpenSearch(t)
	taps := integration.TapStores(t, env, dg, search)
	notifyEmailOn(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ids := map[string]uuid.UUID{}
	for _, name := range []string{"ana", "bo", "cy"} {
		ids[name] = uuid.New()
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2 || '@acme.test', $2, $2, NOW(), NOW())`, ids[name], name)
		exec(`INSERT INTO users_notification_preferences (user_id, email_enabled, unsubscribe_token) VALUES ($1, false, $2)`, ids[name], "tok-"+name)
	}
	// pgCalls is the Postgres statements one Dispatch from ana to bo and cy
	// made on its own goroutine, and how many it queued.
	pgCalls := func(t *testing.T) (calls []string, queued int) {
		t.Helper()
		w, err := taps.Measure(func() { queued = tell(ids["ana"].String(), notification.EventChatDM, ids["bo"], ids["cy"]) })
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range w.SyncCalls {
			if strings.HasPrefix(c, "pg ") {
				calls = append(calls, c)
			}
		}
		t.Logf("queued %d; Postgres, on the Dispatch's goroutine:\n%s", queued, strings.Join(calls, "\n"))
		return calls, queued
	}
	// of counts the calls that start with prefix or contain part.
	of := func(calls []string, prefix, part string) (n int) {
		for _, c := range calls {
			if (prefix != "" && strings.HasPrefix(c, prefix)) || (part != "" && strings.Contains(c, part)) {
				n++
			}
		}
		return n
	}
	prefsRead := "pg SELECT " + strings.Join(strings.Fields(prefModels.AllColumns()), " ")[:40]
	madeRow := func(calls []string) bool { return of(calls, "", "INSERT INTO users_notification_preferences") > 0 }

	t.Run("email off: one read each, no write", func(t *testing.T) {
		calls, queued := pgCalls(t)
		users, suppression, prefs := of(calls, "", "FROM users WHERE id = ANY"), of(calls, "", "FROM notification_email_suppressions"), of(calls, prefsRead, "")
		writes := of(calls, "pg INSERT", "") + of(calls, "pg UPDATE", "") + of(calls, "pg DELETE", "")
		if queued != 0 || users != 1 || suppression != 1 || prefs != 1 || writes != 0 {
			t.Errorf("queued %d; reads of users %d, suppression %d, preferences %d, writes %d; want 0, 1, 1, 1, 0", queued, users, suppression, prefs, writes)
		}
	})
	t.Run("email on: queued, and still no row made", func(t *testing.T) {
		for _, name := range []string{"bo", "cy"} {
			exec(`UPDATE users_notification_preferences SET email_enabled = true WHERE user_id = $1`, ids[name])
		}
		calls, queued := pgCalls(t)
		if queued != 2 || madeRow(calls) || of(calls, prefsRead, "") != 1 {
			t.Errorf("queued %d, preference reads %d, a row made %v; want 2, 1, none", queued, of(calls, prefsRead, ""), madeRow(calls))
		}
	})
}

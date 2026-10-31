//go:build integration

package integration_test

// meet-02 (1) over HTTP: an event's invitees come into its call without
// knocking, by the key their own invitation's link already carries (the one
// the organiser's "copy guest link" gives, and the invitation email holds).
// Anyone else from outside still knocks and waits: no key, a made-up one, or
// an invitee of another event at this event's call. The event's meeting is
// made with the invitees' lobby.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/akashc777/OneCamp/business/CallRoom" // hands the calendar its event calls
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func callLobbyInviteesMigration(t *testing.T, dir string) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(here), "..", "..", "migrations", "*_call_lobby_invitees."+dir+".sql"))
	if len(matches) != 1 {
		t.Fatalf("want one call_lobby_invitees %s migration, found %v", dir, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The invitees' lobby migration goes down and up again on Postgres 12: down,
// no meeting may choose it and event calls' guests all wait; up, it is a
// choice again, and event calls made before it move to it (a personal room
// and an event call whose owner let anyone in keep theirs).
func TestCallLobbyInviteesMigrationUpDownUp(t *testing.T) {
	env := integration.SetupEnv(t)
	owner := uuid.New()
	meeting := func(id, kind, lobby string) {
		t.Helper()
		if _, err := env.PG.Exec(`INSERT INTO call_meetings (id, kind, owner, lobby, created_at) VALUES ($1, $2, $3, $4, NOW())`, id, kind, owner, lobby); err != nil {
			t.Fatalf("meeting %s: %v", id, err)
		}
	}
	lobbyOf := func(id string) string {
		t.Helper()
		var lobby string
		if err := env.PG.QueryRow(`SELECT lobby FROM call_meetings WHERE id = $1`, id).Scan(&lobby); err != nil {
			t.Fatal(err)
		}
		return lobby
	}
	meeting("ev-invitees", "event", "invitees")
	if _, err := env.PG.Exec(callLobbyInviteesMigration(t, "down")); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := lobbyOf("ev-invitees"); got != "guests" {
		t.Errorf("down: an event call's lobby %q, want guests", got)
	}
	if _, err := env.PG.Exec(`INSERT INTO call_meetings (id, kind, owner, lobby, created_at) VALUES ('ev-x', 'event', $1, 'invitees', NOW())`, owner); err == nil {
		t.Error("down: a meeting chose the invitees' lobby")
	}
	meeting("ev-old", "event", "guests")
	meeting("ev-open", "event", "nobody")
	meeting("room", "personal", "guests")
	if _, err := env.PG.Exec(callLobbyInviteesMigration(t, "up")); err != nil {
		t.Fatalf("up again: %v", err)
	}
	for id, want := range map[string]string{"ev-invitees": "invitees", "ev-old": "invitees", "ev-open": "nobody", "room": "guests"} {
		if got := lobbyOf(id); got != want {
			t.Errorf("up: %s's lobby %q, want %q", id, got, want)
		}
	}
	if _, err := env.PG.Exec(`INSERT INTO call_meetings (id, kind, owner, lobby, created_at) VALUES ('m1', 'instant', $1, 'everyone', NOW())`, owner); err == nil {
		t.Error("up: a lobby choice the meeting doesn't know was stored")
	}
	if _, err := env.PG.Exec(`DELETE FROM call_meetings WHERE owner = $1`, owner); err != nil {
		t.Fatal(err)
	}
}

func TestAnEventsInviteesComeIntoItsCallWithoutKnocking(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }

	// Sam's two events, each with a guest by email and a call of its own.
	event := func(title, guest string, hour int) string {
		t.Helper()
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
			`{"title":"`+title+`","start":{"at":"`+at(hour)+`","zone":"Europe/London"},"end":{"at":"`+at(hour+1)+`"},`+
				`"attendees":[{"email":"`+guest+`"}]}`, "")
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", title, code, body)
		}
		return dataEvent(t, body).ID
	}
	callAt := func(id string) string {
		t.Helper()
		code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+id+"/call", sam, "", id)
		var c struct {
			Slug string `json:"slug"`
		}
		if code != http.StatusOK || json.Unmarshal(body["data"], &c) != nil || c.Slug == "" {
			t.Fatalf("the call of %s: %d %v", id, code, body)
		}
		return c.Slug
	}
	keyOf := func(id, email string) string {
		t.Helper()
		code, body := v2(t, eventController.CalendarGuestLink, http.MethodPost, "/calendar/events/"+id+"/guest-link", sam, `{"email":"`+email+`"}`, id)
		var link struct {
			URL string `json:"url"`
		}
		if code != http.StatusOK || json.Unmarshal(body["data"], &link) != nil || !strings.Contains(link.URL, "/rsvp/") {
			t.Fatalf("%s's link: %d %v", email, code, body)
		}
		return link.URL[strings.LastIndex(link.URL, "/")+1:]
	}
	design := event("Design review", "Alex Kim <alex@example.org>", 9)
	board := event("Board review", "jo@example.org", 11)
	designCall, boardCall := callAt(design), callAt(board)
	alexKey, joKey := keyOf(design, "alex@example.org"), keyOf(board, "jo@example.org")

	join := func(slug, name, invitation string) (int, map[string]any) {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"display_name": name, "invitation": invitation})
		code, out := public(t, guestController.JoinRoom, http.MethodPost, "/public/r/"+slug+"/join", string(in), map[string]string{"slug": slug})
		var m map[string]any
		_ = json.Unmarshal(out["data"], &m)
		return code, m
	}

	// Alex's page offers his event's call, where he joins with his invitation.
	code, body := public(t, eventController.GetPublicInvitation, http.MethodGet, "/public/rsvp/"+alexKey, "", map[string]string{"token": alexKey})
	var page struct {
		CallAddress string `json:"call_address"`
	}
	if code != http.StatusOK || json.Unmarshal(body["data"], &page) != nil || page.CallAddress != "/r/"+designCall {
		t.Fatalf("Alex's page: %d %v, want the call at /r/%s", code, body, designCall)
	}

	// Alex, by his own invitation, comes straight in: a call token, no knock.
	if code, m := join(designCall, "Alex Kim", alexKey); code != http.StatusOK || m["token"] == nil || m["waiting"] != nil {
		t.Fatalf("Alex by his invitation: %d %v, want a call token at once", code, m)
	}
	// A forwarded link lets anyone in under any typed name, so the join's
	// audit row says whose invitation it was: the guest's address and the event.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'guest.join'
			AND metadata->>'invitation_email' = 'alex@example.org' AND metadata->>'invitation_event' = $1`, design).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d guest.join audit rows name Alex's invitation to %s, want 1", n, design)
		}
	}
	// Without it, everyone from outside knocks and waits.
	for name, key := range map[string]string{"no invitation": "", "a made-up key": "not-anyones-key", "Jo's, for another event": joKey} {
		if code, m := join(designCall, "Alex Kim", key); code != http.StatusAccepted || m["waiting"] != true || m["token"] != nil {
			t.Errorf("at the design review's call with %s: %d %v, want a knock that waits", name, code, m)
		}
	}
	// Jo's invitation opens Jo's own event's call.
	if code, m := join(boardCall, "Jo", joKey); code != http.StatusOK || m["token"] == nil {
		t.Errorf("Jo by her invitation at her event's call: %d %v, want a call token", code, m)
	}

	// Both events' meetings let their invitees in.
	rows, err := env.PG.Query(`SELECT lobby FROM call_meetings WHERE kind = 'event' AND archived_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var lobby string
		if err := rows.Scan(&lobby); err != nil {
			t.Fatal(err)
		}
		if n++; lobby != callStateModel.LobbyInvitees {
			t.Errorf("an event's meeting lobby %q, want %q", lobby, callStateModel.LobbyInvitees)
		}
	}
	if n != 2 {
		t.Errorf("%d event meetings, want 2", n)
	}
}

//go:build integration

package integration_test

// An event's own call over HTTP (calendar B11; calls zoom-01, meet-02,
// zoom-15): its organiser gives it one on demand; a guest who may not change
// the event sees it where they read the event but can neither give it one nor
// take it off; its address answers as a room named for the event, guests
// waiting, the organiser's meeting; taking it off, or deleting the event,
// ends it.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	_ "github.com/akashc777/OneCamp/business/CallRoom" // hands the calendar its event calls
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnEventsOwnCallOverHTTP(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("APP_SECRET_KEK", "calendar-integration-tests")
	t.Setenv("JWT_SECRET", "event-call-integration-secret")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	integration.SetupLiveKit(t)
	ctx := context.Background()
	if err := settingsBusiness.SetGuestAccessEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := settingsBusiness.SetGuestRoomsEnabled(true); err != nil {
		t.Fatal(err)
	}
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	at := func(hour int) string { return day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", sam,
		`{"title":"Design review","start":{"at":"`+at(9)+`","zone":"Europe/London"},"end":{"at":"`+at(10)+`"},`+
			`"attendees":[{"user_uuid":"`+maya.UserDgraphInfo.Uuid+`"}]}`, "")
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	e := dataEvent(t, body)
	path := "/calendar/events/" + e.ID + "/call"
	type call struct {
		Slug    string `json:"slug"`
		Address string `json:"address"`
	}
	callOf := func(body map[string]json.RawMessage) call {
		t.Helper()
		var c call
		if err := json.Unmarshal(body["data"], &c); err != nil || c.Slug == "" {
			t.Fatalf("no call in %v: %v", body, err)
		}
		return c
	}
	page := func(slug string) (int, map[string]any) {
		t.Helper()
		code, out := public(t, guestController.RoomAtAddress, http.MethodGet, "/public/r/"+slug, "", map[string]string{"slug": slug})
		var m map[string]any
		_ = json.Unmarshal(out["data"], &m)
		return code, m
	}

	// Maya, a guest who may not change it, can't give it a call.
	if code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, path, maya, "", e.ID); code != http.StatusForbidden {
		t.Fatalf("Maya gave Sam's event a call: %d %v", code, body)
	}
	code, body = v2(t, eventController.AddCalendarEventCall, http.MethodPost, path, sam, "", e.ID)
	if code != http.StatusOK {
		t.Fatalf("Sam gave his event a call: %d %v", code, body)
	}
	c := callOf(body)
	if !strings.HasPrefix(c.Slug, "design-review-") || c.Address != "/r/"+c.Slug {
		t.Fatalf("its call: %+v", c)
	}
	if _, again := v2(t, eventController.AddCalendarEventCall, http.MethodPost, path, sam, "", e.ID); callOf(again) != c {
		t.Fatalf("asked again: %v, want the same call", again)
	}

	// Maya sees it where she reads the event (the /event routes the web reads).
	seen, err := calendarBusiness.GetEvent(ctx, maya, e.ID)
	if err != nil || seen.Call == nil || seen.Call.Address != c.Address {
		t.Fatalf("Maya's read: %+v, %v", seen, err)
	}

	// Its address answers as a room named for the event; its meeting is Sam's,
	// an event's, whose guests wait unless they are its invitees (meet-02).
	if code, m := page(c.Slug); code != http.StatusOK || m["state"] != "live" || m["title"] != "Design review" {
		t.Fatalf("its address: %d %v", code, m)
	}
	var meetings []callStateModel.Meeting
	rows, err := env.PG.QueryContext(ctx, `SELECT id, kind, owner, lobby FROM call_meetings WHERE kind = 'event' AND archived_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m callStateModel.Meeting
		if err := rows.Scan(&m.ID, &m.Kind, &m.Owner, &m.Lobby); err != nil {
			t.Fatal(err)
		}
		meetings = append(meetings, m)
	}
	_ = rows.Close()
	if len(meetings) != 1 || meetings[0].Owner.String() != sam.UserDgraphInfo.Uuid || meetings[0].Lobby != callStateModel.LobbyInvitees {
		t.Fatalf("its meeting: %+v", meetings)
	}

	// Maya can't take it off; Sam can, and its address stops answering.
	if code, body := v2(t, eventController.RemoveCalendarEventCall, http.MethodDelete, path, maya, "", e.ID); code != http.StatusForbidden {
		t.Fatalf("Maya took the call off: %d %v", code, body)
	}
	if code, body := v2(t, eventController.RemoveCalendarEventCall, http.MethodDelete, path, sam, "", e.ID); code != http.StatusOK {
		t.Fatalf("Sam took the call off: %d %v", code, body)
	}
	if code, m := page(c.Slug); code == http.StatusOK && m["state"] == "live" {
		t.Fatalf("its address still answers: %v", m)
	}
	if seen, err := calendarBusiness.GetEvent(ctx, sam, e.ID); err != nil || seen.Call != nil {
		t.Fatalf("the event after: %+v, %v", seen.Call, err)
	}

	// A new call ends with the event.
	_, body = v2(t, eventController.AddCalendarEventCall, http.MethodPost, path, sam, "", e.ID)
	next := callOf(body)
	if next.Slug == c.Slug {
		t.Fatalf("the new call took the old address %s", c.Slug)
	}
	cur, err := calendarBusiness.Events.Get(ctx, sam, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := calendarBusiness.Events.Delete(ctx, sam, e.ID, "", &cur.Sequence); err != nil {
		t.Fatal(err)
	}
	if code, m := page(next.Slug); code == http.StatusOK && m["state"] == "live" {
		t.Fatalf("the deleted event's call still answers: %v", m)
	}
}

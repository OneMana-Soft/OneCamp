//go:build integration

package integration_test

// An event's own call over HTTP (calendar B11; calls zoom-01, meet-02,
// zoom-15): its organiser gives it one on demand; a guest who may not change
// the event sees it where they read the event but can neither give it one nor
// take it off; its address answers as a room named for the event, guests
// waiting, the organiser's meeting; taking it off ends it, and deleting the
// event closes it until the event comes back from the trash (V-641).
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestAnEventsOwnCallOverHTTP|TestARestoredEventsCallAnswersAtItsAddressAgainOverHTTP|TestASeriesTailNeverReopensNorEndsItsSeriesCallOverHTTP)$' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	_ "github.com/akashc777/OneCamp/business/CallRoom" // hands the calendar its event calls
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	eventController "github.com/akashc777/OneCamp/controllers/Calendar"
	guestController "github.com/akashc777/OneCamp/controllers/Guest"
	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
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
	// an event's, whose guests wait unless they are its invitees (meet-02),
	// and knows its event, where what the call makes lands (meet-02 (2)).
	if code, m := page(c.Slug); code != http.StatusOK || m["state"] != "live" || m["title"] != "Design review" {
		t.Fatalf("its address: %d %v", code, m)
	}
	var meetings []callStateModel.Meeting
	rows, err := env.PG.QueryContext(ctx, `SELECT id, kind, owner, lobby, event_id FROM call_meetings WHERE kind = 'event' AND archived_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m callStateModel.Meeting
		var event uuid.NullUUID
		if err := rows.Scan(&m.ID, &m.Kind, &m.Owner, &m.Lobby, &event); err != nil {
			t.Fatal(err)
		}
		m.Event = event.UUID
		meetings = append(meetings, m)
	}
	_ = rows.Close()
	if len(meetings) != 1 || meetings[0].Owner.String() != sam.UserDgraphInfo.Uuid || meetings[0].Lobby != callStateModel.LobbyInvitees ||
		meetings[0].Event.String() != e.ID {
		t.Fatalf("its meeting: %+v, want event %s's", meetings, e.ID)
	}
	if home, err := callStateBusiness.HomeOf(ctx, "meet-"+meetings[0].ID); err != nil || home.Event.String() != e.ID ||
		home.Ref.Kind != callStateBusiness.KindMeeting || home.Ref.ID != meetings[0].ID {
		t.Fatalf("its call's home: %+v, %v; want the event %s", home, err, e.ID)
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

	// Its call, taken off and given again, is one room (gcal-15 head B: the
	// room its members are in), and still knows the event its recordings and
	// notes belong to.
	var calls int
	if err := env.PG.QueryRowContext(ctx, `SELECT count(*) FROM call_meetings WHERE event_id = $1`, e.ID).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("the event's calls: %d, %v; want 1", calls, err)
	}
}

// calendar V-641 over HTTP (as Google's restored event keeps its Meet link):
// deleting an event closes its call, and bringing the event back from the
// trash opens the same call again, at the address its guests already have,
// as the event's meeting again. A guest can't bring it back, so the address
// stays closed. A call taken off before the delete, or whose link an admin
// turned off, stays off. A series' call closes and comes back with the
// series; an instance has its series' call, which deleting the instance
// alone never closes.
func TestARestoredEventsCallAnswersAtItsAddressAgainOverHTTP(t *testing.T) {
	r := newEventCallRig(t)
	ctx, env, sam, maya, day := r.ctx, r.env, r.sam, r.maya, r.day
	made, addCall, live, del, restore, callSeen := r.made, r.addCall, r.live, r.del, r.restore, r.callSeen

	t.Run("an event's call opens again at its address", func(t *testing.T) {
		e := made("Design review", "")
		c := addCall(e.ID)
		del(e.ID)
		if live(c.Slug) {
			t.Fatalf("the deleted event's call still answers at %s", c.Slug)
		}
		if code := restore(maya, e.ID); code == http.StatusOK {
			t.Fatalf("Maya, a guest, brought Sam's event back: %d", code)
		}
		if live(c.Slug) {
			t.Fatalf("a refused restore opened the call again at %s", c.Slug)
		}
		if code := restore(sam, e.ID); code != http.StatusOK {
			t.Fatalf("Sam brought his event back: %d", code)
		}
		if !live(c.Slug) {
			t.Fatalf("the restored event's call doesn't answer at %s", c.Slug)
		}
		if got := callSeen(e.ID); got == nil || *got != c {
			t.Fatalf("Maya's read of the restored event: %+v, want its call %+v", got, c)
		}
		var meetings, open int
		if err := env.PG.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE archived_at IS NULL) FROM call_meetings WHERE event_id = $1`, e.ID).
			Scan(&meetings, &open); err != nil || meetings != 1 || open != 1 {
			t.Fatalf("the event's meetings: %d, %d open, %v; want its one, open", meetings, open, err)
		}
	})

	t.Run("a call taken off, or turned off by an admin, before the delete stays off", func(t *testing.T) {
		for _, how := range []string{"taken off", "turned off"} {
			e := made("Retro "+how, "")
			c := addCall(e.ID)
			if how == "taken off" {
				if code, body := v2(t, eventController.RemoveCalendarEventCall, http.MethodDelete, "/calendar/events/"+e.ID+"/call", sam, "", e.ID); code != http.StatusOK {
					t.Fatalf("take it off: %d %v", code, body)
				}
			} else {
				g, err := guestModel.GetLiveBySlug(ctx, c.Slug)
				if err != nil || g == nil {
					t.Fatalf("its link: %+v %v", g, err)
				}
				if err := guestModel.Revoke(ctx, g.Id); err != nil {
					t.Fatal(err)
				}
			}
			del(e.ID)
			if code := restore(sam, e.ID); code != http.StatusOK {
				t.Fatalf("%s: restore %d", how, code)
			}
			if live(c.Slug) {
				t.Errorf("%s: the call answers again at %s", how, c.Slug)
			}
			if got := callSeen(e.ID); got != nil {
				t.Errorf("%s: Maya's read of the restored event has a call: %+v", how, got)
			}
		}
	})

	t.Run("a series' call comes back with it; an instance keeps its series' call", func(t *testing.T) {
		s := made("Stand-up", `,"recurrence":{"rule":"FREQ=WEEKLY"}`)
		c := addCall(s.ID)
		ref := func(week int) string {
			return s.ID + "_" + day.Add(9*time.Hour).AddDate(0, 0, 7*week).Format("20060102T150405Z")
		}
		if err := calendarBusiness.Events.Delete(ctx, sam, ref(1), calendarBusiness.ScopeThis, nil); err != nil {
			t.Fatal(err)
		}
		if !live(c.Slug) {
			t.Fatalf("deleting one instance closed its series' call")
		}
		if code := restore(sam, ref(1)); code != http.StatusOK {
			t.Fatalf("the instance back: %d", code)
		}
		if got := callSeen(ref(1)); got == nil || *got != c {
			t.Fatalf("the instance back: %+v, want its series' call %+v", got, c)
		}
		if err := calendarBusiness.Events.Delete(ctx, sam, ref(2), calendarBusiness.ScopeAll, nil); err != nil {
			t.Fatal(err)
		}
		if live(c.Slug) {
			t.Fatalf("the deleted series' call still answers at %s", c.Slug)
		}
		if code := restore(sam, s.ID); code != http.StatusOK {
			t.Fatalf("the series back: %d", code)
		}
		if !live(c.Slug) {
			t.Fatalf("the restored series' call doesn't answer at %s", c.Slug)
		}
		for _, id := range []string{s.ID, ref(2)} {
			if got := callSeen(id); got == nil || *got != c {
				t.Fatalf("%s back: %+v, want the series' call %+v", id, got, c)
			}
		}
	})
}

// calendar V-641 over HTTP (main, 6 Nov): "this and following" gives the new
// series its series' call, but the call is the series' that it was made for.
// A tail deleted with its instances comes back from the trash without it,
// offering to add one of its own, so nobody is pulled back into a call a
// deleted part of the series shared: its series took that call off since, and
// its old link still doesn't answer. Deleting a new series whole leaves its
// series' call answering.
func TestASeriesTailNeverReopensNorEndsItsSeriesCallOverHTTP(t *testing.T) {
	r := newEventCallRig(t)
	sam := r.sam
	refAt := func(id string, week int) string {
		return id + "_" + r.day.Add(9*time.Hour).AddDate(0, 0, 7*week).Format("20060102T150405Z")
	}
	roomOf := func(id string) string {
		t.Helper()
		var room string
		if err := r.env.PG.QueryRowContext(r.ctx, `SELECT COALESCE(call_room, '') FROM events WHERE id = $1`, id).Scan(&room); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return room
	}

	t.Run("a tail deleted with its instances comes back without its series' call", func(t *testing.T) {
		s := r.made("Stand-up", `,"recurrence":{"rule":"FREQ=WEEKLY"}`)
		c := r.addCall(s.ID)
		if code, body := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+refAt(s.ID, 2)+"?scope=following", sam, "", refAt(s.ID, 2)); code != http.StatusOK {
			t.Fatalf("delete this and following: %d %v", code, body)
		}
		if !r.live(c.Slug) {
			t.Fatalf("deleting the series' later instances closed its call")
		}
		code, body := v2(t, eventController.CalendarTrash, http.MethodGet, "/calendar/events/trash", sam, "", "")
		var trash []calendarBusiness.Event
		if err := json.Unmarshal(body["data"], &trash); code != http.StatusOK || err != nil || len(trash) != 1 || trash[0].ID == s.ID {
			t.Fatalf("the trash: %d %v", code, body)
		}
		tail := trash[0].ID
		if code, body := v2(t, eventController.RemoveCalendarEventCall, http.MethodDelete, "/calendar/events/"+s.ID+"/call", sam, "", s.ID); code != http.StatusOK {
			t.Fatalf("take the series' call off: %d %v", code, body)
		}
		if code := r.restore(sam, tail); code != http.StatusOK {
			t.Fatalf("the tail back: %d", code)
		}
		if r.live(c.Slug) {
			t.Fatalf("the tail back opened the call its series took off, at %s", c.Slug)
		}
		if got := r.callSeen(tail); got != nil || roomOf(tail) != "" {
			t.Fatalf("Maya's read of the tail back: %+v in %q, want no call", got, roomOf(tail))
		}
		own := r.addCall(tail)
		if own.Slug == c.Slug || !r.live(own.Slug) || r.live(c.Slug) {
			t.Fatalf("the tail's own call at %s (answers %v); the series' old one at %s answers %v", own.Slug, r.live(own.Slug), c.Slug, r.live(c.Slug))
		}
	})

	t.Run("a new series deleted whole leaves its series' call answering", func(t *testing.T) {
		s := r.made("Retro", `,"recurrence":{"rule":"FREQ=WEEKLY"}`)
		c := r.addCall(s.ID)
		code, body := v2(t, eventController.GetCalendarEvent, http.MethodGet, "/calendar/events/"+refAt(s.ID, 1), sam, "", refAt(s.ID, 1))
		if code != http.StatusOK {
			t.Fatalf("the instance: %d %v", code, body)
		}
		inst := dataEvent(t, body)
		code, body = v2(t, eventController.UpdateCalendarEvent, http.MethodPatch, "/calendar/events/"+inst.ID, sam,
			`{"sequence":`+itoa(inst.Sequence)+`,"scope":"following","title":"Retro, room 2"}`, inst.ID)
		if code != http.StatusOK {
			t.Fatalf("this and following: %d %v", code, body)
		}
		fresh := dataEvent(t, body)
		if got := r.callSeen(fresh.ID); fresh.ID == s.ID || got == nil || *got != c {
			t.Fatalf("the new series %s: %+v, want its series' call %+v", fresh.ID, got, c)
		}
		r.del(fresh.ID)
		if !r.live(c.Slug) {
			t.Fatalf("deleting the new series ended its series' call at %s", c.Slug)
		}
		if code := r.restore(sam, fresh.ID); code != http.StatusOK {
			t.Fatalf("the new series back: %d", code)
		}
		if got := r.callSeen(fresh.ID); got != nil || roomOf(fresh.ID) != "" {
			t.Fatalf("the new series back: %+v in %q, want no call", got, roomOf(fresh.ID))
		}
		if got := r.callSeen(s.ID); got == nil || *got != c || !r.live(c.Slug) {
			t.Fatalf("the series: %+v, want its call %+v answering", got, c)
		}
	})
}

// eventCallRig is Sam's events, Maya their guest, and their calls over HTTP,
// with guest access and its rooms on.
type eventCallRig struct {
	ctx       context.Context
	env       *integration.Env
	sam, maya *userModels.UserInfo
	// day is tomorrow at 00:00 UTC; its events are at 09:00.
	day time.Time
	// made is an event of Sam's, Maya its guest; extra adds its fields.
	made    func(title, extra string) calendarBusiness.Event
	addCall func(id string) seenCall
	// live is whether a call answers at its address now.
	live    func(slug string) bool
	del     func(id string)
	restore func(who *userModels.UserInfo, id string) int
	// callSeen is the event's call as Maya reads it, nil for none.
	callSeen func(id string) *seenCall
}

// seenCall is an event's call as its readers are told it. The room id never
// leaves the server (EventCall.Room is json:"-"), so a call is told by its
// slug and its address.
type seenCall struct {
	Slug    string `json:"slug"`
	Address string `json:"address"`
}

func newEventCallRig(t *testing.T) *eventCallRig {
	t.Helper()
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
	r := &eventCallRig{ctx: ctx, env: env, sam: seedCalendarPerson(t, env.PG, dg, "Sam"), maya: seedCalendarPerson(t, env.PG, dg, "Maya"),
		day: time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)}
	at := func(hour int) string { return r.day.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339) }
	r.made = func(title, extra string) calendarBusiness.Event {
		t.Helper()
		code, body := v2(t, eventController.CreateCalendarEvent, http.MethodPost, "/calendar/events", r.sam,
			`{"title":"`+title+`","start":{"at":"`+at(9)+`","zone":"UTC"},"end":{"at":"`+at(10)+`"},`+
				`"attendees":[{"user_uuid":"`+r.maya.UserDgraphInfo.Uuid+`"}]`+extra+`}`, "")
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", title, code, body)
		}
		return dataEvent(t, body)
	}
	r.addCall = func(id string) seenCall {
		t.Helper()
		code, body := v2(t, eventController.AddCalendarEventCall, http.MethodPost, "/calendar/events/"+id+"/call", r.sam, "", id)
		var c seenCall
		if code != http.StatusOK || json.Unmarshal(body["data"], &c) != nil || c.Slug == "" {
			t.Fatalf("add a call to %s: %d %v", id, code, body)
		}
		return c
	}
	r.live = func(slug string) bool {
		t.Helper()
		code, out := public(t, guestController.RoomAtAddress, http.MethodGet, "/public/r/"+slug, "", map[string]string{"slug": slug})
		var m map[string]any
		_ = json.Unmarshal(out["data"], &m)
		return code == http.StatusOK && m["state"] == "live"
	}
	r.del = func(id string) {
		t.Helper()
		if code, body := v2(t, eventController.DeleteCalendarEvent, http.MethodDelete, "/calendar/events/"+id, r.sam, "", id); code != http.StatusOK {
			t.Fatalf("delete %s: %d %v", id, code, body)
		}
	}
	r.restore = func(who *userModels.UserInfo, id string) int {
		t.Helper()
		code, _ := v2(t, eventController.RestoreCalendarEvent, http.MethodPost, "/calendar/events/"+id+"/restore", who, "", id)
		return code
	}
	r.callSeen = func(id string) *seenCall {
		t.Helper()
		seen, err := calendarBusiness.GetEvent(ctx, r.maya, id)
		if err != nil {
			t.Fatalf("Maya's read of %s: %v", id, err)
		}
		if seen.Call == nil {
			return nil
		}
		return &seenCall{Slug: seen.Call.Slug, Address: seen.Call.Address}
	}
	return r
}

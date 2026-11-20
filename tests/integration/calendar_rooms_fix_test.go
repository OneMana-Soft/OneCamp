//go:build integration

package integration_test

// Rooms, the review round (CR-335, CR-B100): the clash check fails closed,
// the v1 API answers a refusal as one, room_taken names the clashing day in
// the writer's zone, a restore lets go of rooms booked meanwhile, and a room's
// calendar shows an instance's own title.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/google/uuid"
)

type roomRefusalRead struct {
	Code string `json:"code"`
	Data struct {
		Room string    `json:"room"`
		Date string    `json:"date"`
		At   time.Time `json:"at"`
	} `json:"data"`
}

// roomsFixWorld is a world with a room, Huddle, and a day a week out.
func roomsFixWorld(t *testing.T) (*roomsWorld, map[string]uuid.UUID, roomRead, time.Time) {
	t.Helper()
	w, p := roomsSetup(t)
	code, raw := w.call(p["Ada"], "POST", "/admin/calendar/rooms", map[string]any{"name": "Huddle", "capacity": 4})
	if code != http.StatusCreated {
		t.Fatalf("add the huddle: %d %s", code, raw)
	}
	return w, p, data[roomRead](t, raw), time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 7)
}

func repeating(title string, start time.Time, minutes int, rule string, rooms ...string) map[string]any {
	b := eventBody(title, start, minutes, rooms...)
	b["recurrence"] = map[string]any{"rule": rule}
	return b
}

func roomEventID(t *testing.T, raw []byte) string {
	t.Helper()
	return data[struct {
		ID string `json:"id"`
	}](t, raw).ID
}

// A write the check can't finish is refused, never let through: at the cap
// it is checked; one past the cap (occurrences, or bookings read) is refused
// with its own code.
func TestRoomsCheckFailsClosedPastItsCaps(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	restore := calendarBusiness.SetRoomCheckCapsForTest(100, 3)
	defer restore()
	if code, raw := w.call(maya, "POST", "/calendar/events", repeating("Three days", day.Add(9*time.Hour), 30, "FREQ=DAILY;COUNT=3", huddle.ID)); code != http.StatusCreated {
		t.Fatalf("a series at the cap: %d %s", code, raw)
	}
	var refusal roomRefusalRead
	code, raw := w.call(maya, "POST", "/calendar/events", repeating("Four days", day.Add(12*time.Hour), 30, "FREQ=DAILY;COUNT=4", huddle.ID))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "rooms_too_many" {
		t.Errorf("a series past the occurrence cap: %d %s, want 409 rooms_too_many", code, raw)
	}
	restore()
	restore = calendarBusiness.SetRoomCheckCapsForTest(1, 100)
	for _, d := range []int{11, 12} {
		if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Sam's", day.AddDate(0, 0, d).Add(8*time.Hour), 30, huddle.ID)); code != http.StatusCreated {
			t.Fatalf("sam's booking: %d %s", code, raw)
		}
	}
	refusal.Code = ""
	code, raw = w.call(maya, "POST", "/calendar/events", repeating("Past the bookings", day.AddDate(0, 0, 10).Add(14*time.Hour), 30, "FREQ=DAILY;COUNT=3", huddle.ID))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "rooms_too_many" {
		t.Errorf("a series past the bookings cap: %d %s, want 409 rooms_too_many", code, raw)
	}
}

// The last instance of a finite series, moved past the series' end, still
// holds the room (B100's B1).
func TestRoomsHoldTheLastInstanceMovedPastTheEnd(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	ten := day.Add(10 * time.Hour)
	code, raw := w.call(maya, "POST", "/calendar/events", repeating("Standup", ten, 30, "FREQ=WEEKLY;COUNT=4", huddle.ID))
	if code != http.StatusCreated {
		t.Fatalf("the standup: %d %s", code, raw)
	}
	last, moved := roomEventID(t, raw)+"_"+ten.AddDate(0, 0, 21).Format("20060102T150405Z"), ten.AddDate(0, 0, 23)
	at := func(d time.Time) map[string]any { return map[string]any{"at": d, "zone": "UTC"} }
	if code, raw := w.patch(maya, last, map[string]any{"scope": "this", "start": at(moved), "end": at(moved.Add(30 * time.Minute))}); code != http.StatusOK {
		t.Fatalf("move the last standup two days later: %d %s", code, raw)
	}
	var refusal roomRefusalRead
	code, raw = w.call(sam, "POST", "/calendar/events", eventBody("Huddle grab", moved, 30, huddle.ID))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "room_taken" {
		t.Errorf("booking over the moved last standup: %d %s, want 409 room_taken", code, raw)
	}
}

// Over the v1 API (a member's token) a double booking is 409 room_taken and
// an unknown room 404, never 503 "try again".
func TestRoomRefusalsOverTheAPIAreAnsweredAsRefusals(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	ten := day.Add(10 * time.Hour)
	if code, raw := w.call(maya, "POST", "/calendar/events", eventBody("Maya's", ten, 60, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("maya's booking: %d %s", code, raw)
	}
	tok, err := apiTokenBusiness.CreateToken(context.Background(), "rooms", []string{apiTokenBusiness.ScopeCalendarRead, apiTokenBusiness.ScopeCalendarWrite}, 30, sam, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path string, body any) (int, string) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	// Rooms aren't an argument of the API: an event holding one, moved over
	// the API onto another booking of it, is refused as a refusal.
	code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Sam's", ten.Add(3*time.Hour), 30, huddle.ID))
	if code != http.StatusCreated {
		t.Fatalf("sam's booking: %d %s", code, raw)
	}
	id := roomEventID(t, raw)
	_, raw = w.call(sam, "GET", "/calendar/events/"+id, nil)
	seen := data[struct {
		Sequence int `json:"sequence"`
	}](t, raw)
	move := map[string]any{"sequence": seen.Sequence, "start": map[string]any{"at": ten.Add(30 * time.Minute), "zone": "UTC"}, "end": map[string]any{"at": ten.Add(time.Hour), "zone": "UTC"}}
	if code, out := send(http.MethodPatch, "/v1/events/"+id, move); code != http.StatusConflict || !strings.Contains(out, "room_taken") {
		t.Errorf("a double booking over the API: %d %s, want 409 room_taken", code, out)
	}
	if code, out := send(http.MethodPost, "/v1/events", eventBody("With a room", ten.Add(4*time.Hour), 30, uuid.NewString())); code != http.StatusBadRequest || !strings.Contains(out, "rooms isn't an argument") {
		t.Errorf("rooms sent to the API: %d %s, want 400, rooms isn't an argument", code, out)
	}
}

// room_taken names the day that clashes, in the zone of the event refused:
// 16:30 in Los Angeles is that day there (the next in UTC), and a clash on a
// three-day booking's last day names that day.
func TestRoomTakenNamesTheClashingDayInTheWritersZone(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	y, m, d := day.Date()
	late := time.Date(y, m, d, 16, 30, 0, 0, la)
	inLA := func(title string, at time.Time) map[string]any {
		return map[string]any{"title": title, "start": map[string]any{"at": at, "zone": la.String()},
			"end": map[string]any{"at": at.Add(30 * time.Minute), "zone": la.String()}, "rooms": []string{huddle.ID}}
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", inLA("Sam's late one", late)); code != http.StatusCreated {
		t.Fatalf("sam's booking: %d %s", code, raw)
	}
	var refusal roomRefusalRead
	code, raw := w.call(maya, "POST", "/calendar/events", inLA("Maya's late one", late))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Data.Date != late.Format("2006-01-02") || !refusal.Data.At.Equal(late) {
		t.Errorf("a clash at 16:30 in Los Angeles: %d %s, want date %s at %s", code, raw, late.Format("2006-01-02"), late.UTC().Format(time.RFC3339))
	}
	offsite := day.AddDate(0, 0, 3).Add(9 * time.Hour)
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Offsite", offsite, 3*24*60-6*60, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("the three-day offsite: %d %s", code, raw)
	}
	third := offsite.AddDate(0, 0, 2).Add(time.Hour)
	refusal.Data.Date = ""
	code, raw = w.call(maya, "POST", "/calendar/events", eventBody("On day three", third, 30, huddle.ID))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Data.Date != third.Format("2006-01-02") {
		t.Errorf("a clash on the offsite's third day: %d %s, want date %s", code, raw, third.Format("2006-01-02"))
	}
}

// Bringing an event, or one occurrence, back from the trash onto a room
// booked meanwhile brings it back without that room, and the answer lists
// the rooms let go.
func TestRoomsRestoreLetsGoOfRoomsBookedMeanwhile(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	type restored struct {
		Rooms      []roomRead `json:"rooms"`
		RoomsLetGo []roomRead `json:"rooms_let_go"`
	}
	away := func(what, id string, at time.Time) {
		t.Helper()
		if code, raw := w.call(maya, "DELETE", "/calendar/events/"+id+"?scope=this", nil); code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("%s: delete: %d %s", what, code, raw)
		}
		if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Took the slot", at, 30, huddle.ID)); code != http.StatusCreated {
			t.Fatalf("%s: booking its slot: %d %s", what, code, raw)
		}
	}
	// A whole event comes back without the room booked meanwhile, and says so.
	one := day.Add(9 * time.Hour)
	code, raw := w.call(maya, "POST", "/calendar/events", eventBody("Restorable", one, 30, huddle.ID))
	if code != http.StatusCreated {
		t.Fatalf("the event: %d %s", code, raw)
	}
	event := roomEventID(t, raw)
	away("an event", event, one)
	code, raw = w.call(maya, "POST", "/calendar/events/"+event+"/restore", nil)
	if back := data[restored](t, raw); code != http.StatusOK || len(back.RoomsLetGo) != 1 || back.RoomsLetGo[0].Name != "Huddle" || len(back.Rooms) != 0 {
		t.Errorf("an event restored onto a taken room: %d %s, want 200 without the huddle, which it let go", code, raw)
	}
	// One occurrence never strips a room from its whole series: rooms are the
	// series', so its restore is refused in words, and the series keeps the
	// room and stays editable (with other rooms, the occurrence comes back).
	retro := day.AddDate(0, 0, 1).Add(11 * time.Hour)
	code, raw = w.call(maya, "POST", "/calendar/events", repeating("Retro", retro, 30, "FREQ=WEEKLY;COUNT=4", huddle.ID))
	if code != http.StatusCreated {
		t.Fatalf("the retro: %d %s", code, raw)
	}
	series := roomEventID(t, raw)
	week := retro.AddDate(0, 0, 7)
	occurrence := series + "_" + week.Format("20060102T150405Z")
	away("one occurrence", occurrence, week)
	code, raw = w.call(maya, "POST", "/calendar/events/"+occurrence+"/restore", nil)
	var refused struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &refused)
	if code != http.StatusConflict || refused.Code != "room_taken" || !strings.HasPrefix(refused.Msg, "Huddle is booked then") ||
		!strings.HasSuffix(refused.Msg, "Change the series' rooms or restore it later.") {
		t.Errorf("one occurrence restored onto a taken room: %d %s, want 409 room_taken: Huddle is booked then. Change the series' rooms or restore it later.", code, raw)
	}
	if code, raw := w.call(maya, "GET", "/calendar/events/"+series, nil); code != http.StatusOK || len(data[restored](t, raw).Rooms) != 1 {
		t.Errorf("the series after a refused restore of one occurrence: %d %s, want it still holding the huddle", code, raw)
	}
	if code, raw := w.patch(maya, series, map[string]any{"rooms": []string{}}); code != http.StatusOK {
		t.Fatalf("the series' rooms, changed after the refusal: %d %s", code, raw)
	}
	if code, raw := w.call(maya, "POST", "/calendar/events/"+occurrence+"/restore", nil); code != http.StatusOK {
		t.Errorf("one occurrence restored once its series let the room go: %d %s, want 200", code, raw)
	}
}

// A room's calendar shows an instance retitled on its own by its own title.
func TestARoomsCalendarShowsAnInstancesOwnTitle(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya := p["Maya"]
	retro := day.Add(11 * time.Hour)
	code, raw := w.call(maya, "POST", "/calendar/events", repeating("Retro", retro, 30, "FREQ=WEEKLY;COUNT=3", huddle.ID))
	if code != http.StatusCreated {
		t.Fatalf("the retro: %d %s", code, raw)
	}
	week := retro.AddDate(0, 0, 7)
	if code, raw := w.patch(maya, roomEventID(t, raw)+"_"+week.Format("20060102T150405Z"), map[string]any{"scope": "this", "title": "Retro, with guests"}); code != http.StatusOK {
		t.Fatalf("retitle one retro: %d %s", code, raw)
	}
	q := "?from=" + day.Format(time.RFC3339) + "&to=" + day.AddDate(0, 0, 21).Format(time.RFC3339)
	code, raw = w.call(maya, "GET", "/calendar/rooms/"+huddle.ID+"/bookings"+q, nil)
	if code != http.StatusOK || !bytes.Contains(raw, []byte("Retro, with guests")) || bytes.Count(raw, []byte(`"Retro"`)) != 2 {
		t.Errorf("the huddle's calendar: %d %s, want the retitled retro by its own title", code, raw)
	}
}

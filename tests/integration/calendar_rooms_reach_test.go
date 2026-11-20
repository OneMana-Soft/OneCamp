//go:build integration

package integration_test

// Rooms, review round 3 (CR-B100 r2): a finite all-day series west of UTC
// checks the evening of its last day, and a booking far ahead is checked
// over its own span, so one typo year never makes a room unusable for an
// endless daily series.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
)

// A finite all-day series in Los Angeles is refused when its room is booked
// on the evening of its last day (17:00 there is past UTC midnight). The
// control is the same series over a 10:00 booking on its last day, in a
// second room. (B100 r2's P1.)
func TestRoomsAllDaySeriesWestOfUTCChecksItsLastEvening(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	code, raw := w.call(p["Ada"], "POST", "/admin/calendar/rooms", map[string]any{"name": "Probe room", "capacity": 4})
	if code != http.StatusCreated {
		t.Fatalf("second room: %d %s", code, raw)
	}
	probe := data[roomRead](t, raw)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	first := day.AddDate(0, 0, 10)
	y, m, d := first.Date()
	timed := func(title string, at time.Time, room string) map[string]any {
		return map[string]any{"title": title, "start": map[string]any{"at": at, "zone": la.String()},
			"end": map[string]any{"at": at.Add(time.Hour), "zone": la.String()}, "rooms": []string{room}}
	}
	allDay := func(room string) map[string]any {
		return map[string]any{"title": "Training days",
			"start":      map[string]any{"date": first.Format("2006-01-02"), "zone": la.String()},
			"end":        map[string]any{"date": first.AddDate(0, 0, 1).Format("2006-01-02"), "zone": la.String()},
			"recurrence": map[string]any{"rule": "FREQ=DAILY;COUNT=3"}, "rooms": []string{room}}
	}
	morning := time.Date(y, m, d+2, 10, 0, 0, 0, la)
	if code, raw := w.call(sam, "POST", "/calendar/events", timed("Sam's morning", morning, probe.ID)); code != http.StatusCreated {
		t.Fatalf("control booking: %d %s", code, raw)
	}
	if code, raw := w.call(maya, "POST", "/calendar/events", allDay(probe.ID)); code != http.StatusConflict {
		t.Fatalf("CONTROL: the all-day series over a 10:00 booking on its last day: %d %s, want 409", code, raw)
	}
	evening := time.Date(y, m, d+2, 17, 0, 0, 0, la)
	if code, raw := w.call(sam, "POST", "/calendar/events", timed("Sam's evening", evening, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("evening booking: %d %s", code, raw)
	}
	if code, raw := w.call(maya, "POST", "/calendar/events", allDay(huddle.ID)); code != http.StatusConflict {
		t.Errorf("the all-day series over a 17:00 LA booking on its last day: %d %s, want 409", code, raw)
	}
}

// One booking far ahead (a typo year) never stops an endless daily series
// from booking the room, nor from being changed once it holds it; a real
// clash far ahead is still refused as room_taken. (B100 r2's P2.)
func TestRoomsFarBookingIsCheckedOverItsOwnSpan(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	room := func(name string) string {
		code, raw := w.call(p["Ada"], "POST", "/admin/calendar/rooms", map[string]any{"name": name, "capacity": 4})
		if code != http.StatusCreated {
			t.Fatalf("room %s: %d %s", name, code, raw)
		}
		return data[roomRead](t, raw).ID
	}
	probe, clash := room("Probe room"), room("Far room")
	ten := day.Add(10 * time.Hour)
	daily := func(room string) map[string]any { return repeating("Daily standup", ten, 15, "FREQ=DAILY", room) }
	code, raw := w.call(maya, "POST", "/calendar/events", daily(probe))
	if code != http.StatusCreated {
		t.Fatalf("CONTROL: an endless daily series in an empty room: %d %s", code, raw)
	}
	standup := roomEventID(t, raw)
	far := ten.AddDate(20, 0, 0).Add(3 * time.Hour)
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Typo year", far, 30, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("a booking twenty years ahead: %d %s", code, raw)
	}
	if code, raw := w.call(maya, "POST", "/calendar/events", daily(huddle.ID)); code != http.StatusCreated {
		t.Errorf("an endless daily series in a room with one booking twenty years ahead: %d %s, want 201", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Typo year too", far, 30, probe)); code != http.StatusCreated {
		t.Fatalf("a booking twenty years ahead beside the series: %d %s", code, raw)
	}
	if code, raw := w.patch(maya, standup, map[string]any{"title": "Morning standup"}); code != http.StatusOK {
		t.Errorf("retitling the endless series holding a room booked twenty years ahead: %d %s, want 200", code, raw)
	}
	// The far check reads the writer's own exceptions over the far
	// booking's span: an occurrence cancelled twenty years out frees its
	// slot, and once someone books it the series still takes an edit.
	gone := ten.AddDate(20, 0, 1)
	if code, raw := w.call(maya, "DELETE", "/calendar/events/"+standup+"_"+gone.UTC().Format("20060102T150405Z")+"?scope=this", nil); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("cancelling the series' occurrence twenty years ahead: %d %s", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Took the far slot", gone, 15, probe)); code != http.StatusCreated {
		t.Fatalf("a booking in the cancelled occurrence's slot twenty years ahead: %d %s, want 201", code, raw)
	}
	if code, raw := w.patch(maya, standup, map[string]any{"title": "Standup"}); code != http.StatusOK {
		t.Errorf("retitling the series after its cancelled far slot was booked: %d %s, want 200", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Far clash", ten.AddDate(20, 0, 0).Add(5*time.Minute), 30, clash)); code != http.StatusCreated {
		t.Fatalf("a clashing booking twenty years ahead: %d %s", code, raw)
	}
	var refusal roomRefusalRead
	code, raw = w.call(maya, "POST", "/calendar/events", daily(clash))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "room_taken" || refusal.Data.Room != "Far room" {
		t.Errorf("an endless daily series over a clash twenty years ahead: %d %s, want 409 room_taken in Far room", code, raw)
	}
	// An endless booking that begins past the horizon is checked over its
	// first year, as its own write was.
	later := room("Later room")
	if code, raw := w.call(sam, "POST", "/calendar/events", repeating("Weekly, two years on", ten.AddDate(2, 0, 0).Add(5*time.Minute), 30, "FREQ=WEEKLY", later)); code != http.StatusCreated {
		t.Fatalf("an endless weekly series two years ahead: %d %s", code, raw)
	}
	refusal.Code = ""
	code, raw = w.call(maya, "POST", "/calendar/events", daily(later))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "room_taken" {
		t.Errorf("an endless daily series over an endless weekly one beginning two years ahead: %d %s, want 409 room_taken", code, raw)
	}
}

// A far booking's own span is still capped: when the writer has more
// occurrences in it than one check may expand, the write is refused as
// rooms_too_many, while a short far booking under the same caps is not.
func TestRoomsFarCheckFailsClosedWhenOneSpanIsTooDense(t *testing.T) {
	w, p, huddle, day := roomsFixWorld(t)
	maya, sam := p["Maya"], p["Sam"]
	restore := calendarBusiness.SetRoomCheckCapsForTest(100, 400)
	defer restore()
	code, raw := w.call(p["Ada"], "POST", "/admin/calendar/rooms", map[string]any{"name": "Dense room", "capacity": 4})
	if code != http.StatusCreated {
		t.Fatalf("second room: %d %s", code, raw)
	}
	dense := data[roomRead](t, raw).ID
	ten := day.Add(10 * time.Hour)
	daily := func(room string) map[string]any { return repeating("Daily standup", ten, 15, "FREQ=DAILY", room) }
	ahead := day.AddDate(2, 0, 0).Add(8 * time.Hour)
	if code, raw := w.call(sam, "POST", "/calendar/events", eventBody("Two years on", ahead, 30, huddle.ID)); code != http.StatusCreated {
		t.Fatalf("a booking two years ahead: %d %s", code, raw)
	}
	if code, raw := w.call(maya, "POST", "/calendar/events", daily(huddle.ID)); code != http.StatusCreated {
		t.Errorf("an endless daily series beside one short booking two years ahead: %d %s, want 201", code, raw)
	}
	if code, raw := w.call(sam, "POST", "/calendar/events", repeating("Weekly, two years on", ahead, 30, "FREQ=WEEKLY;COUNT=60", dense)); code != http.StatusCreated {
		t.Fatalf("a weekly series two years ahead: %d %s", code, raw)
	}
	var refusal roomRefusalRead
	code, raw = w.call(maya, "POST", "/calendar/events", daily(dense))
	if _ = json.Unmarshal(raw, &refusal); code != http.StatusConflict || refusal.Code != "rooms_too_many" {
		t.Errorf("an endless daily series over a far span past the occurrence cap: %d %s, want 409 rooms_too_many", code, raw)
	}
}

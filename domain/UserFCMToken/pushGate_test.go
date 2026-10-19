package domain

import (
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// lookups are every lookup of devices or people to push to, by name: the
// query each runs now, and its user id column.
func lookups() []struct{ lookup, query, userCol string } {
	return []struct{ lookup, query, userCol string }{
		{"GetFCMTokenByUserId", tokensOfPersonQuery(), "users_fcm_token.user_id"},
		{"GetFCMTokenByListOfUserId", tokensOfPeopleQuery(3), "users_fcm_token.user_id"},
		{"ReachableNow", reachableQuery(), "u.id"},
	}
}

// Every lookup of devices to push to applies the whole push gate, whichever
// place pushes and whichever store holds the calendar's events: a deactivated
// person's devices are never handed out (deactivation leaves their tokens in
// place, and BE-11-G-3 had closed only Deliver's lookup, so whatever looked
// devices up for itself still reached their phone), and neither are the
// devices of someone paused, in quiet hours or in focus time.
func TestEveryDeviceLookupAppliesThePushGate(t *testing.T) {
	for _, store := range []string{"graph", "postgres"} {
		t.Setenv("CALENDAR_STORE", store)
		for _, c := range lookups() {
			if !strings.Contains(c.query, stillActive(c.userCol)) {
				t.Errorf("%s (%s store) hands out a deactivated person's devices", c.lookup, store)
			}
			if !strings.Contains(c.query, notPaused(c.userCol)) {
				t.Errorf("%s (%s store) hands out the devices of someone paused, in quiet hours or in focus time", c.lookup, store)
			}
		}
	}
}

// The gate asks once where the calendar's events are, and reads focus time
// from there alone. A record of the move that answers differently each time
// it is asked stands in for the moment the move is recorded. Asked part by
// part, the gate named the one store's table with calendar_events' columns,
// and the lookup failed: a push to nobody.
func TestThePushGateReadsFocusTimeFromOneStore(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	moved := false
	helpers.SetCalendarMoved(func() bool { moved = !moved; return moved })
	// With the store on, no probe and one that says moved answer alike.
	t.Cleanup(func() { helpers.SetCalendarMoved(func() bool { return true }) })
	for i := 0; i < 2; i++ {
		for _, c := range lookups() {
			old := strings.Contains(c.query, "FROM calendar_events fe")
			oldCols := strings.Contains(c.query, "fe.is_focus") && strings.Contains(c.query, "fe.start_time")
			newCols := strings.Contains(c.query, "fe.kind = 'focus'") && strings.Contains(c.query, "fe.start_at")
			if old != oldCols || old == newCols || (!old && !strings.Contains(c.query, "FROM events fe")) {
				t.Errorf("%s mixes the stores:\n%s", c.lookup, c.query)
			}
		}
	}
}

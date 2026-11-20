//go:build integration

package business

import "time"

// HoldClockForTest stops the clock the sidebar's kept answer of which calls
// are going on (activeRooms) ages by, until release: an answer kept while it
// is held is served however long the test takes, as it would be within
// activeRoomsTTL, until a call event empties it. tests/integration
// calls_dot_cache_test.go holds it across each event's reads, so a loaded
// machine can't age the kept answer out between two of them. It exists only
// in the integration build, so no product code can reach it.
func HoldClockForTest() (release func()) {
	at := time.Now()
	activeRoomsHeldAt.Store(&at)
	return func() { activeRoomsHeldAt.Store(nil) }
}

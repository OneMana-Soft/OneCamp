//go:build integration

package Calendar

// SetRoomCheckCapsForTest lowers how many bookings and occurrences the room
// clash check reads before it refuses, and returns what puts them back. It
// exists only in the integration build, so no product code can reach it.
func SetRoomCheckCapsForTest(bookings, occurrences int) (restore func()) {
	b, o := maxRoomBookings, maxRoomOccurrences
	maxRoomBookings, maxRoomOccurrences = bookings, occurrences
	return func() { maxRoomBookings, maxRoomOccurrences = b, o }
}

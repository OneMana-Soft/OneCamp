//go:build integration

package helpers

import "time"

// SetClockForTest makes Now answer now() until restore is called. It exists
// only in the integration build: the tag keeps it out of the shipped binary,
// so no product code, and nothing a request sends, can move the clock.
func SetClockForTest(now func() time.Time) (restore func()) {
	was := clock.Swap(&now)
	return func() { clock.Store(was) }
}

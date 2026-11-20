//go:build integration

package Calendar

import (
	"context"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/User"
	"google.golang.org/api/calendar/v3"
)

// SwapGoogleEventsFetchForTest sets what reads a person's Google calendar for
// the event list, and returns what puts the real one back. It exists only in
// the integration build, so no product code can reach it.
func SwapGoogleEventsFetchForTest(f func(context.Context, *model.UserInfo, *time.Time, *time.Time) ([]*calendar.Event, error)) (restore func()) {
	was := fetchGoogleEvents
	fetchGoogleEvents = f
	return func() { fetchGoogleEvents = was }
}

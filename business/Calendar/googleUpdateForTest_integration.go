//go:build integration

package Calendar

import (
	"context"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// SwapGoogleEventUpdateForTest sets what writes an edit to an event's Google
// copy, and returns what puts the real one back. It exists only in the
// integration build, so no product code can reach it.
func SwapGoogleEventUpdateForTest(f func(context.Context, string, adapter.CreateOrUpdateEventInput, *model.UserInfo, bool) error) (restore func()) {
	was := updateGoogleEvent
	updateGoogleEvent = f
	return func() { updateGoogleEvent = was }
}

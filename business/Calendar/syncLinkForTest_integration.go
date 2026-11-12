//go:build integration

package Calendar

import (
	"context"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// SyncLinkForTest keeps one link in step through provider, as the sweep does
// for each synced link (syncAll): the provider's changes in, then OneCamp's
// out. It exists only in the integration build, so no product code can
// reach it.
func SyncLinkForTest(ctx context.Context, link *models.SyncLink, provider calendarSyncBusiness.Adapter) error {
	return stored.syncLink(ctx, link, provider)
}

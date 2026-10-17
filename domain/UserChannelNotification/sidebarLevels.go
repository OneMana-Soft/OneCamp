package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/UserChannelNotification"
)

// GetChannelNotificationTypesForUser is channel id → the person's notification
// level (all, mention or block), in one read, for the sidebar: it compared
// channels against a level that doesn't exist ("none") from a payload that
// never carried one, so a muted channel never looked muted.
func GetChannelNotificationTypesForUser(ctx context.Context, userId string) (levels map[string]string, err error) {
	query := `
		SELECT channel_id::text, notification_type::text
		FROM users_channel_notification
		WHERE user_id = $1
	`
	levels, err = models.GetChannelNotificationTypesForUser(query, userId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetChannelNotificationTypesForUser Failed to read notification levels err: %+v",
			err)
	}
	return
}

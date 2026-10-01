package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/LastSeenChannel"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenChannel(ctx context.Context, userID uuid.UUID, channelID uuid.UUID, lastSeenChannelTime time.Time) (err error) {
	query := `
		INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, channel_id)
		DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen;
	`

	err = models.CreateOrUpdateLastSeenChannel(query, userID, channelID, lastSeenChannelTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateLastSeenChannel Failed to create or update err: %+v",
			err)
		return
	}
	return
}

func BulkUpdateLastSeenChannelForUser(ctx context.Context, userID uuid.UUID, channelIDs []uuid.UUID, lastSeenChannelTime time.Time) (err error) {
	if len(channelIDs) == 0 {
		return nil
	}

	query := `INSERT INTO last_seen_channel (user_id, channel_id, user_last_seen) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	values = append(values, userID, lastSeenChannelTime)
	for i := 0; i < len(channelIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($1, $%d, $2)", i+3))
		values = append(values, channelIDs[i])
	}

	query += strings.Join(placeholders, ",")
	query += ` ON CONFLICT (user_id, channel_id) DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen`

	err = models.BulkCreateOrUpdateLastSeenChannel(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateLastSeenChannelForUser Failed to bulk update err: %+v",
			err)
		return err
	}
	return nil
}

// GetLastSeenChannel returns the user's last-seen time for one channel and
// whether a row exists (false == never seen).
func GetLastSeenChannel(ctx context.Context, userID, channelID uuid.UUID) (time.Time, bool, error) {
	return models.GetLastSeenChannel(ctx, userID, channelID)
}

// GetAllLastSeenChannelsForUser returns channel_id → last-seen for the user.
func GetAllLastSeenChannelsForUser(ctx context.Context, userID uuid.UUID) (map[string]time.Time, error) {
	return models.GetAllLastSeenChannelsForUser(ctx, userID)
}

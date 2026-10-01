package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/LastSeenChat"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenChat(ctx context.Context, userID uuid.UUID, grpID string, lastSeenChannelTime time.Time) (err error) {
	query := `
		INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, grp_id)
		DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen;
	`
	err = models.CreateOrUpdateLastSeenChat(query, userID, grpID, lastSeenChannelTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateLastSeenChannel Failed to create or update err: %+v",
			err)
		return
	}
	return
}

func BulkCreateOrUpdateLastSeenChat(ctx context.Context, userIDs []string, grpID string, lastSeenChannelTime time.Time) (err error) {

	if len(userIDs) == 0 {
		return nil
	}

	query := `INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES `
	values := []interface{}{}
	placeholders := []string{}
	values = append(values, grpID, lastSeenChannelTime)

	for i := 0; i < len(userIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $1, $2)", i+3))
		values = append(values, userIDs[i])
	}

	query += strings.Join(placeholders, ",")
	query += ` ON CONFLICT (user_id, grp_id) DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen`

	err = models.BulkCreateOrUpdateLastSeenChat(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateOrUpdateLastSeenChat Failed to create chat last seen err: %+v",
			err)
		return err
	}

	return nil
}

func BulkCreateLastSeenChatIfNotExists(ctx context.Context, userIDs []string, grpID string) (err error) {
	if len(userIDs) == 0 {
		return nil
	}

	query := `INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES `
	values := []interface{}{}
	placeholders := []string{}
	// Use epoch 0 so it correctly defaults as "unread" until they actually view it
	values = append(values, grpID, time.Unix(0, 0).UTC())

	for i := 0; i < len(userIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $1, $2)", i+3))
		values = append(values, userIDs[i])
	}

	query += strings.Join(placeholders, ",")
	// DO NOTHING prevents overwriting existing last_seen_chat records if they somehow exist
	query += ` ON CONFLICT (user_id, grp_id) DO NOTHING`

	err = models.BulkCreateOrUpdateLastSeenChat(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateLastSeenChatIfNotExists Failed to create chat last seen err: %+v",
			err)
		return err
	}

	return nil
}

func BulkUpdateLastSeenChatForSender(ctx context.Context, grpIDs []string, userID string, lastSeenChannelTime time.Time) (err error) {
	if len(grpIDs) == 0 {
		return nil
	}

	query := `INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES `
	values := []interface{}{}
	placeholders := []string{}
	values = append(values, userID, lastSeenChannelTime)

	for i := 0; i < len(grpIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($1, $%d, $2)", i+3))
		values = append(values, grpIDs[i])
	}

	query += strings.Join(placeholders, ",")
	query += ` ON CONFLICT (user_id, grp_id) DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen`

	err = models.BulkCreateOrUpdateLastSeenChat(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkUpdateLastSeenChatForSender Failed to create chat last seen err: %+v",
			err)
		return err
	}

	return nil
}

// GetLastSeenChat returns the user's last-seen time for one DM/group
// grouping id and whether a row exists (false == never seen).
func GetLastSeenChat(ctx context.Context, userID uuid.UUID, grpID string) (time.Time, bool, error) {
	return models.GetLastSeenChat(ctx, userID, grpID)
}

// GetAllLastSeenChatsForUser returns grp_id → last-seen for the user.
func GetAllLastSeenChatsForUser(ctx context.Context, userID uuid.UUID) (map[string]time.Time, error) {
	return models.GetAllLastSeenChatsForUser(ctx, userID)
}

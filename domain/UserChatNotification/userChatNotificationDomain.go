package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/UserChatNotification"
)

// CreateChatNotificationType gives both people in a new direct conversation
// their setting for it. Each row stands alone: a row one of them already has
// is kept, and the other's is still written. It was one insert that failed
// whole on any row already there, which left the other person with no
// setting, and so never told of a message in it.
func CreateChatNotificationType(ctx context.Context, userId string, toUUID string, grpID string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		INSERT INTO users_chat_notification 
		(user_id, grp_id, notification_type, updated_at)
		VALUES ($1, $3, $4, $5),
		($2, $3, $4, $5)
		ON CONFLICT (user_id, grp_id) DO NOTHING
	`

	err = models.CreateChatNotificationType(query, userId, toUUID, grpID, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateCharNotificationType Failed to create user's chat notification type err: %+v",
			err)
		return
	}
	return
}

func BulkCreateChatNotification(ctx context.Context, userIDs []string, grpID string, notificationType string, lastSeenChannelTime time.Time) (err error) {

	if len(userIDs) == 0 {
		return nil
	}

	query := `INSERT INTO users_chat_notification (user_id, grp_id, notification_type, updated_at) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	values = append(values, grpID, notificationType, lastSeenChannelTime)

	for i := 0; i < len(userIDs); i++ {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $1, $2, $3)", i+4))
		values = append(values, userIDs[i])
	}

	query += strings.Join(placeholders, ",")
	// As for a direct conversation: one row already there doesn't cost the
	// others theirs.
	query += ` ON CONFLICT (user_id, grp_id) DO NOTHING`

	err = models.BulkCreateChatNotification(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateChatNotification Failed to create chat notification err: %+v",
			err)
		return err
	}

	return nil
}

func BulkCreateDMNotificationsIfNotExists(ctx context.Context, senderId string, toUUIDs []string, grpIDs []string, notificationType string, updateTime time.Time) (err error) {
	// Nothing to do is not the same thing as being handed inconsistent input.
	//
	// This used to return nil for both, which meant a caller whose two lists had drifted
	// apart got no notifications for the ENTIRE batch, with no error and nothing logged.
	// Row i pairs recipient i with grouping id i, so a mismatch is a bug upstream and has to
	// say so; an empty list is genuinely a no-op.
	if len(toUUIDs) == 0 {
		return nil
	}
	if err := helpers.RequireSameLength("domain/BulkCreateDMNotificationsIfNotExists",
		helpers.NamedLen{Name: "toUUIDs", Len: len(toUUIDs)},
		helpers.NamedLen{Name: "grpIDs", Len: len(grpIDs)},
	); err != nil {
		return err
	}

	query := `INSERT INTO users_chat_notification (user_id, grp_id, notification_type, updated_at) VALUES `
	values := []interface{}{}
	placeholders := []string{}

	values = append(values, senderId, notificationType, updateTime)

	// Each pair requires inserting 2 rows (one for sender, one for recipient)
	for i := 0; i < len(toUUIDs); i++ {
		// Sender row: ($1, grpId, type, time)
		placeholders = append(placeholders, fmt.Sprintf("($1, $%d, $2, $3)", len(values)+1))
		values = append(values, grpIDs[i])

		// Recipient row: (toUUId, grpId, type, time)
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $2, $3)", len(values)+1, len(values)+2))
		values = append(values, toUUIDs[i], grpIDs[i])
	}

	query += strings.Join(placeholders, ",")
	query += ` ON CONFLICT (user_id, grp_id) DO NOTHING`

	err = models.BulkCreateChatNotification(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkCreateDMNotificationsIfNotExists Failed to create DM notifications err: %+v",
			err)
		return err
	}
	return nil
}

func UpdateChatNotificationType(ctx context.Context, userId string, grpID string, notificationType string, updatedTime time.Time) (err error) {
	query := `
		UPDATE users_chat_notification
        SET notification_type = $1, updated_at = $2
		WHERE user_id = $3 AND grp_id = $4
	`

	err = models.UpdateChatNotificationType(query, userId, grpID, notificationType, updatedTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateChatNotificationType Failed to update user's chat notification type err: %+v",
			err)
		return
	}
	return
}

func GetNotificationTypeByUserIdAndToUserId(ctx context.Context, userId string, grpId string) (notificationType string, err error) {
	query := `
		SELECT notification_type
		FROM users_chat_notification
		WHERE user_id = $1 AND grp_id = $2
	`

	notificationType, err = models.GetNotificationTypeByUserIdAndToUserId(query, userId, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetNotificationTypeByUserIdAndToUserId Failed to user's notification err: %+v",
			err)
		return
	}
	return

}

func GetEligibleUsersForChatActivity(ctx context.Context, grpId string, userId string, mentionsList []string, limitToMentions bool) (userIds []string, err error) {
	query := `
		SELECT 
			user_id
		FROM 
			users_chat_notification
		WHERE 
			grp_id = $1
			AND user_id != $2
			AND (
					(notification_type = 'all' AND ($4 = false OR user_id = ANY($3))) OR
				  	(notification_type = 'mention' AND user_id = ANY($3))
				);
	`

	userIds, err = models.GetEligibleUsersForChatActivity(query, grpId, userId, mentionsList, limitToMentions)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetEligibleUsersForChatActivity Failed to get eligible users err: %+v",
			err)
		return
	}
	return
}

// GetLevelsInConversation is the setting for the conversation grpId of each of
// userIds who is in it, by lower-case user id; someone with no setting isn't
// in it. The thread audience's read (notificationBusiness.ThreadAudience), as
// GetLevelsInChannel is a channel's.
func GetLevelsInConversation(ctx context.Context, grpId string, userIds []string) (map[string]string, error) {
	if len(userIds) == 0 {
		return map[string]string{}, nil
	}
	query := `
		SELECT lower(user_id::text), notification_type
		FROM users_chat_notification
		WHERE grp_id = $1 AND user_id = ANY($2)
	`
	levels, err := models.GetLevelsInConversation(query, grpId, userIds)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLevelsInConversation Failed to get the people's settings for the conversation err: %+v",
			err)
		return nil, err
	}
	return levels, nil
}

// GiveMissingChatSettings gives everyone who is in a conversation and has no
// notification setting for it the default, "all", and answers how many it
// gave. Who is in a conversation is read from last_seen_chat, which holds a
// row for each of its people from the moment it starts or they join (written
// only for its participants). Settings went missing two ways: adding someone
// to a group moved the group to a new id without its settings, and a direct
// conversation's two settings were one insert that failed whole when either
// was already there. Either way, nobody missing one was told of anything in
// it. Safe to run again and alongside itself: a setting already there, or
// given meanwhile, is kept. last_seen_chat outlives someone's place in a
// group, so nobody the conversations index has as having left the group is
// given one (ForgetLeaversChatSettings).
func GiveMissingChatSettings(ctx context.Context) (int64, error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO users_chat_notification (user_id, grp_id, notification_type, updated_at)
		SELECT lsc.user_id, lsc.grp_id, 'all', NOW()
		FROM last_seen_chat lsc
		WHERE NOT EXISTS (
			SELECT 1 FROM users_chat_notification ucn
			WHERE ucn.user_id = lsc.user_id AND ucn.grp_id = lsc.grp_id
		)
		AND NOT EXISTS (
			SELECT 1 FROM conversations c
			JOIN conversation_members cm ON cm.conversation_id = c.id
			WHERE c.kind = 'group' AND c.ref = lsc.grp_id
				AND cm.user_id = lsc.user_id AND cm.left_at IS NOT NULL
		)
		ON CONFLICT (user_id, grp_id) DO NOTHING`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GiveMissingChatSettings err: %+v", err)
		return 0, err
	}
	return res.RowsAffected()
}

// ForgetChatSetting takes away the person's setting for the DM or group grpID:
// Leave's, as they go. Who hears of a group's messages is read from these
// settings, so a setting kept told a leaver of everything said there after.
func ForgetChatSetting(ctx context.Context, userID, grpID string) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM users_chat_notification WHERE user_id = $1 AND grp_id = $2`, userID, grpID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ForgetChatSetting err: %+v", err)
	}
	return err
}

// ForgetLeaversChatSettings takes away the setting of everyone who has left the
// group it is for, and answers how many it took. The conversations index keeps
// when each person left a group: Leave records it (it fails when it can't),
// and only taking them back in clears it. Leave kept the leaver's setting
// before it took it away itself, so installs from then still hold some. Safe
// to run again and alongside itself.
func ForgetLeaversChatSettings(ctx context.Context) (int64, error) {
	forgot, err := pgexec.Exec(ctx, `
		DELETE FROM users_chat_notification ucn
		USING conversations c, conversation_members cm
		WHERE c.kind = 'group' AND c.ref = ucn.grp_id
			AND cm.conversation_id = c.id AND cm.user_id = ucn.user_id
			AND cm.left_at IS NOT NULL`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ForgetLeaversChatSettings err: %+v", err)
		return 0, err
	}
	return forgot, nil
}

package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	Activity "github.com/akashc777/OneCamp/models/dgraph/Activity"
	models "github.com/akashc777/OneCamp/models/postgres/LastSeenActivity"
	"github.com/google/uuid"
)

func CreateOrUpdateLastSeenActivity(ctx context.Context, userID uuid.UUID, lastSeenActivityTime time.Time) (err error) {
	query := `
		INSERT INTO last_seen_activity (user_id, user_last_seen)
		VALUES ($1, $2)
		ON CONFLICT (user_id)
		DO UPDATE SET user_last_seen = EXCLUDED.user_last_seen;
	`

	err = models.CreateOrUpdateLastSeenActivity(query, userID, lastSeenActivityTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateLastSeenActivity Failed to create or update err: %+v",
			err)
		return
	}
	return
}

func GetLastSeenActivityByUserId(ctx context.Context, userID uuid.UUID) (lastSeenActivityTime time.Time, err error) {
	query := `
		SELECT user_last_seen
		FROM last_seen_activity
		WHERE user_id = $1;
	`

	lastSeenActivityTime, err = models.GetLastSeenActivityByUserId(query, userID)

	if err != nil && err.Error() != "sql: no rows in result set" {
		helpers.LogErrorWithContext(ctx,
			"domain/GetLastSeenActivityByUserId Failed to get last seen activity err: %+v",
			err)
		return
	}
	// If err is "no rows", we return the zero value of time.Time with no error
	if err != nil && err.Error() == "sql: no rows in result set" {
		err = nil
	}

	return
}

func GetTotalUnreadActivityCount(ctx context.Context, userDgraphId string, userID uuid.UUID) (totalCount uint64, err error) {

	lastSeen, err := GetLastSeenActivityByUserId(ctx, userID)
	if err != nil {
		return
	}

	variables := make(map[string]string)
	variables["$user_id"] = userID.String()
	variables["$user_uid"] = userDgraphId
	variables["$time"] = lastSeen.Format(time.RFC3339)

	query := `query UnreadCount($user_id: string, $user_uid: string, $time: string) {

		mentionInfo(func: uid($user_uid)) {
			count: count(~mention_users @filter(gt(mention_created_at, $time) AND (has(mention_chat) OR has(mention_post) OR has(mention_comment))))
		}
		commentInfo(func: uid($user_uid)) {
			count: count(~comment_on_content_added_by @filter(gt(comment_created_at, $time) AND not gt(comment_deleted_at, "1970-01-01T00:00:00Z") AND (has(comment_doc) OR has(comment_post) OR has(comment_chat) OR has(comment_task) OR has(comment_board)) AND NOT uid_in(comment_by, $user_uid)))
		}
		reactionInfo(func: uid($user_uid)) {
			count: count(~reaction_on_content_added_by @filter(gt(reaction_added_at, $time) AND (has(~post_reactions) OR has(~chat_reactions) OR has(~comment_reactions)) AND NOT uid_in(reaction_added_by, $user_uid)))
		}
	}`

	totalCount, err = Activity.GetTotalUnreadActivityCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetTotalUnreadActivityCount Failed to get count from dgraph err: %+v", err)
		return
	}

	return
}

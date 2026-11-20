package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	readStateModels "github.com/akashc777/OneCamp/models/postgres/ReadState"
	"github.com/google/uuid"
)

// openChat marks a conversation read for its reader as it opens and says how
// many were unread just before (A1-06, H5B). It is one statement where the
// open was two (the read state's position, then the mark): every part of a
// statement reads the snapshot from before its writes, so the count is what
// the open found (chat B27). Unread is the read state's one rule
// (models/postgres/ReadState Chat.Unread); with no marker, none. The mark
// keeps the later of the two times, as every write here does.
var openChat = `WITH before AS (
		SELECT COUNT(m.id) AS unread
		FROM last_seen_chat lsc
		JOIN chats m ON ` + readStateModels.Chat.Unread() + `
		WHERE lsc.user_id = $1::uuid AND lsc.grp_id = $2::varchar
	), mark AS (
		INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen)
		VALUES ($1::uuid, $2::varchar, $3)
		ON CONFLICT (user_id, grp_id)
		DO UPDATE SET user_last_seen = GREATEST(last_seen_chat.user_last_seen, EXCLUDED.user_last_seen)
	)
	SELECT unread FROM before`

// OpenChat marks grouping read for who as of at, and says how many messages
// were unread there just before: one statement.
func OpenChat(ctx context.Context, who uuid.UUID, grouping string, at time.Time) (int, error) {
	return pgexec.Scalar[int](ctx, openChat, who, grouping, at)
}

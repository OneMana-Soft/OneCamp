package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BulkDeleteChatsAndPosts removes the posts and chats with these ids, in one
// transaction. Ids that aren't uuids name no row and are left out; the rest
// are compared as uuids, so the primary key's index finds them.
func BulkDeleteChatsAndPosts(ctx context.Context, postIDs, chatIDs []string) error {
	postIDs, chatIDs = onlyUUIDs(postIDs), onlyUUIDs(chatIDs)
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		if len(postIDs) > 0 {
			if _, err := pgexec.Exec(ctx, `DELETE FROM posts WHERE id = ANY($1::uuid[])`, pq.Array(postIDs)); err != nil {
				return err
			}
		}
		if len(chatIDs) > 0 {
			if _, err := pgexec.Exec(ctx, `DELETE FROM chats WHERE id = ANY($1::uuid[])`, pq.Array(chatIDs)); err != nil {
				return err
			}
		}
		return nil
	})
}

// onlyUUIDs is ids that parse as uuids.
func onlyUUIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			out = append(out, u.String())
		}
	}
	return out
}

// Insert is one INSERT statement and its arguments.
type Insert struct {
	Query string
	Args  []any
}

// BulkInsertChatsAndPosts runs the inserts in one transaction. Each is its own
// statement: Postgres refuses several commands in one parameterised statement.
func BulkInsertChatsAndPosts(ctx context.Context, inserts []Insert) error {
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		for _, in := range inserts {
			if _, err := pgexec.Exec(ctx, in.Query, in.Args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkInsertChatAndPost Failed to execute bulk insert err: %+v", err)
	}
	return err
}

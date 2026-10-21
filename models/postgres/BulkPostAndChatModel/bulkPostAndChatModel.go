package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
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

func BulkInsertChatsAndPosts(query string, values []interface{}) (err error) {

	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkInsertChatAndPost Failed to execute bulk insert err: %+v",
			err)
		return err
	}

	return nil
}

package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/lib/pq"
)

// BulkDeleteChatsAndPosts removes the posts and chats with these ids, in one
// transaction.
func BulkDeleteChatsAndPosts(ctx context.Context, postIDs, chatIDs []string) error {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if len(postIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM posts WHERE id::text = ANY($1)`, pq.Array(postIDs)); err != nil {
			return err
		}
	}
	if len(chatIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chats WHERE id::text = ANY($1)`, pq.Array(chatIDs)); err != nil {
			return err
		}
	}
	return tx.Commit()
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

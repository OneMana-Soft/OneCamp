package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

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

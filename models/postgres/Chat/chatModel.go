package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type Chat struct {
	Id        uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	CreatedBy uuid.UUID `json:"created_by"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

type ChatMessageCount struct {
	GrpId     string
	ChatCount uint64
}

func CreateChat(query string, id uuid.UUID, createdBy uuid.UUID, grpId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		createdBy,
		grpId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChat Failed to create new chat err: %+v",
			err)
		return
	}

	return
}

func UpdateChatByUUID(query string, chatUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		time.Now(),
		chatUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatByUUID Failed to update chat err: %+v",
			err)
		return
	}

	return
}

func HardDeleteChatByUUID(query string, chatUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		chatUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteChatByUUID Failed to hard delete chat err: %+v",
			err)
		return
	}

	return
}

func GetLatestChatMessageCountByUserID(query string, userID uuid.UUID) (chatMessageCount map[string]*ChatMessageCount, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetLatestChatMessageCountByUserID Failed to get chat count err: %+v",
			err)
		return
	}
	// A scan error inside the loop returns early, so without this the pooled
	// connection is never released — database/sql only auto-closes when Next()
	// runs to completion.
	defer rows.Close()

	chatMessageCount = make(map[string]*ChatMessageCount)

	// Process the rows to get the result
	for rows.Next() {
		var chatMessageCountTemp ChatMessageCount
		if err = rows.Scan(&chatMessageCountTemp.GrpId, &chatMessageCountTemp.ChatCount); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetLatestChatMessageCountByUserID Failed to scan row err: %+v",
				err)
			return
		}

		chatMessageCount[chatMessageCountTemp.GrpId] = &chatMessageCountTemp
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without this the function returns a
	// PARTIAL result with a nil error, so the caller cannot tell truncated data
	// from a genuinely short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "chatModel.go rows iteration failed err: %+v", err)
		return
	}

	return
}

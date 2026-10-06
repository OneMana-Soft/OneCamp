package models

import (
	"context"
	"database/sql"
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

func GetChatByUUID(query string, chatUUID uuid.UUID) (chatInfo *Chat, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var chatData Chat
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, chatUUID)
	err = row.Scan(
		&chatData.Id,
		&chatData.CreatedBy,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetChatByUUID Failed to get chat err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		chatData.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		chatData.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		chatData.DeletedAt = deletedAt.Time
	}

	return &chatData, nil
}

func BulkUpdateGrpIdInChatAndAtachment(query string, oldGrpId string, newGrpId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		newGrpId,
		oldGrpId,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkUpdateGrpIdInChatAndAtachment Failed to update chat and attachments err: %+v",
			err)
		return
	}

	return
}

// GetEntityIdsOlderThan returns UUIDs of entities created before cutoff.
func GetEntityIdsOlderThan(query string, cutoff time.Time) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without this the function returns a
	// PARTIAL result with a nil error, so the caller cannot tell truncated data
	// from a genuinely short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Chat archive-candidate rows iteration failed err: %+v", err)
		return nil, err
	}
	return ids, nil
}

// BulkArchiveEntity soft-deletes entities created before cutoff.
func BulkArchiveEntity(query string, cutoff time.Time) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	result, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	return rows, nil
}

package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type Post struct {
	Id          uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	ChannelUUID uuid.UUID `gorm:"unique" json:"email_id"`
	CreatedBy   uuid.UUID `json:"user_name"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   time.Time
}

type ChannelPostCount struct {
	ChannelId uuid.UUID
	PostCount uint64
}

func CreatePost(query string, id uuid.UUID, channelUUID uuid.UUID, createdBy uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		channelUUID,
		createdBy,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreatePost Failed to create new post err: %+v",
			err)
		return
	}

	return
}

func BulkInsertPost(query string, values ...interface{}) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkInsertPost Failed to execute bulk insert err: %+v",
			err)
		return err
	}

	return nil
}

func GetPostByUUID(query string, postUUID uuid.UUID) (postInfo *Post, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var postData Post
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, postUUID)
	err = row.Scan(
		&postData.Id,
		&postData.ChannelUUID,
		&postData.CreatedBy,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetPostByUUID Failed to get post err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		postData.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		postData.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		postData.DeletedAt = deletedAt.Time
	}

	return &postData, nil
}

func UpdatePostByUUID(query string, currentTime time.Time, postUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		currentTime,
		postUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdatePostByUUID Failed to update post err: %+v",
			err)
		return
	}

	return
}

func GetLatestPostInChannelCountByUserID(query string, userID uuid.UUID) (err error, channelPostCount map[string]*ChannelPostCount) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetLatestPostInChannelCountByUserID Failed to post count post err: %+v",
			err)
		return
	}
	// A scan error inside the loop returns early, so without this the pooled
	// connection is never released — database/sql only auto-closes when Next()
	// runs to completion.
	defer rows.Close()

	channelPostCount = make(map[string]*ChannelPostCount)

	// Process the rows to get the result
	for rows.Next() {
		var channelPostCountTemp ChannelPostCount
		if err = rows.Scan(&channelPostCountTemp.ChannelId, &channelPostCountTemp.PostCount); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetLatestPostInChannelCountByUserID Failed to scan row err: %+v",
				err)
			return
		}

		channelPostCount[channelPostCountTemp.ChannelId.String()] = &channelPostCountTemp
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without this the function returns a
	// PARTIAL result with a nil error, so the caller cannot tell truncated data
	// from a genuinely short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "postModel.go rows iteration failed err: %+v", err)
		return
	}

	return
}

func HardDeletePstByUUID(query string, postUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		postUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeletePstByUUID Failed to hard delete post err: %+v",
			err)
		return
	}

	return
}

// GetEntityIdsOlderThan returns UUIDs of entities created before cutoff that are not soft-deleted.
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
		helpers.LogErrorWithContext(ctx, "models/Post archive-candidate rows iteration failed err: %+v", err)
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

package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type Comment struct {
	Id        uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	CreatedBy uuid.UUID `json:"user_name"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

func CreateComment(query string, id uuid.UUID, createdBy uuid.UUID, currentTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		createdBy,
		currentTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateComment Failed to create new post err: %+v",
			err)
		return
	}

	return
}

func GetCommentByUUID(query string, postUUID uuid.UUID) (postInfo *Comment, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var commentData Comment
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, postUUID)
	err = row.Scan(
		&commentData.Id,
		&commentData.CreatedBy,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetCommentByUUID Failed to get post err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		commentData.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		commentData.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		commentData.DeletedAt = deletedAt.Time
	}

	return &commentData, nil
}

func UpdateCommentByUUID(query string, commentUUID uuid.UUID, currentTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		currentTime,
		commentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateCommentByUUID Failed to update post err: %+v",
			err)
		return
	}

	return
}

func HardDeleteCommentByUUID(query string, commentUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		commentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteCommentByUUID Failed to hard delete post err: %+v",
			err)
		return
	}

	return
}

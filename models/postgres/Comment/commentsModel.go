package models

import (
	"context"
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

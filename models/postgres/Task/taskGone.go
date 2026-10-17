package models

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// GetTaskDeleted is whether the task was deleted; false for a task that
// isn't there at all.
func GetTaskDeleted(taskUUID uuid.UUID) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var deleted bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM tasks WHERE id = $1`, taskUUID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return deleted, err
}

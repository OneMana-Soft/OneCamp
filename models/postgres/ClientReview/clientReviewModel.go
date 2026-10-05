// Package models (ClientReview) stores clients' verdicts on tasks (migration 183).
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

const (
	Approved = "approved"
	Changes  = "changes"
)

// Review is one verdict a client gave on a task.
type Review struct {
	Id        uuid.UUID `json:"id"`
	TaskUUID  uuid.UUID `json:"task_uuid"`
	Decision  string    `json:"decision"`
	Name      string    `json:"name"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

// Add records a verdict.
func Add(taskID, projectID, grantID uuid.UUID, decision, name, note string) (*Review, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var r Review
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO task_client_reviews (task_uuid, project_uuid, grant_id, decision, name, note) VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, task_uuid, decision, name, note, created_at`, taskID, projectID, grantID, decision, name, note).
		Scan(&r.Id, &r.TaskUUID, &r.Decision, &r.Name, &r.Note, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Latest is a task's newest verdict, or nil.
func Latest(taskID uuid.UUID) (*Review, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var r Review
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT id, task_uuid, decision, name, note, created_at FROM task_client_reviews
		 WHERE task_uuid = $1 ORDER BY created_at DESC LIMIT 1`, taskID).
		Scan(&r.Id, &r.TaskUUID, &r.Decision, &r.Name, &r.Note, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// LatestForProject is each task's newest verdict in a project, by task.
func LatestForProject(projectID uuid.UUID) (map[uuid.UUID]Review, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT DISTINCT ON (task_uuid) id, task_uuid, decision, name, note, created_at FROM task_client_reviews
		 WHERE project_uuid = $1 ORDER BY task_uuid, created_at DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]Review{}
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.Id, &r.TaskUUID, &r.Decision, &r.Name, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		out[r.TaskUUID] = r
	}
	return out, rows.Err()
}

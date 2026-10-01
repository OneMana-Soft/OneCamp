package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

type CalendarEvent struct {
	Id                    uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	Title                 string
	Description           string
	StartTime             time.Time
	EndTime               time.Time
	CreatedBy             uuid.UUID
	GoogleCalendarEventId *string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             time.Time
}

func CreateCalendarEvent(query string, id uuid.UUID, title string, description string, startTime time.Time, endTime time.Time, createdBy uuid.UUID, googleCalEventId *string, createdAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		title,
		description,
		startTime,
		endTime,
		createdBy,
		googleCalEventId,
		createdAt,
		createdAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateCalendarEvent Failed to create calendar event err: %+v", err)
		return err
	}
	return nil
}

func GetCalendarEventByUUID(query string, id uuid.UUID) (*CalendarEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var event CalendarEvent
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var googleCalEventId sql.NullString

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, id)
	err := row.Scan(
		&event.Id,
		&event.Title,
		&event.Description,
		&event.StartTime,
		&event.EndTime,
		&event.CreatedBy,
		&googleCalEventId,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/GetCalendarEventByUUID Failed to get calendar event err: %+v", err)
		return nil, err
	}

	if googleCalEventId.Valid {
		event.GoogleCalendarEventId = &googleCalEventId.String
	}
	if createdAt.Valid {
		event.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		event.UpdatedAt = updatedAt.Time
	}
	if deletedAt.Valid {
		event.DeletedAt = deletedAt.Time
	}

	return &event, nil
}

func UpdateCalendarEvent(query string, id uuid.UUID, title string, description string, startTime time.Time, endTime time.Time, googleCalEventId *string, updatedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		title,
		description,
		startTime,
		endTime,
		googleCalEventId,
		updatedAt,
		id,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateCalendarEvent Failed to update event err: %+v", err)
		return err
	}
	return nil
}

func DeleteCalendarEvent(query string, id uuid.UUID, deletedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deletedAt,
		deletedAt,
		id,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteCalendarEvent Failed to delete event err: %+v", err)
		return err
	}
	return nil
}

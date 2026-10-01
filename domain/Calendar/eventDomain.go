package Calendar

import (
	"context"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Calendar"
	models "github.com/akashc777/OneCamp/models/postgres/Calendar"
	"github.com/google/uuid"
)

func CreateCalendarEvent(ctx context.Context, id uuid.UUID, title, description string, startTime, endTime time.Time, createdBy uuid.UUID, googleCalEventId *string, createdAt time.Time) error {
	query := `
		INSERT INTO calendar_events (id, title, description, start_time, end_time, created_by, google_calendar_event_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	err := models.CreateCalendarEvent(query, id, title, description, startTime, endTime, createdBy, googleCalEventId, createdAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateCalendarEvent Failed to create new event in postgres err: %+v", err)
		return err
	}
	return nil
}

func UpdateCalendarEvent(ctx context.Context, id uuid.UUID, title, description string, startTime, endTime time.Time, googleCalEventId *string, updatedAt time.Time) error {
	query := `
		UPDATE calendar_events
		SET title = $1, description = $2, start_time = $3, end_time = $4, google_calendar_event_id = $5, updated_at = $6
		WHERE id = $7
	`
	err := models.UpdateCalendarEvent(query, id, title, description, startTime, endTime, googleCalEventId, updatedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateCalendarEvent Failed to update event in postgres err: %+v", err)
		return err
	}
	return nil
}

func DeleteCalendarEvent(ctx context.Context, id uuid.UUID, deletedAt time.Time) error {
	query := `
		UPDATE calendar_events
		SET deleted_at = $1, updated_at = $2
		WHERE id = $3
	`
	err := models.DeleteCalendarEvent(query, id, deletedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteCalendarEvent Failed to delete event in postgres err: %+v", err)
		return err
	}
	return nil
}

func GetCalendarEventByUUID(ctx context.Context, id uuid.UUID) (*models.CalendarEvent, error) {
	query := `
		SELECT id, title, description, start_time, end_time, created_by, google_calendar_event_id, created_at, updated_at, deleted_at
		FROM calendar_events
		WHERE id = $1 AND deleted_at IS NULL
	`
	event, err := models.GetCalendarEventByUUID(query, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetCalendarEventByUUID Failed to get event from postgres err: %+v", err)
		return nil, err
	}
	return event, nil
}

func CreateOrUpdateDgraphEvent(ctx context.Context, dgraphEvent *dgraphStruct.DgraphEvent) (string, error) {
	dgraphEvent.DType = []string{"Event"}
	query := ""
	if dgraphEvent.Uid == "" || dgraphEvent.Uid == "uid(event)" {
		query = `query {
				event as var(func: eq(event_uuid, "` + dgraphEvent.Uuid + `"))
			}`
	}
	eventUid, err := dgraphModels.CreateOrUpdateDgraphEvent(ctx, dgraphEvent, query, "")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateOrUpdateDgraphEvent Failed to create or update event in dgraph err: %+v", err)
		return "", err
	}
	return eventUid, nil
}

func DeleteDgraphEvent(ctx context.Context, eventUuid string) error {
	query := `query {
				event as var(func: eq(event_uuid, "` + eventUuid + `"))
			}`
	delStringJSON := `
		{
			"uid": "uid(event)"
		}
	`
	_, err := dgraphModels.CreateOrUpdateDgraphEvent(ctx, &dgraphStruct.DgraphEvent{}, query, delStringJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteDgraphEvent Failed to delete event in dgraph err: %+v", err)
		return err
	}
	return nil
}

func RemoveParticipantFromDgraphEvent(ctx context.Context, eventUid, participantUid string) error {
	delStringJSON := fmt.Sprintf(`{"uid":"%s", "event_participants": [{"uid": "%s"}]}`, eventUid, participantUid)
	_, err := dgraphModels.CreateOrUpdateDgraphEvent(ctx, &dgraphStruct.DgraphEvent{Uid: eventUid}, "", delStringJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RemoveParticipantFromDgraphEvent Failed to remove participant err: %+v", err)
		return err
	}
	return nil
}

func GetDgraphEventInfoByUUID(ctx context.Context, eventUuid string) (*dgraphStruct.DgraphEvent, error) {
	variables := make(map[string]string)
	variables["$id"] = eventUuid
	query := `query EventInfo($id: string){
			eventInfo(func: eq(event_uuid, $id)) {
				uid
				event_uuid
				event_title
				event_description
				event_start_time
				event_end_time
				event_google_calendar_id
				event_created_by {
					uid
					user_uuid
					user_name
					user_full_name
				}
				event_participants {
					uid
					user_uuid
					user_name
					user_full_name
				}
			}
		}`

	event, err := dgraphModels.GetDgraphEventInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphEventInfoByUUID Failed to get event from dgraph err: %+v", err)
		return nil, err
	}
	return event, nil
}

func GetDgraphEventsByUserId(ctx context.Context, userUuid string, startDate *time.Time, endDate *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	variables := make(map[string]string)
	variables["$id"] = userUuid

	filter := ""
	queryArgs := "$id: string"
	if startDate != nil && endDate != nil {
		variables["$startDate"] = startDate.Format(time.RFC3339)
		variables["$endDate"] = endDate.Format(time.RFC3339)
		queryArgs += ", $startDate: string, $endDate: string"
		filter = `@filter(le(event_start_time, $endDate) AND ge(event_end_time, $startDate))`
	}

	query := `query UserEvents(` + queryArgs + `){
			userInfo(func: eq(user_uuid, $id)) {
				user_events: ~event_created_by ` + filter + ` {
					uid
					event_uuid
					event_title
					event_description
					event_start_time
					event_end_time
					event_google_calendar_id
					event_created_by {
						uid
						user_uuid
						user_name
						user_full_name
					}
					event_participants {
						uid
						user_uuid
						user_name
						user_full_name
					}
				}
				joined_events: ~event_participants ` + filter + ` {
					uid
					event_uuid
					event_title
					event_description
					event_start_time
					event_end_time
					event_google_calendar_id
					event_created_by {
						uid
						user_uuid
						user_name
						user_full_name
					}
					event_participants {
						uid
						user_uuid
						user_name
						user_full_name
					}
				}
			}
		}`

	// dgraphModels doesn't have an exact function returning []DgraphEvent nested under userInfo, so we need to do this query manually or reuse one.
	// Actually, we can define a custom query handler in EventModel or do it here.
	// For simplicity, let's add GetDgraphUserEvents query handler to dgraphModels.
	events, err := dgraphModels.GetDgraphUserEvents(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphEventsByUserId Failed to get events from dgraph err: %+v", err)
		return nil, err
	}
	return events, nil
}

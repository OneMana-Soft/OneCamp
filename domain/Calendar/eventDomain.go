package Calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Calendar"
	models "github.com/akashc777/OneCamp/models/postgres/Calendar"
	v2Models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/dgraph-io/dgo/v230/protos/api"
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

// SetCalendarEventFocus marks an event as focus time: while it runs, its
// creator's notifications are paused (see domain/UserFCMToken/pushGate.go).
func SetCalendarEventFocus(ctx context.Context, id uuid.UUID, isFocus bool) error {
	if err := models.SetCalendarEventFocus(id, isFocus); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SetCalendarEventFocus err: %+v", err)
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

// ErrEventNotFound is no event with that id (see GetDgraphEventInfoByUUID).
var ErrEventNotFound = dgraphModels.ErrEventNotFound

// UpdateDgraphEvent writes an event's new values and, in the same request,
// deletes what the update takes away: the guests taken off it and, when its
// notes were cleared, its notes. A set mutation can't do either. It only adds
// to a list such as event_participants, and the empty notes never reach it
// (omitempty), so a removed guest kept the event and cleared notes came back.
// event.Uid must be the event's uid.
func UpdateDgraphEvent(ctx context.Context, event *dgraphStruct.DgraphEvent, removedParticipantUids []string, clearDescription bool) error {
	if event.Uid == "" || event.Uid == "uid(event)" {
		return fmt.Errorf("domain/UpdateDgraphEvent: the event's uid is required")
	}
	event.DType = []string{"Event"}
	del := eventDeletions(event.Uid, removedParticipantUids, clearDescription)
	if _, err := dgraphModels.CreateOrUpdateDgraphEvent(ctx, event, "", del); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateDgraphEvent Failed to update event in dgraph err: %+v", err)
		return err
	}
	return nil
}

// eventDeletions is the delete mutation for UpdateDgraphEvent, or "" when it
// deletes nothing. A null value deletes every value of a predicate. Pure.
func eventDeletions(eventUid string, removedParticipantUids []string, clearDescription bool) string {
	del := map[string]any{"uid": eventUid}
	if len(removedParticipantUids) > 0 {
		edges := make([]map[string]string, 0, len(removedParticipantUids))
		for _, uid := range removedParticipantUids {
			edges = append(edges, map[string]string{"uid": uid})
		}
		del["event_participants"] = edges
	}
	if clearDescription {
		del["event_description"] = nil
	}
	if len(del) == 1 {
		return ""
	}
	b, _ := json.Marshal(del)
	return string(b)
}

// ForgetCalendarEventGoogleCopy forgets the Google copy Google says is gone
// (models.ForgetGoogleCopy): only the id, only while it is still that one.
func ForgetCalendarEventGoogleCopy(ctx context.Context, id uuid.UUID, gone string) error {
	if err := models.ForgetGoogleCopy(ctx, id, gone); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ForgetCalendarEventGoogleCopy err: %+v", err)
		return err
	}
	return nil
}

// ForgetDgraphEventGoogleCopy is ForgetCalendarEventGoogleCopy for the event's
// graph node: one upsert drops the id only while the node still names gone, so
// a copy made since stays. It deletes the id: writing a nil one does nothing
// (omitempty), and every later edit would fail against the stale id again.
func ForgetDgraphEventGoogleCopy(ctx context.Context, eventUid, gone string) error {
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: `query q($e: string, $g: string) {
			e as var(func: uid($e)) @filter(eq(event_google_calendar_id, $g))
		}`,
		Vars:      map[string]string{"$e": eventUid, "$g": gone},
		Mutations: []*api.Mutation{{DelNquads: []byte(`uid(e) <event_google_calendar_id> * .`), Cond: "@if(eq(len(e), 1))"}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ForgetDgraphEventGoogleCopy err: %+v", err)
	}
	return err
}

// liveEvent keeps the old stores' events nobody deleted (CALENDAR_STORE=graph):
// a deleted one keeps its Event node with event_deleted_at set
// (DeleteDgraphEvent). Every read of Event nodes says it.
var liveEvent = dgraphquery.Live("event_deleted_at")

// DeleteDgraphEvent deletes an event's graph node as the old stores delete its
// row (calendar_events.deleted_at): softly. The node stays, its
// event_deleted_at and event_updated_at at deletedAt, and every read leaves it
// out (liveEvent). One deleted already stays as it was.
func DeleteDgraphEvent(ctx context.Context, eventUuid string, deletedAt time.Time) error {
	at := deletedAt.UTC().Format(time.RFC3339Nano)
	set, err := json.Marshal(map[string]string{"uid": "uid(e)", "event_deleted_at": at, "event_updated_at": at})
	if err != nil {
		return err
	}
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: `query q($id: string) {
			e as var(func: eq(event_uuid, $id)) @filter(` + liveEvent + `)
		}`,
		Vars:      map[string]string{"$id": eventUuid},
		Mutations: []*api.Mutation{{SetJson: set, Cond: "@if(eq(len(e), 1))"}},
	})
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
			eventInfo(func: eq(event_uuid, $id)) @filter(` + liveEvent + `) {
				uid
				event_uuid
				event_title
				event_is_focus
				event_is_away
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

	filter := `@filter(` + liveEvent + `)`
	queryArgs := "$id: string"
	if startDate != nil && endDate != nil {
		variables["$startDate"] = startDate.Format(time.RFC3339)
		variables["$endDate"] = endDate.Format(time.RFC3339)
		queryArgs += ", $startDate: string, $endDate: string"
		filter = `@filter(` + liveEvent + ` AND le(event_start_time, $endDate) AND ge(event_end_time, $startDate))`
	}

	query := `query UserEvents(` + queryArgs + `){
			userInfo(func: eq(user_uuid, $id)) {
				user_events: ~event_created_by ` + filter + ` {
					uid
					event_uuid
					event_title
					event_is_focus
				event_is_away
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
					event_is_focus
				event_is_away
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

// SetEventGoogleID records an event's copy in Google Calendar in the
// calendar's one store: the id, and nothing else.
func SetEventGoogleID(ctx context.Context, id uuid.UUID, googleID string) error {
	return v2Models.SetGoogleID(ctx, v2Models.Pool(), id, googleID)
}

package Calendar

import (
	"context"
	"errors"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func CreateEvent(ctx context.Context, userInfo *model.UserInfo, eventInfo adapter.CreateOrUpdateEventInput) (*adapter.OutputEvent, error) {
	eventUUID := uuid.New()
	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	startTime, err := time.Parse(time.RFC3339Nano, eventInfo.StartTime)
	if err != nil {
		startTime, err = time.Parse(time.RFC3339, eventInfo.StartTime)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CreateEvent failed to parse start time err: %+v, val: %s", err, eventInfo.StartTime)
			return nil, err
		}
	}

	endTime, err := time.Parse(time.RFC3339Nano, eventInfo.EndTime)
	if err != nil {
		endTime, err = time.Parse(time.RFC3339, eventInfo.EndTime)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/CreateEvent failed to parse end time err: %+v, val: %s", err, eventInfo.EndTime)
			return nil, err
		}
	}

	err = domain.CreateCalendarEvent(ctx, eventUUID, eventInfo.Title, eventInfo.Description, startTime, endTime, userInfo.UserPostgresInfo.Id, nil, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateEvent failed to create new event in postgres err: %+v", err)
		return nil, err
	}

	isAway := eventInfo.IsAway != nil && *eventInfo.IsAway
	// Time off isn't focus time: away, nobody is waiting on your notifications.
	isFocus := eventInfo.IsFocus != nil && *eventInfo.IsFocus && !isAway
	if isFocus {
		if err := domain.SetCalendarEventFocus(ctx, eventUUID, true); err != nil {
			return nil, err
		}
	}

	var participants []*dgraphStruct.DgraphUser
	var participantUuids []string
	// Batched lookup: one Dgraph round-trip for all participants
	// instead of one per participant. For a 50-person event that's
	// 50x fewer round-trips on the create path.
	if len(eventInfo.Participants) > 0 {
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, eventInfo.Participants)
		byUUID := make(map[string]*dgraphStruct.DgraphUser, len(users))
		for _, u := range users {
			if u != nil {
				byUUID[u.Uuid] = u
			}
		}
		for _, pUuid := range eventInfo.Participants {
			if u, ok := byUUID[pUuid]; ok {
				participants = append(participants, &dgraphStruct.DgraphUser{Uid: u.Uid})
				participantUuids = append(participantUuids, pUuid)
			}
		}
	}

	dgraphEvent := &dgraphStruct.DgraphEvent{
		Uid:         "uid(event)",
		Uuid:        eventUUID.String(),
		Title:       eventInfo.Title,
		Description: eventInfo.Description,
		StartTime:   &startTime,
		EndTime:     &endTime,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		},
		Participants: participants,
		IsFocus:      &isFocus,
		IsAway:       &isAway,
		CreatedAt:    &currentTime,
		UpdatedAt:    &currentTime,
		DeletedAt:    &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphEvent(ctx, dgraphEvent)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateEvent failed to create new event in dgraph err: %+v", err)
		return nil, err
	}

	// Trigger Google Calendar Sync
	go integrationBusiness.SyncEventToGoogleCalendar(context.Background(), eventUUID, eventInfo, userInfo, eventInfo.SyncToGoogleCalendar)

	return &adapter.OutputEvent{
		EventUuid:    eventUUID.String(),
		Title:        eventInfo.Title,
		Description:  eventInfo.Description,
		StartTime:    &startTime,
		EndTime:      &endTime,
		CreatedBy:    userInfo.UserDgraphInfo.Uuid,
		Participants: participantUuids,
		IsFocus:      isFocus,
		IsAway:       isAway,
	}, nil
}

func UpdateEvent(ctx context.Context, eventUUID uuid.UUID, eventInfo adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo) error {
	currentTime := time.Now()

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to get existing event err: %+v", err)
		return err
	}
	if existingEvent == nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent existing event not found")
		return errors.New("event not found")
	}

	if existingEvent.CreatedBy == nil || existingEvent.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent user is not the creator")
		return errors.New("only the creator can edit this event")
	}

	startTime, err := time.Parse(time.RFC3339Nano, eventInfo.StartTime)
	if err != nil {
		startTime, err = time.Parse(time.RFC3339, eventInfo.StartTime)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to parse start time err: %+v, val: %s", err, eventInfo.StartTime)
			return err
		}
	}

	endTime, err := time.Parse(time.RFC3339Nano, eventInfo.EndTime)
	if err != nil {
		endTime, err = time.Parse(time.RFC3339, eventInfo.EndTime)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to parse end time err: %+v, val: %s", err, eventInfo.EndTime)
			return err
		}
	}

	err = domain.UpdateCalendarEvent(ctx, eventUUID, eventInfo.Title, eventInfo.Description, startTime, endTime, existingEvent.GoogleCalendarEventId, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to update event in postgres err: %+v", err)
		return err
	}
	// An event is time off or focus time, never both: whichever is turned on
	// turns the other off (away wins when both are).
	off := false
	if eventInfo.IsAway != nil && *eventInfo.IsAway {
		eventInfo.IsFocus = &off
	} else if eventInfo.IsFocus != nil && *eventInfo.IsFocus {
		eventInfo.IsAway = &off
	}
	if eventInfo.IsFocus != nil {
		if err := domain.SetCalendarEventFocus(ctx, eventUUID, *eventInfo.IsFocus); err != nil {
			return err
		}
	}

	var participants []*dgraphStruct.DgraphUser
	if len(eventInfo.Participants) > 0 {
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, eventInfo.Participants)
		byUUID := make(map[string]*dgraphStruct.DgraphUser, len(users))
		for _, u := range users {
			if u != nil {
				byUUID[u.Uuid] = u
			}
		}
		for _, pUuid := range eventInfo.Participants {
			if u, ok := byUUID[pUuid]; ok {
				participants = append(participants, &dgraphStruct.DgraphUser{Uid: u.Uid})
			}
		}
	}

	dgraphEvent := &dgraphStruct.DgraphEvent{
		Uid:          existingEvent.Uid,
		Uuid:         eventUUID.String(),
		Title:        eventInfo.Title,
		Description:  eventInfo.Description,
		StartTime:    &startTime,
		EndTime:      &endTime,
		Participants: participants,
		IsFocus:      eventInfo.IsFocus,
		IsAway:       eventInfo.IsAway,
		UpdatedAt:    &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphEvent(ctx, dgraphEvent)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to update event in dgraph err: %+v", err)
		return err
	}

	// Trigger Google Calendar Sync Update
	if existingEvent.GoogleCalendarEventId != nil {
		go func() {
			bgCtx := context.Background()
			updateErr := integrationBusiness.UpdateGoogleCalendarEvent(bgCtx, *existingEvent.GoogleCalendarEventId, eventInfo, userInfo)
			if updateErr != nil {
				var goneErr *integrationBusiness.GCalEventGoneError
				if errors.As(updateErr, &goneErr) {
					// GCal event was deleted externally — clear stale reference
					helpers.MessageLogs.InfoLog.Printf("UpdateEvent: clearing stale GCal ID %s for event %s", goneErr.GoogleEventId, eventUUID.String())
					_ = domain.UpdateCalendarEvent(bgCtx, eventUUID, eventInfo.Title, eventInfo.Description, startTime, endTime, nil, time.Now())
					dgraphClear := &dgraphStruct.DgraphEvent{
						Uid:                   existingEvent.Uid,
						Uuid:                  eventUUID.String(),
						GoogleCalendarEventId: nil,
					}
					_, _ = domain.CreateOrUpdateDgraphEvent(bgCtx, dgraphClear)
				}
			}
		}()
	}

	return nil
}

// ErrNotEventCreator is someone other than an event's creator trying to
// delete it: invitees leave an event, they don't delete it for everyone.
var ErrNotEventCreator = errors.New("Only the person who made this event can delete it.")

func DeleteEvent(ctx context.Context, eventUUID uuid.UUID, userInfo *model.UserInfo) error {
	currentTime := time.Now()

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteEvent failed to get existing event err: %+v", err)
		return err
	}
	// As UpdateEvent: the creator only. This checked nothing, so anyone could
	// delete anyone's meeting or booking.
	if existingEvent == nil || existingEvent.CreatedBy == nil || existingEvent.CreatedBy.Uuid != userInfo.UserDgraphInfo.Uuid {
		return ErrNotEventCreator
	}

	err = domain.DeleteCalendarEvent(ctx, eventUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteEvent failed to delete event in postgres err: %+v", err)
		return err
	}

	err = domain.DeleteDgraphEvent(ctx, eventUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteEvent failed to delete event in dgraph err: %+v", err)
		return err
	}

	// Trigger Google Calendar Sync Delete
	if existingEvent != nil && existingEvent.GoogleCalendarEventId != nil {
		go integrationBusiness.DeleteGoogleCalendarEvent(context.Background(), *existingEvent.GoogleCalendarEventId, userInfo)
	}

	return nil
}

func LeaveEvent(ctx context.Context, eventUUID uuid.UUID, userInfo *model.UserInfo) error {

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if err != nil || existingEvent == nil {
		helpers.LogErrorWithContext(ctx, "business/LeaveEvent failed to get existing event err: %+v", err)
		return err
	}

	var participantUidToRemove string
	isParticipant := false
	for _, p := range existingEvent.Participants {
		if p.Uuid == userInfo.UserDgraphInfo.Uuid {
			isParticipant = true
			participantUidToRemove = p.Uid
		}
	}

	if !isParticipant {
		return errors.New("you are not a participant in this event")
	}

	err = domain.RemoveParticipantFromDgraphEvent(ctx, existingEvent.Uid, participantUidToRemove)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/LeaveEvent failed to update event in dgraph err: %+v", err)
		return err
	}

	// Determine the Google Calendar Event ID — check Dgraph first, then fall back to Postgres
	var gcalEventId string
	if existingEvent.GoogleCalendarEventId != nil && *existingEvent.GoogleCalendarEventId != "" {
		gcalEventId = *existingEvent.GoogleCalendarEventId
	} else {
		// Dgraph may not have the GCal ID (e.g., due to prior sync bugs). Check Postgres.
		pgEvent, pgErr := domain.GetCalendarEventByUUID(ctx, eventUUID)
		if pgErr == nil && pgEvent != nil && pgEvent.GoogleCalendarEventId != nil && *pgEvent.GoogleCalendarEventId != "" {
			gcalEventId = *pgEvent.GoogleCalendarEventId
		}
	}

	// Sync the attendee removal to Google Calendar if the event is synced
	if gcalEventId != "" && existingEvent.CreatedBy != nil && existingEvent.CreatedBy.Uuid != "" {
		go func() {
			bgCtx := context.Background()

			// Get the creator's Dgraph info to retrieve their email
			creatorDgraphInfo, err := userDomain.GetActiveDgraphUserInfoByUUID(bgCtx, existingEvent.CreatedBy.Uuid)
			if err != nil || creatorDgraphInfo == nil || creatorDgraphInfo.EmailID == "" {
				helpers.MessageLogs.ErrorLog.Printf("LeaveEvent: failed to get creator dgraph info for GCal sync: %+v", err)
				return
			}

			// Look up creator's Postgres user by email to get their UUID (needed for integration lookup)
			creatorPostgresUser, err := userDomain.GetUserByEmailId(bgCtx, &creatorDgraphInfo.EmailID)
			if err != nil || creatorPostgresUser == nil {
				helpers.MessageLogs.ErrorLog.Printf("LeaveEvent: failed to get creator postgres info for GCal sync: %+v", err)
				return
			}

			// Get the leaving user's email
			leavingUserEmail := userInfo.UserPostgresInfo.EmailID

			integrationBusiness.RemoveAttendeeFromGoogleCalendarEvent(
				bgCtx,
				gcalEventId,
				leavingUserEmail,
				&model.UserInfo{
					UserPostgresInfo: *creatorPostgresUser,
				},
			)
		}()
	}

	return nil
}

func GetEventsList(ctx context.Context, user *model.UserInfo, startDate *time.Time, endDate *time.Time) ([]*dgraphStruct.DgraphEvent, error) {
	userIdStr := user.UserDgraphInfo.Uuid

	events, err := domain.GetDgraphEventsByUserId(ctx, userIdStr, startDate, endDate)
	if err != nil {
		return nil, err
	}

	// Fetch google calendar events
	gcalEvents, err := integrationBusiness.FetchUserGoogleCalendarEvents(ctx, user, startDate, endDate)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf("Error fetching Google Calendar events for user %s: %+v", userIdStr, err)
	} else if len(gcalEvents) > 0 {
		// Create a set of existing Google Calendar Event IDs already in OneCamp to avoid duplicates
		existingGCalIds := make(map[string]bool)
		for _, e := range events {
			if e.GoogleCalendarEventId != nil && *e.GoogleCalendarEventId != "" {
				existingGCalIds[*e.GoogleCalendarEventId] = true
			}
		}

		// Also fetch synced tasks to avoid overlap
		if userUID, err := uuid.Parse(userIdStr); err == nil {
			taskGCalIds, _ := taskDomain.GetSyncedTaskGCalIds(ctx, userUID)
			for _, id := range taskGCalIds {
				existingGCalIds[id] = true
			}
		}

		for _, item := range gcalEvents {
			// Skip if this event is already tracked in OneCamp
			if existingGCalIds[item.Id] {
				continue
			}
			startTime := item.Start.DateTime
			if startTime == "" {
				startTime = item.Start.Date // All-day event
			}

			endTime := item.End.DateTime
			if endTime == "" {
				endTime = item.End.Date
			}

			ptStart, errStart := time.Parse(time.RFC3339, startTime)
			// Handle date-only parsing for all day events
			if errStart != nil {
				ptStart, _ = time.Parse("2006-01-02", startTime)
			}

			ptEnd, errEnd := time.Parse(time.RFC3339, endTime)
			if errEnd != nil {
				ptEnd, _ = time.Parse("2006-01-02", endTime)
			}

			// Fallback deduplication: skip Google events that match an existing OneCamp event by Title + Time.
			// This handles the brief window before the sync goroutine persists the GCal ID.
			// isDuplicate := false
			// for _, e := range events {
			// 	if e.Title == item.Summary &&
			// 		e.StartTime != nil && e.StartTime.Equal(ptStart) &&
			// 		e.EndTime != nil && e.EndTime.Equal(ptEnd) {
			// 		isDuplicate = true
			// 		break
			// 	}
			// }
			// if isDuplicate {
			// 	continue
			// }

			desc := item.Description
			googleEventId := item.Id

			var createdBy *dgraphStruct.DgraphUser
			var participants []*dgraphStruct.DgraphUser

			if item.Creator != nil && (item.Creator.Self || item.Creator.Email == user.UserPostgresInfo.EmailID) {
				createdBy = &dgraphStruct.DgraphUser{Uid: user.UserDgraphInfo.Uid, Uuid: user.UserDgraphInfo.Uuid}
			} else {
				createdBy = nil
				participants = append(participants, &dgraphStruct.DgraphUser{Uid: user.UserDgraphInfo.Uid, Uuid: user.UserDgraphInfo.Uuid})
			}

			events = append(events, &dgraphStruct.DgraphEvent{
				Uid:                   "gcal-" + item.Id,
				Uuid:                  "gcal-" + item.Id, // We use a pseudo UUID for google events
				Title:                 item.Summary,
				Description:           desc,
				StartTime:             &ptStart,
				EndTime:               &ptEnd,
				CreatedBy:             createdBy,
				Participants:          participants,
				GoogleCalendarEventId: &googleEventId,
			})
		}
	}

	return events, nil
}

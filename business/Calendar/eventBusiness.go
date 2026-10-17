package Calendar

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	domain "github.com/akashc777/OneCamp/domain/Calendar"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"google.golang.org/api/calendar/v3"
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

// Errors the calendar's handlers answer with their own status. Their text is
// written for the person.
var (
	// ErrEventNotFound is an event that isn't there, or that the person asking
	// isn't on. Both answer the same, so nobody learns that someone else's
	// event exists.
	ErrEventNotFound = errors.New("This event isn't available.")
	// ErrOnlyCreatorChanges is a guest trying to edit an event.
	ErrOnlyCreatorChanges = errors.New("Only the person who made this event can change it.")
	// ErrCreatorCannotLeave is an event's creator trying to leave it: they
	// delete it instead.
	ErrCreatorCannotLeave = errors.New("You made this event, so you can delete it but not leave it.")
)

// EventInputError is an event request that can't be read as sent.
type EventInputError struct{ msg string }

func (e *EventInputError) Error() string { return e.msg }

// parseEventTime reads an event's start or end as the app sends it (RFC 3339).
func parseEventTime(value, which string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t, err = time.Parse(time.RFC3339, value)
	}
	if err != nil {
		return time.Time{}, &EventInputError{"The " + which + " time isn't a time."}
	}
	return t, nil
}

// isEventCreator and isEventParticipant say how someone is on an event.
func isEventCreator(e *dgraphStruct.DgraphEvent, userUUID string) bool {
	return e != nil && e.CreatedBy != nil && userUUID != "" && e.CreatedBy.Uuid == userUUID
}

func isEventParticipant(e *dgraphStruct.DgraphEvent, userUUID string) bool {
	if e == nil || userUUID == "" {
		return false
	}
	for _, p := range e.Participants {
		if p != nil && p.Uuid == userUUID {
			return true
		}
	}
	return false
}

// diffParticipants compares an event's guests with the whole new list an
// update sends. It answers who to add (ids not on the event yet, in the list's
// order) and who was taken off (guests the list no longer names). The creator
// is never a guest of their own event: their id in the list adds nothing, and
// a guest entry of theirs is taken off. Pure.
func diffParticipants(existing *dgraphStruct.DgraphEvent, wanted []string) (add []string, removed []*dgraphStruct.DgraphUser) {
	creator := ""
	if existing != nil && existing.CreatedBy != nil {
		creator = existing.CreatedBy.Uuid
	}
	keep := make(map[string]bool, len(wanted))
	for _, id := range wanted {
		if id = strings.TrimSpace(id); id != "" && id != creator {
			keep[id] = true
		}
	}
	current := map[string]bool{}
	if existing != nil {
		for _, p := range existing.Participants {
			if p == nil || p.Uuid == "" {
				continue
			}
			current[p.Uuid] = true
			if !keep[p.Uuid] && p.Uid != "" {
				removed = append(removed, p)
			}
		}
	}
	for _, id := range wanted {
		if id = strings.TrimSpace(id); keep[id] && !current[id] {
			add = append(add, id)
			current[id] = true
		}
	}
	return add, removed
}

// UpdateEvent changes an event, for its creator. Participants, when the
// request names them, are the whole new guest list, an empty one included,
// and whoever it no longer names is taken off and told. A request without
// participants leaves the guests as they are.
func UpdateEvent(ctx context.Context, eventUUID uuid.UUID, eventInfo adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo) error {
	currentTime := time.Now()

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if errors.Is(err, domain.ErrEventNotFound) {
		return ErrEventNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to get existing event err: %+v", err)
		return err
	}

	me := userInfo.UserDgraphInfo.Uuid
	if !isEventCreator(existingEvent, me) {
		if isEventParticipant(existingEvent, me) {
			return ErrOnlyCreatorChanges
		}
		return ErrEventNotFound
	}

	startTime, err := parseEventTime(eventInfo.StartTime, "start")
	if err != nil {
		return err
	}
	endTime, err := parseEventTime(eventInfo.EndTime, "end")
	if err != nil {
		return err
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

	// Guests. A set mutation only ever adds to the list, so a guest taken off
	// stayed on the event, kept reading it and stayed busy. The ones taken off
	// are deleted in the same request that writes the rest.
	var added []*dgraphStruct.DgraphUser
	var removed []*dgraphStruct.DgraphUser
	if eventInfo.Participants != nil {
		var addUUIDs []string
		addUUIDs, removed = diffParticipants(existingEvent, eventInfo.Participants)
		if len(addUUIDs) > 0 {
			users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, addUUIDs)
			byUUID := make(map[string]*dgraphStruct.DgraphUser, len(users))
			for _, u := range users {
				if u != nil {
					byUUID[u.Uuid] = u
				}
			}
			for _, id := range addUUIDs {
				if u, ok := byUUID[id]; ok && u.Uid != "" {
					added = append(added, &dgraphStruct.DgraphUser{Uid: u.Uid})
				}
			}
		}
	}
	removedUids := make([]string, 0, len(removed))
	for _, p := range removed {
		removedUids = append(removedUids, p.Uid)
	}

	dgraphEvent := &dgraphStruct.DgraphEvent{
		Uid:          existingEvent.Uid,
		Uuid:         eventUUID.String(),
		Title:        eventInfo.Title,
		Description:  eventInfo.Description,
		StartTime:    &startTime,
		EndTime:      &endTime,
		Participants: added,
		IsFocus:      eventInfo.IsFocus,
		IsAway:       eventInfo.IsAway,
		UpdatedAt:    &currentTime,
	}
	// Cleared notes are deleted, not written: an empty string never reaches
	// the store (omitempty), so the old notes came back after a save.
	if err := domain.UpdateDgraphEvent(ctx, dgraphEvent, removedUids, eventInfo.Description == ""); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateEvent failed to update event in dgraph err: %+v", err)
		return err
	}

	// Tell whoever was taken off; never the creator, who did it.
	var told []string
	for _, p := range removed {
		if p.Uuid != me {
			told = append(told, p.Uuid)
		}
	}
	if len(told) > 0 {
		actor, event := userInfo.UserDgraphInfo, *existingEvent
		helpers.GoSafe(func() { tellRemovedGuests(context.WithoutCancel(ctx), &actor, &event, told, startTime) })
	}

	// Google's copy. The guest list goes only when the request named one, and
	// then as a whole, an empty one included.
	if existingEvent.GoogleCalendarEventId != nil && *existingEvent.GoogleCalendarEventId != "" {
		googleEventID, syncGuests := *existingEvent.GoogleCalendarEventId, eventInfo.Participants != nil
		go func() {
			bgCtx := context.Background()
			updateErr := integrationBusiness.UpdateGoogleCalendarEvent(bgCtx, googleEventID, eventInfo, userInfo, syncGuests)
			var goneErr *integrationBusiness.GCalEventGoneError
			if errors.As(updateErr, &goneErr) {
				// Deleted on Google: forget the copy in both stores, so later
				// edits stop failing against it.
				helpers.MessageLogs.InfoLog.Printf("UpdateEvent: clearing stale GCal ID %s for event %s", goneErr.GoogleEventId, eventUUID.String())
				_ = domain.UpdateCalendarEvent(bgCtx, eventUUID, eventInfo.Title, eventInfo.Description, startTime, endTime, nil, time.Now())
				_ = domain.ClearDgraphEventGoogleId(bgCtx, existingEvent.Uid)
			}
		}()
	}

	bookingEventUpdated(ctx, existingEvent, startTime, endTime)
	return nil
}

// eventUninvited is the notice kind for a guest taken off an event.
const eventUninvited = "event.uninvited"

// tellNotice tells people; swapped in tests.
var tellNotice = notificationBusiness.Tell

// tellRemovedGuests tells the people taken off an event, in their inbox and
// by a push that opens their calendar: who took them off and when it was,
// never its title. A notice names a thing only for someone who can still open
// it, and they no longer can.
func tellRemovedGuests(ctx context.Context, actor *dgraphStruct.DgraphUser, event *dgraphStruct.DgraphEvent, people []string, start time.Time) {
	// Told off the request, so bounded as TellBookingOwner is: a push service
	// that doesn't answer holds nothing for long, shutdown included.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	recipients := make([]notificationBusiness.NoticeRecipient, 0, len(people))
	for _, id := range people {
		recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "subject"})
	}
	tellNotice(ctx, notificationBusiness.Notice{
		Kind:         eventUninvited,
		ResourceKind: "event",
		ResourceUUID: event.Uuid,
		Actor:        actor,
		Data:         map[string]string{"start": start.UTC().Format(time.RFC3339)},
		// Each removal is told once, and a later one again.
		DedupKey: eventUninvited + "|" + event.Uuid + "|" + strconv.FormatInt(time.Now().UnixNano(), 10),
		Title:    actor.DisplayName() + " took you off an event",
		Path:     "/app/calendar",
		// The calendar itself: the event is no longer theirs to open.
		Push: &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR},
	}, recipients)
}

// ErrNotEventCreator is someone other than an event's creator trying to
// delete it: invitees leave an event, they don't delete it for everyone.
var ErrNotEventCreator = errors.New("Only the person who made this event can delete it.")

func DeleteEvent(ctx context.Context, eventUUID uuid.UUID, userInfo *model.UserInfo) error {
	currentTime := time.Now()

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if errors.Is(err, domain.ErrEventNotFound) {
		return ErrEventNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteEvent failed to get existing event err: %+v", err)
		return err
	}
	// As UpdateEvent: the creator only. This checked nothing, so anyone could
	// delete anyone's meeting or booking. Someone not on it at all hears it
	// isn't there.
	if !isEventCreator(existingEvent, userInfo.UserDgraphInfo.Uuid) {
		if isEventParticipant(existingEvent, userInfo.UserDgraphInfo.Uuid) {
			return ErrNotEventCreator
		}
		return ErrEventNotFound
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

	bookingEventDeleted(ctx, existingEvent)
	return nil
}

func LeaveEvent(ctx context.Context, eventUUID uuid.UUID, userInfo *model.UserInfo) error {

	existingEvent, err := domain.GetDgraphEventInfoByUUID(ctx, eventUUID.String())
	if errors.Is(err, domain.ErrEventNotFound) {
		return ErrEventNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/LeaveEvent failed to get existing event err: %+v", err)
		return err
	}

	var participantUidToRemove string
	isParticipant := false
	for _, p := range existingEvent.Participants {
		if p != nil && p.Uuid == userInfo.UserDgraphInfo.Uuid {
			isParticipant = true
			participantUidToRemove = p.Uid
		}
	}

	if !isParticipant {
		if isEventCreator(existingEvent, userInfo.UserDgraphInfo.Uuid) {
			return ErrCreatorCannotLeave
		}
		return ErrEventNotFound
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
			if item == nil || existingGCalIds[item.Id] {
				continue
			}
			if e := googleEventItem(item, user); e != nil {
				events = append(events, e)
			}
		}
	}

	markBookedEvents(ctx, user, events)
	return events, nil
}

// googleDay is how Google writes an all-day event's dates.
const googleDay = "2006-01-02"

// googleEventItem is a Google Calendar event as the calendar lists it: the
// id "gcal-<google id>", and the reader as its creator or as its guest.
//
// An all-day event is its dates. Google gives the first day and the day after
// the last; read as midnight UTC they put "Offsite, Wednesday" on Tuesday and
// Wednesday in New York and on Wednesday and Thursday in India. So the item
// carries the dates (AllDay, StartDate, EndDate), and the web places it by
// them. Its times stay what they were, midnight UTC on the first day and the
// day after the last, so an app from before the dates draws it as it did.
// nil for an event whose times can't be read. Pure.
func googleEventItem(item *calendar.Event, user *model.UserInfo) *dgraphStruct.DgraphEvent {
	if item == nil || item.Start == nil || item.End == nil || item.Id == "" {
		return nil
	}
	googleEventId := item.Id
	ev := &dgraphStruct.DgraphEvent{
		Uid:                   "gcal-" + item.Id,
		Uuid:                  "gcal-" + item.Id, // a pseudo id: the event lives on Google
		Title:                 item.Summary,
		Description:           item.Description,
		GoogleCalendarEventId: &googleEventId,
	}
	if item.Start.Date != "" {
		first, err := time.Parse(googleDay, item.Start.Date)
		if err != nil {
			return nil
		}
		after, err := time.Parse(googleDay, item.End.Date)
		if err != nil || !after.After(first) {
			after = first.AddDate(0, 0, 1)
		}
		ev.StartTime, ev.EndTime = &first, &after
		ev.AllDay, ev.StartDate, ev.EndDate = true, first.Format(googleDay), after.Format(googleDay)
	} else {
		start, err1 := time.Parse(time.RFC3339, item.Start.DateTime)
		end, err2 := time.Parse(time.RFC3339, item.End.DateTime)
		if err1 != nil || err2 != nil {
			return nil
		}
		ev.StartTime, ev.EndTime = &start, &end
	}
	if user != nil {
		me := &dgraphStruct.DgraphUser{Uid: user.UserDgraphInfo.Uid, Uuid: user.UserDgraphInfo.Uuid}
		if item.Creator != nil && (item.Creator.Self || (item.Creator.Email != "" && item.Creator.Email == user.UserPostgresInfo.EmailID)) {
			ev.CreatedBy = me
		} else {
			ev.Participants = []*dgraphStruct.DgraphUser{me}
		}
	}
	return ev
}

// GetEvent is one event, for someone on it. A OneCamp event answers its
// creator and its participants; "gcal-<google id>", as GetEventsList gives a
// Google event, is read from the asker's own Google calendar. Anyone else gets
// ErrEventNotFound, as for no event at all, so nobody learns that someone
// else's event exists.
func GetEvent(ctx context.Context, userInfo *model.UserInfo, eventID string) (*dgraphStruct.DgraphEvent, error) {
	var ev *dgraphStruct.DgraphEvent
	if googleID, ok := strings.CutPrefix(eventID, "gcal-"); ok {
		if strings.TrimSpace(googleID) == "" {
			return nil, &EventInputError{"That isn't an event id."}
		}
		item, err := integrationBusiness.FetchUserGoogleCalendarEvent(ctx, userInfo, googleID)
		if err != nil {
			return nil, err
		}
		if ev = googleEventItem(item, userInfo); ev == nil {
			return nil, ErrEventNotFound
		}
	} else {
		id, err := uuid.Parse(eventID)
		if err != nil {
			return nil, &EventInputError{"That isn't an event id."}
		}
		found, err := domain.GetDgraphEventInfoByUUID(ctx, id.String())
		if errors.Is(err, domain.ErrEventNotFound) {
			return nil, ErrEventNotFound
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/GetEvent err: %+v", err)
			return nil, err
		}
		me := userInfo.UserDgraphInfo.Uuid
		if !isEventCreator(found, me) && !isEventParticipant(found, me) {
			return nil, ErrEventNotFound
		}
		ev = found
	}
	markBookedEvents(ctx, userInfo, []*dgraphStruct.DgraphEvent{ev})
	return ev, nil
}

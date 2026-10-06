package Integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	domain "github.com/akashc777/OneCamp/domain/Integration"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/oauth"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// GCalEventGoneError is returned when a Google Calendar event no longer exists (404/410).
// Callers should clear the stale google_calendar_event_id from OneCamp.
type GCalEventGoneError struct {
	GoogleEventId string
}

func (e *GCalEventGoneError) Error() string {
	return fmt.Sprintf("google calendar event %s no longer exists", e.GoogleEventId)
}

// isGoogleNotFoundError checks if a Google API error is a 404 Not Found or 410 Gone.
func isGoogleNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "404") || strings.Contains(errStr, "410") ||
		strings.Contains(errStr, "notFound") || strings.Contains(errStr, "deleted")
}

type persistingTokenSource struct {
	base         oauth2.TokenSource
	initialToken *oauth2.Token
	userId       uuid.UUID
	ctx          context.Context
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}

	if s.initialToken == nil || t.AccessToken != s.initialToken.AccessToken {
		// Token has been refreshed. Store it.
		s.initialToken = t
		domain.UpdateIntegrationToken(s.ctx, "user", s.userId, "google_calendar", &t.AccessToken, &t.RefreshToken, &t.Expiry)
		helpers.MessageLogs.InfoLog.Printf("Refreshed Google Calendar token for user %s", s.userId)
	}
	return t, nil
}

func getGoogleCalendarService(ctx context.Context, integration *integrationModel.Integration) (*calendar.Service, error) {
	if integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" {
		return nil, fmt.Errorf("google calendar not connected")
	}

	config := oauth.GoogleCalendarConfig()
	if config == nil {
		return nil, fmt.Errorf("google calendar is not configured")
	}
	token := &oauth2.Token{
		AccessToken:  *integration.AccessToken,
		RefreshToken: *integration.RefreshToken,
	}
	if integration.ExpiresAt != nil {
		token.Expiry = *integration.ExpiresAt
	}

	ts := &persistingTokenSource{
		base:         config.TokenSource(ctx, token),
		initialToken: token,
		userId:       integration.EntityId,
		ctx:          ctx,
	}

	client := oauth2.NewClient(ctx, ts)
	return calendar.NewService(ctx, option.WithHTTPClient(client))
}

func GenerateGoogleCalendarAuthURL(ctx context.Context, userInfo *model.UserInfo) string {
	config := oauth.GoogleCalendarConfig()
	if config == nil {
		return ""
	}
	// State could be a CSRF token or user ID to verify later.
	state := userInfo.UserDgraphInfo.Uuid

	// Offline access gives us a refresh token. Approval force ensures we actually get it.
	url := config.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	return url
}

func ExchangeGoogleCalendarCodeAndSave(ctx context.Context, code string, userInfo *model.UserInfo) error {
	config := oauth.GoogleCalendarConfig()
	if config == nil {
		return fmt.Errorf("google calendar is not configured")
	}

	token, err := config.Exchange(ctx, code)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ExchangeGoogleCalendarCodeAndSave Failed to exchange code for token err: %+v", err)
		return err
	}

	var refreshToken *string
	if token.RefreshToken != "" {
		refreshToken = &token.RefreshToken
	}

	userId := userInfo.UserPostgresInfo.Id // assuming uuid.UUID

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		expiresAt = &token.Expiry
	}

	err = domain.UpsertIntegration(ctx, "user", userId, "google_calendar", &token.AccessToken, refreshToken, nil, nil, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ExchangeGoogleCalendarCodeAndSave Failed to save token err: %+v", err)
		return err
	}

	return nil
}

func GetGoogleCalendarIntegration(ctx context.Context, userInfo *model.UserInfo) (*integrationModel.Integration, error) {
	userId := userInfo.UserPostgresInfo.Id
	return domain.GetIntegration(ctx, "user", userId, "google_calendar")
}

func IsGoogleCalendarConnected(ctx context.Context, userInfo *model.UserInfo) (bool, error) {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" {
		return false, nil
	}
	return true, nil
}

func UnlinkGoogleCalendar(ctx context.Context, userInfo *model.UserInfo) error {
	userId := userInfo.UserPostgresInfo.Id
	// Can optionally revoke token here via Google API
	// revTokUrl := "https://oauth2.googleapis.com/revoke?token=" + integration.AccessToken
	// http.Post(revTokUrl, "application/x-www-form-urlencoded", nil)

	err := domain.DeleteIntegration(ctx, "user", userId, "google_calendar")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnlinkGoogleCalendar Failed to delete integration err: %+v", err)
		return err
	}
	return nil
}

// HandleGoogleCalendarWebhookEvent is the entry point for Google
// Calendar push notifications. The HTTP layer authenticates the
// request via the channel token, drains the body, and forwards the
// already-extracted headers here so the business layer never needs to
// touch the http.Request — that means we can run on a detached
// background context that survives the HTTP handler returning.
//
// This is currently a stub. When wired:
//  1. Look up the integration row for channelID (set when /watch was created).
//  2. Use the per-user sync token to fetch changed events.
//  3. Reconcile with OneCamp's calendar entities (Dgraph/Postgres).
//
// channelID + channelToken + resourceState + resourceID are passed
// straight through so a future implementation has everything it needs.
func HandleGoogleCalendarWebhookEvent(ctx context.Context, channelID, channelToken, resourceState, resourceID string) error {
	_ = ctx
	_ = channelID
	_ = channelToken
	_ = resourceState
	_ = resourceID
	// TODO: implement the sync-token-driven reconcile path.
	return nil
}

func UpdateGoogleCalendarSyncTask(ctx context.Context, enabled bool, userInfo *model.UserInfo) error {
	userId := userInfo.UserPostgresInfo.Id
	err := domain.UpdateTaskSyncEnabled(ctx, "user", userId, "google_calendar", enabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateGoogleCalendarSyncTask Failed to update sync preference err: %+v", err)
		return err
	}
	return nil
}

func SyncTaskToGoogleCalendar(ctx context.Context, taskUUID string, assigneeUUIDStr string) error {
	if assigneeUUIDStr == "" {
		// Task is unassigned. If it was previously synced, we should delete it.
		// We'll handle this by fetching the task and checking if it has a GCal ID.
		ctx = context.Background() // Ensure context is alive for async execution if needed
		return syncTaskInternal(ctx, taskUUID, nil)
	}

	assigneeUUID, err := uuid.Parse(assigneeUUIDStr)
	if err != nil {
		return err
	}

	integration, err := domain.GetIntegration(ctx, "user", assigneeUUID, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" || !integration.TaskSyncEnabled {
		// Not linked or sync disabled
		return nil
	}

	return syncTaskInternal(ctx, taskUUID, integration)
}

func syncTaskInternal(ctx context.Context, taskUUIDStr string, integration *integrationModel.Integration) error {
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		return err
	}

	// Fetch task from Postgres to get GCal ID
	pgTask, err := taskDomain.GetTaskByUUID(ctx, taskUUID)
	if err != nil || pgTask == nil {
		return err
	}

	// Fetch task from Dgraph for metadata (Name, DueDate, Assignee, etc.)
	// Use "0x1" as userUid to avoid Dgraph "ID can't be empty" in server-side operations.
	dgraphTask, err := taskDomain.GetDgraphBasicTaskInfoByUUID(ctx, taskUUIDStr, "0x1")
	if err != nil || dgraphTask == nil {
		return err
	}

	// If no integration, we can't do anything (we don't have auth to delete from old assignee's calendar)
	if integration == nil {
		if pgTask.TaskGoogleCalendarId != nil {
			helpers.MessageLogs.InfoLog.Printf("Cannot delete task %s from previous assignee's calendar - no integration provided", taskUUIDStr)
		}
		return nil
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	// Safety check 1: Is this integration's owner STILL the assignee?
	isAssignee := false
	if dgraphTask.Assignee != nil && dgraphTask.Assignee.Uuid == integration.EntityId.String() {
		isAssignee = true
	}

	// Safety check 2: Is the task soft-deleted / archived?
	isArchived := !pgTask.DeletedAt.IsZero()

	// If the user lost access, the task is archived, or it lost its due date -> DELETE from their calendar
	if !isAssignee || isArchived || dgraphTask.DueDate == nil {
		if pgTask.TaskGoogleCalendarId != nil {
			err = srv.Events.Delete("primary", *pgTask.TaskGoogleCalendarId).Do()
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/syncTaskInternal Failed to delete event %s err: %+v", *pgTask.TaskGoogleCalendarId, err)
			} else {
				// Only clear the Postgres GCal ID if the person deleting it was the one who owned it
				// (If a new assignee is clearing it, we'd clear it, but here it's safer to clear it if it was successfully deleted)
				taskDomain.UpdateTaskGoogleCalendarId(ctx, pgTask.Id, nil)
				helpers.MessageLogs.InfoLog.Printf("Deleted GCal event %s for task %s because assignee removed or task archived/undated", *pgTask.TaskGoogleCalendarId, taskUUIDStr)
			}
		}
		return nil
	}

	eventSummary := dgraphTask.Name
	if dgraphTask.Status == "completed" {
		eventSummary = "[DONE] " + eventSummary
	}

	desc := ""
	if dgraphTask.Description != nil {
		desc = *dgraphTask.Description
	}

	// Google Calendar requires a Start and End date.
	dueTime := *dgraphTask.DueDate
	event := &calendar.Event{
		Summary:      eventSummary,
		Description:  desc,
		Transparency: "transparent", // Show as "Free"
		Start:        &calendar.EventDateTime{DateTime: dueTime.Format(time.RFC3339)},
		End:          &calendar.EventDateTime{DateTime: dueTime.Add(time.Hour).Format(time.RFC3339)},
	}

	if pgTask.TaskGoogleCalendarId != nil {
		_, err = srv.Events.Patch("primary", *pgTask.TaskGoogleCalendarId, event).Do()
		if err != nil && isGoogleNotFoundError(err) {
			// GCal event was deleted externally — clear stale ID and re-create
			helpers.MessageLogs.InfoLog.Printf("syncTaskInternal: GCal event %s deleted externally for task %s, re-creating", *pgTask.TaskGoogleCalendarId, taskUUIDStr)
			taskDomain.UpdateTaskGoogleCalendarId(ctx, pgTask.Id, nil)
			res, insertErr := srv.Events.Insert("primary", event).Do()
			if insertErr == nil {
				taskDomain.UpdateTaskGoogleCalendarId(ctx, pgTask.Id, &res.Id)
				helpers.MessageLogs.InfoLog.Printf("Re-created GCal event %s for task %s", res.Id, taskUUIDStr)
			}
			err = insertErr
		}
	} else {
		res, err := srv.Events.Insert("primary", event).Do()
		if err == nil {
			// Save new GCal ID to Postgres
			err = taskDomain.UpdateTaskGoogleCalendarId(ctx, pgTask.Id, &res.Id)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/syncTaskInternal Failed to update task gcal id err: %+v", err)
			}
			helpers.MessageLogs.InfoLog.Printf("Created GCal event %s for task %s", res.Id, taskUUIDStr)
		}
	}

	return err
}

func FetchUserGoogleCalendarEvents(ctx context.Context, userInfo *model.UserInfo, startDate *time.Time, endDate *time.Time) ([]*calendar.Event, error) {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return nil, nil // Not connected or no token
	}

	// Use a bounded context so stale-token refresh can't block the entire events response
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	srv, err := getGoogleCalendarService(fetchCtx, integration)
	if err != nil {
		return nil, err
	}

	query := srv.Events.List("primary").ShowDeleted(false).SingleEvents(true).OrderBy("startTime")
	if startDate != nil {
		query = query.TimeMin(startDate.Format(time.RFC3339))
	} else {
		// Default to 1 month ago if no start date provided
		oneMonthAgo := time.Now().AddDate(0, -1, 0)
		query = query.TimeMin(oneMonthAgo.Format(time.RFC3339))
	}
	if endDate != nil {
		query = query.TimeMax(endDate.Format(time.RFC3339))
	} else {
		// Default to +3 months if no end date provided
		threeMonthsLater := time.Now().AddDate(0, 3, 0)
		query = query.TimeMax(threeMonthsLater.Format(time.RFC3339))
	}

	eventsData, err := query.Do()
	if err != nil {
		return nil, err
	}

	return eventsData.Items, nil
}

func SyncEventToGoogleCalendar(ctx context.Context, eventId uuid.UUID, input adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo, syncFlag bool) error {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" {
		return nil // Not connected
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	event := &calendar.Event{
		Summary:     input.Title,
		Description: input.Description,
		Start: &calendar.EventDateTime{
			DateTime: input.StartTime,
		},
		End: &calendar.EventDateTime{
			DateTime: input.EndTime,
		},
	}

	// Resolve participants to emails
	if len(input.Participants) > 0 {
		var attendees []*calendar.EventAttendee
		// Batched lookup: one Dgraph round-trip for all participant
		// emails. Replaces a per-participant loop that was the worst
		// N+1 in the calendar sync path for large meetings.
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, input.Participants)
		for _, pInfo := range users {
			if pInfo != nil && pInfo.EmailID != "" {
				attendees = append(attendees, &calendar.EventAttendee{Email: pInfo.EmailID})
			}
		}
		event.Attendees = attendees
	}

	// This is a simplified version of sync logic for now.
	// In production, we'd check if the event already has a GCal ID.
	if syncFlag {
		res, err := srv.Events.Insert("primary", event).Do()
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/SyncEventToGoogleCalendar failed to insert event err: %+v", err)
			return err
		}

		// Save the Google Calendar Event ID back to OneCamp
		gcalId := res.Id

		// 1. Update Postgres
		// We need to parse times again for the update function if needed, or but we have the originals if it's simpler.
		// Actually, let's just use the ones from input.
		startTime, _ := time.Parse(time.RFC3339, input.StartTime)
		endTime, _ := time.Parse(time.RFC3339, input.EndTime)

		err = calendarDomain.UpdateCalendarEvent(ctx, eventId, input.Title, input.Description, startTime, endTime, &gcalId, time.Now())
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/SyncEventToGoogleCalendar failed to update postgres with gcal id err: %+v", err)
		}

		// 2. Update Dgraph
		// CRITICAL: Uid MUST be "uid(event)" to reference the query variable in the upsert.
		// Without it, the JSON omits uid (omitempty) and Dgraph creates a NEW orphan node
		// instead of updating the existing one.
		dgraphEvent := &dgraphStruct.DgraphEvent{
			Uid:                   "uid(event)",
			Uuid:                  eventId.String(),
			GoogleCalendarEventId: &gcalId,
		}
		_, err = calendarDomain.CreateOrUpdateDgraphEvent(ctx, dgraphEvent)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/SyncEventToGoogleCalendar failed to update dgraph with gcal id err: %+v", err)
		}
	}
	return nil
}

func UpdateGoogleCalendarEvent(ctx context.Context, googleEventId string, input adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo) error {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return fmt.Errorf("google calendar not connected")
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	event := &calendar.Event{
		Summary:     input.Title,
		Description: input.Description,
		Start: &calendar.EventDateTime{
			DateTime: input.StartTime,
		},
		End: &calendar.EventDateTime{
			DateTime: input.EndTime,
		},
	}

	// Resolve participants to emails
	if len(input.Participants) > 0 {
		var attendees []*calendar.EventAttendee
		// Batched lookup: one Dgraph round-trip for all participant
		// emails. Replaces a per-participant loop that was the worst
		// N+1 in the calendar sync path for large meetings.
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, input.Participants)
		for _, pInfo := range users {
			if pInfo != nil && pInfo.EmailID != "" {
				attendees = append(attendees, &calendar.EventAttendee{Email: pInfo.EmailID})
			}
		}
		event.Attendees = attendees
	}

	_, err = srv.Events.Patch("primary", googleEventId, event).Do()
	if err != nil {
		if isGoogleNotFoundError(err) {
			// The GCal event was deleted externally (e.g., user disconnected and deleted on Google).
			// Clear the stale reference so it doesn't cause repeated failures.
			helpers.MessageLogs.InfoLog.Printf("UpdateGoogleCalendarEvent: GCal event %s no longer exists, stale reference will be cleared by caller", googleEventId)
			return &GCalEventGoneError{GoogleEventId: googleEventId}
		}
		helpers.LogErrorWithContext(ctx, "business/UpdateGoogleCalendarEvent failed to patch event %s err: %+v", googleEventId, err)
	}
	return err
}

func DeleteGoogleCalendarEvent(ctx context.Context, googleEventId string, userInfo *model.UserInfo) error {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return fmt.Errorf("google calendar not connected")
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	err = srv.Events.Delete("primary", googleEventId).Do()
	if err != nil {
		if isGoogleNotFoundError(err) {
			// Already deleted on Google — treat as success
			helpers.MessageLogs.InfoLog.Printf("DeleteGoogleCalendarEvent: GCal event %s already deleted, treating as success", googleEventId)
			return nil
		}
		return err
	}
	return nil
}

// RemoveAttendeeFromGoogleCalendarEvent removes a specific attendee from a Google Calendar event
// by patching the attendee list on the event creator's calendar.
func RemoveAttendeeFromGoogleCalendarEvent(ctx context.Context, googleEventId string, leavingUserEmail string, creatorUserInfo *model.UserInfo) error {
	userId := creatorUserInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" {
		return nil // Creator not connected to GCal, nothing to do
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	// Fetch the existing event to get the current attendee list
	existingEvent, err := srv.Events.Get("primary", googleEventId).Do()
	if err != nil {
		os.WriteFile("/tmp/gcal_leave_debug.log", []byte(fmt.Sprintf("Failed to Get event %s: %v\n", googleEventId, err)), 0644)
		helpers.LogErrorWithContext(ctx, "business/RemoveAttendeeFromGoogleCalendarEvent failed to get event %s err: %+v", googleEventId, err)
		return err
	}

	debugLog := fmt.Sprintf("Trying to remove user: %s from event %s\n", leavingUserEmail, googleEventId)
	debugLog += fmt.Sprintf("Original attendees count: %d\n", len(existingEvent.Attendees))

	// Filter out the leaving user's email (case-insensitive)
	var updatedAttendees []*calendar.EventAttendee
	for _, a := range existingEvent.Attendees {
		debugLog += fmt.Sprintf(" - Seeing attendee: %s\n", a.Email)
		if strings.ToLower(a.Email) != strings.ToLower(leavingUserEmail) {
			updatedAttendees = append(updatedAttendees, a)
		} else {
			debugLog += fmt.Sprintf("   -> MATCH! Removing %s\n", a.Email)
		}
	}

	debugLog += fmt.Sprintf("Updated attendees count: %d\n", len(updatedAttendees))

	// Patch with updated attendee list.
	// ForceSendFields ensures we send the empty array if updatedAttendees is empty,
	// bypassing the `omitempty` JSON tag which would otherwise ignore the change.
	patch := &calendar.Event{
		Attendees:       updatedAttendees,
		ForceSendFields: []string{"Attendees"},
	}
	// ForceSendUpdates sends cancellation notifications to the removed attendee
	responseEvent, err := srv.Events.Patch("primary", googleEventId, patch).SendUpdates("all").Do()
	if err != nil {
		debugLog += fmt.Sprintf("PATCH FAILED: %v\n", err)
		helpers.LogErrorWithContext(ctx, "business/RemoveAttendeeFromGoogleCalendarEvent failed to patch event %s err: %+v", googleEventId, err)
	} else {
		debugLog += fmt.Sprintf("PATCH SUCCESS! New attendee count returned by Google: %d\n", len(responseEvent.Attendees))
	}

	// Write log file
	f, _ := os.OpenFile("/tmp/gcal_leave_debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if f != nil {
		f.WriteString("-----------------------\n" + time.Now().String() + "\n" + debugLog)
		f.Close()
	}

	return err
}

// DeclineGoogleCalendarEvent sets the current user's attendee status to "declined" on a Google Calendar event.
// This is used when a participant leaves a GCal-only event (not created in OneCamp).
func DeclineGoogleCalendarEvent(ctx context.Context, googleEventId string, userInfo *model.UserInfo) error {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" {
		return fmt.Errorf("google calendar not connected")
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return err
	}

	// Fetch the existing event to find the user's attendee entry
	existingEvent, err := srv.Events.Get("primary", googleEventId).Do()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeclineGoogleCalendarEvent failed to get event %s err: %+v", googleEventId, err)
		return err
	}

	userEmail := userInfo.UserPostgresInfo.EmailID
	found := false
	for _, a := range existingEvent.Attendees {
		if strings.ToLower(a.Email) == strings.ToLower(userEmail) {
			a.ResponseStatus = "declined"
			found = true
			break
		}
	}

	if !found {
		// User is not in the attendee list; just delete the event from their calendar view
		return srv.Events.Delete("primary", googleEventId).Do()
	}

	patch := &calendar.Event{
		Attendees: existingEvent.Attendees,
	}
	_, err = srv.Events.Patch("primary", googleEventId, patch).SendUpdates("all").Do()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeclineGoogleCalendarEvent failed to patch event %s err: %+v", googleEventId, err)
	}
	return err
}

// HandleGoogleCalendarWebhook is the legacy entry point; retained to
// keep the package's external surface stable. Delegates to the new
// header-only variant.
func HandleGoogleCalendarWebhook(ctx context.Context, req *http.Request) error {
	if req == nil {
		return nil
	}
	return HandleGoogleCalendarWebhookEvent(ctx,
		req.Header.Get("X-Goog-Channel-ID"),
		req.Header.Get("X-Goog-Channel-Token"),
		req.Header.Get("X-Goog-Resource-State"),
		req.Header.Get("X-Goog-Resource-ID"),
	)
}

func CreateGoogleCalendarEvent(ctx context.Context, input adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo) (string, error) {
	userId := userInfo.UserPostgresInfo.Id
	integration, err := domain.GetIntegration(ctx, "user", userId, "google_calendar")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return "", fmt.Errorf("google calendar not connected")
	}

	srv, err := getGoogleCalendarService(ctx, integration)
	if err != nil {
		return "", err
	}

	event := &calendar.Event{
		Summary:     input.Title,
		Description: input.Description,
		Start: &calendar.EventDateTime{
			DateTime: input.StartTime,
		},
		End: &calendar.EventDateTime{
			DateTime: input.EndTime,
		},
	}

	// Resolve participants to emails
	if len(input.Participants) > 0 {
		var attendees []*calendar.EventAttendee
		// Batched lookup: one Dgraph round-trip for all participant
		// emails. Replaces a per-participant loop that was the worst
		// N+1 in the calendar sync path for large meetings.
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, input.Participants)
		for _, pInfo := range users {
			if pInfo != nil && pInfo.EmailID != "" {
				attendees = append(attendees, &calendar.EventAttendee{Email: pInfo.EmailID})
			}
		}
		event.Attendees = attendees
	}

	res, err := srv.Events.Insert("primary", event).Do()
	if err != nil {
		return "", err
	}

	return res.Id, nil
}

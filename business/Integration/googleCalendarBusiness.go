package Integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	calendarDomain "github.com/akashc777/OneCamp/domain/Calendar"
	domain "github.com/akashc777/OneCamp/domain/Integration"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/oauth"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	integrationModel "github.com/akashc777/OneCamp/models/postgres/Integration"
	taskModel "github.com/akashc777/OneCamp/models/postgres/Task"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
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

// storedToken reads a token as the integrations row holds it. A link made
// through the connectors flow, the only way to link Google Calendar now, keeps
// its tokens sealed (helpers.EncryptSecret). A link made before that holds them
// in the clear and keeps working: a value that isn't sealed under this
// install's key is used as it is.
func storedToken(stored *string) string {
	if stored == nil || *stored == "" {
		return ""
	}
	if plain, err := helpers.DecryptSecret(*stored); err == nil && plain != "" {
		return plain
	}
	return *stored
}

// sealToken is a token as the integrations row keeps it (storedToken reads it).
func sealToken(plain string) (*string, error) {
	sealed, err := helpers.EncryptSecret(plain)
	if err != nil {
		return nil, err
	}
	return &sealed, nil
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
		// Refreshed: store it sealed, as the connectors flow does, so the
		// assistant's connector can read the same row.
		refresh := t.RefreshToken
		if refresh == "" && s.initialToken != nil {
			refresh = s.initialToken.RefreshToken
		}
		s.initialToken = t
		access, aerr := sealToken(t.AccessToken)
		sealedRefresh, rerr := sealToken(refresh)
		if aerr != nil || rerr != nil {
			helpers.MessageLogs.ErrorLog.Printf("Google Calendar token for user %s refreshed but not stored: it couldn't be sealed", s.userId)
			return t, nil
		}
		domain.UpdateIntegrationToken(s.ctx, "user", s.userId, "google_calendar", access, sealedRefresh, &t.Expiry)
		helpers.MessageLogs.InfoLog.Printf("Refreshed Google Calendar token for user %s", s.userId)
	}
	return t, nil
}

func getGoogleCalendarService(ctx context.Context, integration *integrationModel.Integration) (*calendar.Service, error) {
	if integration == nil || storedToken(integration.RefreshToken) == "" {
		return nil, fmt.Errorf("google calendar not connected")
	}

	config := oauth.GoogleCalendarConfig()
	if config == nil {
		return nil, fmt.Errorf("google calendar is not configured")
	}
	token := &oauth2.Token{
		AccessToken:  storedToken(integration.AccessToken),
		RefreshToken: storedToken(integration.RefreshToken),
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

// integrationOf reads a person's link to an app; swapped in tests.
var integrationOf = domain.GetIntegration

// A task's row, its graph entry, the write of its Google copy's id and the
// calendar a link opens, as the task sync reads them; swapped in tests.
var (
	taskRowOf         = taskDomain.GetTaskByUUID
	taskGraphOf       = taskDomain.GetDgraphBasicTaskInfoByUUID
	setTaskGoogleID   = taskDomain.UpdateTaskGoogleCalendarId
	calendarOfLink    = getGoogleCalendarService
	clearTaskGoogleID = taskDomain.ClearTaskGoogleCalendarIdIf
)

// taskCopyLine runs the passes on one task's Google copy, hand-overs and
// syncs, one at a time in the order they were asked for, so each reads the row
// the one before it wrote. It spans this process only: across server
// instances the compare-and-set clear (ClearTaskGoogleCalendarIdIf) is what
// keeps a newer id. googleCallTimeout bounds each Google call a pass makes, so
// a call that never answers can't hold the line.
var (
	taskCopyLine      = &helpers.InOrder{}
	googleCallTimeout = 8 * time.Second
)

var errTaskCopyPass = errors.New("the pass on the task's Google copy stopped")

// inTaskCopyLine hands pass to the task's line now and answers once it ran.
func inTaskCopyLine(taskUUID, name string, pass func() error) <-chan error {
	done := make(chan error, 1)
	taskCopyLine.Go(taskUUID, name, func() {
		err := errTaskCopyPass // what the caller hears if pass panics
		defer func() { done <- err }()
		err = pass()
	})
	return done
}

// googleCall bounds one Google call.
func googleCall(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, googleCallTimeout)
}

// googleServiceFor is a person's Google Calendar, its calls bound to ctx.
// (nil, nil) when they haven't linked Google. A link is its refresh token,
// sealed or plain (storedToken): a link whose access token is empty or sealed
// is still a link.
func googleServiceFor(ctx context.Context, userID uuid.UUID) (*calendar.Service, error) {
	integration, err := integrationOf(ctx, "user", userID, "google_calendar")
	if err != nil {
		return nil, err
	}
	if integration == nil || storedToken(integration.RefreshToken) == "" {
		return nil, nil
	}
	return calendarOfLink(ctx, integration)
}

// errGoogleNotLinked is a change to a Google copy for someone who hasn't
// linked Google.
var errGoogleNotLinked = fmt.Errorf("google calendar not connected")

// GoogleCalendarAccount is the address of the Google account a person linked,
// or "" when it isn't known (a link made before it was kept).
func GoogleCalendarAccount(integration *integrationModel.Integration) string {
	if integration == nil || integration.Metadata == nil || *integration.Metadata == "" {
		return ""
	}
	var meta struct {
		AccountEmail string `json:"account_email"`
	}
	if err := json.Unmarshal([]byte(*integration.Metadata), &meta); err != nil {
		return ""
	}
	return meta.AccountEmail
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

	err := domain.DeleteIntegration(ctx, "user", userId, "google_calendar", userId)
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

// SyncTaskToGoogleCalendar brings the task's copy in its assignee's Google
// calendar up to date (assigneeUUIDStr is that person's uuid, "" nobody):
// only for someone linked with task sync on. A hand-over from one person to
// another goes through MoveTaskGoogleCopy instead. It waits its turn in the
// task's line behind every pass asked for before it.
func SyncTaskToGoogleCalendar(ctx context.Context, taskUUID string, assigneeUUIDStr string) error {
	return <-inTaskCopyLine(taskUUID, "calendar.syncTaskCopy", func() error {
		return syncTaskCopy(ctx, taskUUID, assigneeUUIDStr)
	})
}

// syncTaskCopy is SyncTaskToGoogleCalendar's pass, run in the task's line.
func syncTaskCopy(ctx context.Context, taskUUID string, assigneeUUIDStr string) error {
	if assigneeUUIDStr == "" {
		// Unassigned: nobody's link may touch a copy left from before.
		return syncTaskInternal(ctx, taskUUID, nil)
	}
	integration, err := taskSyncLink(ctx, assigneeUUIDStr)
	if err != nil || integration == nil {
		return err
	}
	return syncTaskInternal(ctx, taskUUID, integration)
}

// taskSyncLink is a person's Google link when it may carry their tasks: a
// refresh token and task sync on. nil when it may not, or can't be read.
func taskSyncLink(ctx context.Context, person string) (*integrationModel.Integration, error) {
	personUUID, err := uuid.Parse(person)
	if err != nil {
		return nil, err
	}
	integration, err := integrationOf(ctx, "user", personUUID, "google_calendar")
	if err != nil || integration == nil || integration.RefreshToken == nil || *integration.RefreshToken == "" || !integration.TaskSyncEnabled {
		return nil, nil
	}
	return integration, nil
}

// MoveTaskGoogleCopy follows a task from one assignee to the next (person
// uuids, "" nobody). It takes the task's place in taskCopyLine now, in the
// order the hand-overs were asked for, and runs in the background once every
// earlier pass on the task is done; the channel gets its answer. ctx is the
// door's pgexec.Detached(ctx): the request's values, never its cancel or a
// transaction it holds (googleCopyDoors_test.go). One process never runs two
// passes on a task at once; a second server instance can, and then only the
// compare-and-set clear keeps the newer id. The copy in from's calendar comes out
// first, through from's own link, whether or not from's task sync is on: only
// the copy we wrote, only with from's token, only by the system. Once that
// removal has been tried the id is no one's, so to never patches it in their
// own calendar; to then gets a copy of their own, as SyncTaskToGoogleCalendar
// gives it. from without a usable link can't be reached: that copy stays
// where it is (V-1116).
func MoveTaskGoogleCopy(ctx context.Context, taskUUID, from, to string) <-chan error {
	return inTaskCopyLine(taskUUID, "Task.assignee.calendar", func() error {
		return moveTaskGoogleCopy(ctx, taskUUID, from, to)
	})
}

// moveTaskGoogleCopy is MoveTaskGoogleCopy's pass, run in the task's line.
func moveTaskGoogleCopy(ctx context.Context, taskUUID, from, to string) error {
	if from == to {
		return syncTaskCopy(ctx, taskUUID, to)
	}
	taskID, err := uuid.Parse(taskUUID)
	if err != nil {
		return err
	}
	pgTask, err := taskRowOf(ctx, taskID)
	if err != nil || pgTask == nil {
		return err
	}
	if from != "" && pgTask.TaskGoogleCalendarId != nil {
		removeFromCopy(ctx, pgTask, taskUUID, from)
	}
	if to == "" {
		return nil
	}
	integration, err := taskSyncLink(ctx, to)
	if err != nil || integration == nil {
		return err
	}
	return syncLoadedTask(ctx, pgTask, integration, taskUUID)
}

// removeFromCopy takes the task's copy out of the previous assignee's
// calendar for a hand-over, and forgets its id once that was tried. A copy
// Google says is gone (404, 410) is simply forgotten; any other failure (401,
// 403, 5xx, 429, a timeout, the network) leaves it in from's calendar, and
// that orphan is logged once with its id for V-1116's removal queue.
func removeFromCopy(ctx context.Context, pgTask *taskModel.Task, taskUUID, from string) {
	fromUUID, err := uuid.Parse(from)
	if err != nil {
		return
	}
	srv, err := googleServiceFor(ctx, fromUUID)
	if err != nil || srv == nil {
		helpers.MessageLogs.InfoLog.Printf("Task %s: its Google copy stays in the previous assignee's calendar: no usable link", taskUUID)
		return
	}
	eventID := *pgTask.TaskGoogleCalendarId
	if err := removeTaskCopy(ctx, srv, pgTask, taskUUID); err != nil {
		if !copyGone(err) {
			helpers.MessageLogs.InfoLog.Printf("Task %s: its Google copy %s stays in the previous assignee's calendar as an orphan (V-1116): %v", taskUUID, eventID, err)
		}
		_ = clearTaskGoogleID(ctx, pgTask.Id, eventID)
	}
	pgTask.TaskGoogleCalendarId = nil
}

// ArchiveTaskGoogleCopy takes an archived task's copy out of its assignee's
// calendar, through the assignee's own link, whether or not their task sync
// is on (V-1119): only the copy we wrote (the id the task's row keeps), only
// with the assignee's own token, only by the system. It takes the task's place
// in taskCopyLine now, in the order asked, as MoveTaskGoogleCopy does, and runs
// once every earlier pass on the task is done; the channel gets its answer.
// ctx is as MoveTaskGoogleCopy's.
// The assignee is the one the graph names at the pass, so a hand-over asked
// before it has already moved the copy. A copy Google says is gone (404, 410)
// is forgotten; any other failure keeps the id, and no later pass on the task
// tries again (a bulk delete skips an archived task, an unarchive with task
// sync off makes no DELETE): the archived row's kept id is what V-1116's
// removal queue takes the copy out by.
func ArchiveTaskGoogleCopy(ctx context.Context, taskUUID string) <-chan error {
	return inTaskCopyLine(taskUUID, "Task.archive.calendar", func() error {
		return archiveTaskCopy(ctx, taskUUID)
	})
}

// archiveTaskCopy is ArchiveTaskGoogleCopy's pass, run in the task's line.
func archiveTaskCopy(ctx context.Context, taskUUID string) error {
	taskID, err := uuid.Parse(taskUUID)
	if err != nil {
		return err
	}
	pgTask, err := taskRowOf(ctx, taskID)
	if err != nil || pgTask == nil || pgTask.TaskGoogleCalendarId == nil || pgTask.DeletedAt.IsZero() {
		return err // no copy, or the task is live again
	}
	dgraphTask, err := taskGraphOf(ctx, taskUUID, "0x1")
	if err != nil || dgraphTask == nil {
		return err
	}
	eventID := *pgTask.TaskGoogleCalendarId
	var srv *calendar.Service
	if dgraphTask.Assignee != nil {
		if assignee, perr := uuid.Parse(dgraphTask.Assignee.Uuid); perr == nil {
			if srv, err = googleServiceFor(ctx, assignee); err != nil {
				return err
			}
		}
	}
	if srv == nil {
		helpers.MessageLogs.InfoLog.Printf("Task %s: its Google copy %s stays in the assignee's calendar: no usable link", taskUUID, eventID)
		return nil
	}
	if err := removeTaskCopy(ctx, srv, pgTask, taskUUID); err != nil {
		if copyGone(err) {
			return clearTaskGoogleID(ctx, pgTask.Id, eventID)
		}
		return err
	}
	return nil
}

// removeTaskCopy takes the task's Google copy out of the calendar srv opens
// and, once it's out, forgets its id unless another pass wrote a newer one
// meanwhile. An answer no retry changes (the copy
// isn't there, 404/410, or the link may no longer touch it, 401/403) is
// logged once as an orphan; every error goes back to the caller, unretried.
func removeTaskCopy(ctx context.Context, srv *calendar.Service, pgTask *taskModel.Task, taskUUID string) error {
	eventID := *pgTask.TaskGoogleCalendarId
	callCtx, cancel := googleCall(ctx)
	defer cancel()
	if err := srv.Events.Delete("primary", eventID).Context(callCtx).Do(); err != nil {
		if copyOutOfReach(err) {
			helpers.MessageLogs.InfoLog.Printf("Task %s: its Google copy %s is out of reach, left where it is: %v", taskUUID, eventID, err)
		} else {
			helpers.LogErrorWithContext(ctx, "business/removeTaskCopy Failed to delete event %s err: %+v", eventID, err)
		}
		return err
	}
	_ = clearTaskGoogleID(ctx, pgTask.Id, eventID)
	helpers.MessageLogs.InfoLog.Printf("Deleted GCal event %s for task %s", eventID, taskUUID)
	return nil
}

// copyGone is Google saying the copy isn't there: 404 or 410, by the code.
func copyGone(err error) bool {
	var gErr *googleapi.Error
	return errors.As(err, &gErr) && (gErr.Code == http.StatusNotFound || gErr.Code == http.StatusGone)
}

// copyOutOfReach is a Google answer no retry changes: the event isn't there
// (404, 410) or the link may no longer touch it (401, 403).
func copyOutOfReach(err error) bool {
	if isGoogleNotFoundError(err) {
		return true
	}
	switch connectorBusiness.ClassifyAPIError(err) {
	case connectorBusiness.ProblemExpired, connectorBusiness.ProblemPermissions:
		return true
	}
	return false
}

func syncTaskInternal(ctx context.Context, taskUUIDStr string, integration *integrationModel.Integration) error {
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		return err
	}

	// Fetch task from Postgres to get GCal ID
	pgTask, err := taskRowOf(ctx, taskUUID)
	if err != nil || pgTask == nil {
		return err
	}
	return syncLoadedTask(ctx, pgTask, integration, taskUUIDStr)
}

// syncLoadedTask is syncTaskInternal for a task row already read.
func syncLoadedTask(ctx context.Context, pgTask *taskModel.Task, integration *integrationModel.Integration, taskUUIDStr string) error {
	// Fetch task from Dgraph for metadata (Name, DueDate, Assignee, etc.)
	// Use "0x1" as userUid to avoid Dgraph "ID can't be empty" in server-side operations.
	dgraphTask, err := taskGraphOf(ctx, taskUUIDStr, "0x1")
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

	srv, err := calendarOfLink(ctx, integration)
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
			_ = removeTaskCopy(ctx, srv, pgTask, taskUUIDStr) // logged there; a failure keeps the id
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

	callCtx, cancel := googleCall(ctx)
	defer cancel()
	if pgTask.TaskGoogleCalendarId != nil {
		_, err = srv.Events.Patch("primary", *pgTask.TaskGoogleCalendarId, event).Context(callCtx).Do()
		if err != nil && isGoogleNotFoundError(err) {
			// GCal event was deleted externally — clear stale ID and re-create
			helpers.MessageLogs.InfoLog.Printf("syncTaskInternal: GCal event %s deleted externally for task %s, re-creating", *pgTask.TaskGoogleCalendarId, taskUUIDStr)
			_ = clearTaskGoogleID(ctx, pgTask.Id, *pgTask.TaskGoogleCalendarId)
			insertCtx, cancelInsert := googleCall(ctx)
			defer cancelInsert()
			res, insertErr := srv.Events.Insert("primary", event).Context(insertCtx).Do()
			if insertErr == nil {
				setTaskGoogleID(ctx, pgTask.Id, &res.Id)
				helpers.MessageLogs.InfoLog.Printf("Re-created GCal event %s for task %s", res.Id, taskUUIDStr)
			}
			err = insertErr
		}
	} else {
		res, err := srv.Events.Insert("primary", event).Context(callCtx).Do()
		if err == nil {
			// Save new GCal ID to Postgres
			err = setTaskGoogleID(ctx, pgTask.Id, &res.Id)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "business/syncTaskInternal Failed to update task gcal id err: %+v", err)
			}
			helpers.MessageLogs.InfoLog.Printf("Created GCal event %s for task %s", res.Id, taskUUIDStr)
		}
	}

	return err
}

func FetchUserGoogleCalendarEvents(ctx context.Context, userInfo *model.UserInfo, startDate *time.Time, endDate *time.Time) ([]*calendar.Event, error) {
	// Use a bounded context so stale-token refresh can't block the entire
	// events response; a busy range takes a few pages.
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	srv, err := googleServiceFor(fetchCtx, userInfo.UserPostgresInfo.Id)
	if err != nil || srv == nil {
		return nil, err
	}

	// Without a range: a month back and three ahead.
	from, to := time.Now().AddDate(0, -1, 0), time.Now().AddDate(0, 3, 0)
	if startDate != nil {
		from = *startDate
	}
	if endDate != nil {
		to = *endDate
	}
	return listGoogleEvents(fetchCtx, srv, from, to)
}

// maxGoogleEvents caps the Google events one range lists: ten of Google's
// pages. A busier range shows its first 2,500, and the log says so.
const maxGoogleEvents = 2500

// listGoogleEvents lists the events on a primary calendar between from and
// to, page after page. It read one page, Google's first 250, so in a busy
// month (repeating meetings each count) the rest were missing.
func listGoogleEvents(ctx context.Context, srv *calendar.Service, from, to time.Time) ([]*calendar.Event, error) {
	call := srv.Events.List("primary").ShowDeleted(false).SingleEvents(true).OrderBy("startTime").
		TimeMin(from.Format(time.RFC3339)).TimeMax(to.Format(time.RFC3339)).Context(ctx)
	var out []*calendar.Event
	for {
		page, err := call.Do()
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if len(out) >= maxGoogleEvents {
			if len(out) > maxGoogleEvents || page.NextPageToken != "" {
				helpers.LogInfoWithContext(ctx, "business/Integration listGoogleEvents: more than %d Google events between %s and %s; the first %d are listed",
					maxGoogleEvents, from.Format(time.RFC3339), to.Format(time.RFC3339), maxGoogleEvents)
			}
			return out[:maxGoogleEvents], nil
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		call.PageToken(page.NextPageToken)
	}
}

// FetchUserGoogleCalendarEvent reads one event from the person's own primary
// Google calendar. (nil, nil) when they haven't linked Google, or when Google
// has no such event for them: deleted, or never on their calendar.
func FetchUserGoogleCalendarEvent(ctx context.Context, userInfo *model.UserInfo, googleEventId string) (*calendar.Event, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	srv, err := googleServiceFor(fetchCtx, userInfo.UserPostgresInfo.Id)
	if err != nil || srv == nil {
		return nil, err
	}
	item, err := srv.Events.Get("primary", googleEventId).Context(fetchCtx).Do()
	if err != nil {
		if isGoogleNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}
	if item == nil || item.Status == "cancelled" {
		return nil, nil
	}
	return item, nil
}

// BusySpan is a stretch of time a Google calendar shows someone busy.
type BusySpan struct {
	Start time.Time
	End   time.Time
}

// ErrGoogleBusyUnread is busy times read in part: Google couldn't answer for
// at least one of the person's calendars. Its time is unknown, never free.
var ErrGoogleBusyUnread = errors.New("a Google calendar couldn't be read")

// FetchUserGoogleBusy is when a person's Google calendars show them busy
// between from and to: their primary calendar and every other one they have
// selected in Google, read with FreeBusy (one call for all of them, and no
// event's title). Only the primary calendar counted, so a team calendar or a
// second work calendar never blocked anything. (nil, nil) when they haven't
// linked Google. An error means some or all of it couldn't be read; the spans
// that could be are still returned.
func FetchUserGoogleBusy(ctx context.Context, userID uuid.UUID, from, to time.Time) ([]BusySpan, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	srv, err := googleServiceFor(fetchCtx, userID)
	if err != nil || srv == nil {
		return nil, err
	}
	return googleBusy(fetchCtx, srv, from, to)
}

const (
	// busyQueryDays bounds one FreeBusy query's range; a longer one is asked in parts.
	busyQueryDays = 30
	// maxBusyCalendars is the most calendars one FreeBusy query takes.
	maxBusyCalendars = 50
)

// googleBusy reads busy times from FreeBusy, over the calendars busyCalendars
// names.
func googleBusy(ctx context.Context, srv *calendar.Service, from, to time.Time) ([]BusySpan, error) {
	if !to.After(from) {
		return nil, nil
	}
	ids := busyCalendars(ctx, srv)
	items := make([]*calendar.FreeBusyRequestItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, &calendar.FreeBusyRequestItem{Id: id})
	}
	var spans []BusySpan
	unread := false
	for start := from; start.Before(to); {
		end := start.AddDate(0, 0, busyQueryDays)
		if end.After(to) {
			end = to
		}
		res, err := srv.Freebusy.Query(&calendar.FreeBusyRequest{
			TimeMin: start.UTC().Format(time.RFC3339),
			TimeMax: end.UTC().Format(time.RFC3339),
			Items:   items,
		}).Context(ctx).Do()
		if err != nil {
			return spans, err
		}
		for _, cal := range res.Calendars {
			if len(cal.Errors) > 0 {
				unread = true
				continue
			}
			for _, p := range cal.Busy {
				if p == nil {
					continue
				}
				s, err1 := time.Parse(time.RFC3339, p.Start)
				e, err2 := time.Parse(time.RFC3339, p.End)
				if err1 == nil && err2 == nil && e.After(s) {
					spans = append(spans, BusySpan{Start: s, End: e})
				}
			}
		}
		start = end
	}
	if unread {
		return spans, ErrGoogleBusyUnread
	}
	return spans, nil
}

// busyCalendars are the calendars whose events make someone busy: their
// primary calendar and every other one they have selected (shown) in Google
// Calendar. When the list can't be read, the primary calendar alone.
func busyCalendars(ctx context.Context, srv *calendar.Service) []string {
	ids := []string{"primary"}
	err := srv.CalendarList.List().MinAccessRole("freeBusyReader").Pages(ctx, func(page *calendar.CalendarList) error {
		for _, c := range page.Items {
			if c == nil || c.Id == "" || c.Primary || !c.Selected || c.Deleted || len(ids) >= maxBusyCalendars {
				continue
			}
			ids = append(ids, c.Id)
		}
		return nil
	})
	if err != nil {
		helpers.LogInfoWithContext(ctx, "business/Integration busyCalendars: the calendar list couldn't be read, so only the primary calendar counts: %v", err)
		return []string{"primary"}
	}
	return ids
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

		// The calendar's one store keeps the copy's id and nothing else: the
		// writes below rewrote the event's title, notes and times from the
		// create input after the round trip, undoing an edit made meanwhile
		// (BE-F-6).
		if helpers.CalendarInPostgres() {
			if err := calendarDomain.SetEventGoogleID(ctx, eventId, gcalId); err != nil {
				helpers.LogErrorWithContext(ctx, "business/SyncEventToGoogleCalendar failed to keep the gcal id err: %+v", err)
			}
			return nil
		}

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

// UpdateGoogleCalendarEvent writes an edit to the Google copy of an event.
// With syncGuests, input.Participants is the whole guest list, an empty one
// included; without it the guests on Google are left alone.
func UpdateGoogleCalendarEvent(ctx context.Context, googleEventId string, input adapter.CreateOrUpdateEventInput, userInfo *model.UserInfo, syncGuests bool) error {
	srv, err := googleServiceFor(ctx, userInfo.UserPostgresInfo.Id)
	if err != nil {
		return err
	}
	if srv == nil {
		return errGoogleNotLinked
	}

	var guestEmails []string
	if syncGuests && len(input.Participants) > 0 {
		// Batched lookup: one Dgraph round-trip for all participant
		// emails. Replaces a per-participant loop that was the worst
		// N+1 in the calendar sync path for large meetings.
		users, _ := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, input.Participants)
		for _, pInfo := range users {
			if pInfo != nil && pInfo.EmailID != "" {
				guestEmails = append(guestEmails, pInfo.EmailID)
			}
		}
	}

	err = patchGoogleEvent(ctx, srv, googleEventId, googleEventPatch(input, guestEmails, syncGuests))
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

// googleEventPatch is what an edit sends to the Google copy of an event. A
// patch leaves out every empty value unless told to send it, so notes cleared
// in OneCamp stayed on Google, and so did the last guest taken off. The notes
// always go, and the guest list goes whole when syncGuests. Pure.
func googleEventPatch(input adapter.CreateOrUpdateEventInput, guestEmails []string, syncGuests bool) *calendar.Event {
	ev := &calendar.Event{
		Summary:         input.Title,
		Description:     input.Description,
		Start:           &calendar.EventDateTime{DateTime: input.StartTime},
		End:             &calendar.EventDateTime{DateTime: input.EndTime},
		ForceSendFields: []string{"Description"},
	}
	if syncGuests {
		ev.Attendees = make([]*calendar.EventAttendee, 0, len(guestEmails))
		for _, email := range guestEmails {
			ev.Attendees = append(ev.Attendees, &calendar.EventAttendee{Email: email})
		}
		ev.ForceSendFields = append(ev.ForceSendFields, "Attendees")
	}
	return ev
}

// patchGoogleEvent sends a patch to an event on the primary calendar.
func patchGoogleEvent(ctx context.Context, srv *calendar.Service, googleEventId string, patch *calendar.Event) error {
	_, err := srv.Events.Patch("primary", googleEventId, patch).Context(ctx).Do()
	return err
}

func DeleteGoogleCalendarEvent(ctx context.Context, googleEventId string, userInfo *model.UserInfo) error {
	srv, err := googleServiceFor(ctx, userInfo.UserPostgresInfo.Id)
	if err != nil {
		return err
	}
	if srv == nil {
		return errGoogleNotLinked
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
	srv, err := googleServiceFor(ctx, creatorUserInfo.UserPostgresInfo.Id)
	if err != nil || srv == nil {
		return err // nil when the creator hasn't linked Google: nothing to do
	}

	// Fetch the existing event to get the current attendee list
	existingEvent, err := srv.Events.Get("primary", googleEventId).Do()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAttendeeFromGoogleCalendarEvent failed to get event %s err: %+v", googleEventId, err)
		return err
	}

	// Filter out the leaving user's email (case-insensitive). Addresses are
	// never logged: this once wrote every guest's to a file in /tmp.
	var updatedAttendees []*calendar.EventAttendee
	for _, a := range existingEvent.Attendees {
		if !strings.EqualFold(a.Email, leavingUserEmail) {
			updatedAttendees = append(updatedAttendees, a)
		}
	}

	// Patch with updated attendee list.
	// ForceSendFields ensures we send the empty array if updatedAttendees is empty,
	// bypassing the `omitempty` JSON tag which would otherwise ignore the change.
	patch := &calendar.Event{
		Attendees:       updatedAttendees,
		ForceSendFields: []string{"Attendees"},
	}
	// ForceSendUpdates sends cancellation notifications to the removed attendee
	_, err = srv.Events.Patch("primary", googleEventId, patch).SendUpdates("all").Do()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RemoveAttendeeFromGoogleCalendarEvent failed to patch event %s err: %+v", googleEventId, err)
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

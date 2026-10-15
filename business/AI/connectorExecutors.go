package business

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// connectorExecutors.go — AI tool executors for per-user connectors (Gmail,
// GitHub) and the person's calendar. Each executor resolves the connector strictly from
// the REQUESTING user's UUID, so the AI can only ever read/act on the caller's
// own external accounts. When the user hasn't connected the relevant account,
// the executor returns a friendly, actionable message instead of an error.
//
// Read tools run on demand. Write tools (gmail_send, calendar_create_event,
// github_comment) are gated by the existing ProposedAction confirmation UI —
// the model proposes them, the user confirms, and only then does ExecuteAction
// invoke these.

// RegisterConnectorExecutors wires the connector tools. Called from
// RegisterToolExecutors so they're registered alongside the core tools.
func RegisterConnectorExecutors() {
	ai.RegisterExecutor("gmail_search", executeGmailSearch)
	ai.RegisterExecutor("gmail_send", executeGmailSend)
	ai.RegisterExecutor("calendar_list_events", executeCalendarList)
	ai.RegisterExecutor("calendar_create_event", executeCalendarCreate)
	ai.RegisterExecutor("github_list_prs", executeGitHubListPRs)
	ai.RegisterExecutor("github_list_issues", executeGitHubListIssues)
	ai.RegisterExecutor("github_comment", executeGitHubComment)
}

// notConnectedMsg renders a friendly "connect your account" nudge.
func notConnectedMsg(human string) string {
	return fmt.Sprintf("🔌 Your %s account isn't connected yet. Open Settings → Connectors to connect it, then try again.", human)
}

// friendlyAPIError turns a refused Google/GitHub call into a short, actionable
// message the AI can show the user instead of dumping JSON. The sorting is the
// connector package's, shared with the inbox.
func friendlyAPIError(provider string, err error) string {
	switch connectorBusiness.ClassifyAPIError(err) {
	case connectorBusiness.ProblemAPIDisabled:
		return fmt.Sprintf("⚠️ The %s API isn't enabled in your workspace's Google Cloud project yet. Ask your admin to enable it in the Google Cloud Console, then try again.", provider)
	case connectorBusiness.ProblemExpired:
		return fmt.Sprintf("⚠️ Your %s connection has expired. Please reconnect it under Settings → Connectors.", provider)
	case connectorBusiness.ProblemPermissions:
		return fmt.Sprintf("⚠️ Your %s connection doesn't have the required permissions. Try reconnecting it under Settings → Connectors with the requested access.", provider)
	case connectorBusiness.ProblemRateLimited:
		return fmt.Sprintf("⚠️ %s is rate-limiting requests right now. Please wait a moment and try again.", provider)
	case connectorBusiness.ProblemNotFound:
		return fmt.Sprintf("⚠️ Couldn't find that on %s. It may have been moved, or the connection lacks access to it.", provider)
	default:
		return fmt.Sprintf("⚠️ Your %s request couldn't be completed right now. Please try again in a moment, or reconnect under Settings → Connectors.", provider)
	}
}

// isNotConnected reports whether an error is the connector "not connected" sentinel.
func isNotConnected(err error) bool {
	return errors.Is(err, connectorBusiness.ErrNotConnected)
}

func parseUserUUID(userUUID string) (uuid.UUID, error) {
	return uuid.Parse(userUUID)
}

// --- Gmail ---

func executeGmailSearch(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	query := strings.TrimSpace(action.Params["query"])
	if query == "" {
		return "", nil, fmt.Errorf("query is required")
	}
	limit := int64(parseIntDefault(action.Params["limit"], 10))

	emails, err := connectorBusiness.GmailSearch(ctx, uid, query, limit)
	if isNotConnected(err) {
		return notConnectedMsg("Gmail"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("Gmail", err), nil, nil
	}
	if len(emails) == 0 {
		return "No emails matched that search.", nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d email(s):\n", len(emails)))
	for _, e := range emails {
		sb.WriteString(fmt.Sprintf("• *%s* — from %s\n  %s\n", strings.TrimSpace(e.Subject), strings.TrimSpace(e.From), strings.TrimSpace(e.Snippet)))
	}
	return sb.String(), nil, nil
}

func executeGmailSend(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	to := strings.TrimSpace(action.Params["to"])
	subject := action.Params["subject"]
	body := action.Params["body"]
	if to == "" {
		return "", nil, fmt.Errorf("recipient is required")
	}

	id, err := connectorBusiness.GmailSend(ctx, uid, to, subject, body)
	if isNotConnected(err) {
		return notConnectedMsg("Gmail"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("Gmail", err), nil, nil
	}
	return fmt.Sprintf("✅ Email sent to %s", to), map[string]string{"tool": "gmail_send", "message_id": id}, nil
}

// --- Calendar ---
//
// The calendar tools read and write the person's OneCamp calendar, which
// carries their Google Calendar when linked (calendarDay.go). They need no
// connector.

// calendarListMax bounds the events one answer lists.
const calendarListMax = 50

// executeCalendarList lists the person's calendar from the start of their
// today, for days days: OneCamp events they made or were invited to, and their
// Google Calendar when it is linked.
func executeCalendarList(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	days := parseIntDefault(action.Params["days"], 7)
	if days < 1 {
		days = 1
	}
	if days > 31 {
		days = 31
	}
	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}
	loc, _ := readerZone(ctx)
	from := helpers.DayStart(time.Now(), loc)
	to := from.AddDate(0, 0, days)
	events, err := calendarEventsOf(ctx, userInfo, &from, &to)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read your calendar: %w", err)
	}
	var listed []*dgraphStruct.DgraphEvent
	for _, e := range events {
		if e != nil && e.StartTime != nil && e.EndTime != nil {
			listed = append(listed, e)
		}
	}
	if len(listed) == 0 {
		return fmt.Sprintf("Your calendar has nothing in the next %d day(s).", days), nil, nil
	}
	sortEvents(listed)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Your calendar for the next %d day(s), times in %s:\n", days, loc.String()))
	for i, e := range listed {
		if i == calendarListMax {
			sb.WriteString(fmt.Sprintf("…and %d more.\n", len(listed)-calendarListMax))
			break
		}
		var tags []string
		if strings.HasPrefix(e.Uuid, "gcal-") {
			tags = append(tags, "Google Calendar")
		}
		if e.IsFocus != nil && *e.IsFocus {
			tags = append(tags, "focus time")
		}
		if e.IsAway != nil && *e.IsAway {
			tags = append(tags, "time off")
		}
		if n := len(e.Participants); n > 0 && !strings.HasPrefix(e.Uuid, "gcal-") {
			tags = append(tags, fmt.Sprintf("%d guest(s)", n))
		}
		line := fmt.Sprintf("• %s · %s", eventSpan(e, loc), eventTitle(e))
		if len(tags) > 0 {
			line += " (" + strings.Join(tags, ", ") + ")"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String(), nil, nil
}

// executeCalendarCreate puts an event on the person's OneCamp calendar, the
// calendar's own way (business/Calendar.CreateEvent), with the teammates it
// names as guests. It goes to their Google Calendar too when that is linked.
func executeCalendarCreate(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	title := strings.TrimSpace(action.Params["title"])
	start := strings.TrimSpace(action.Params["start_time"])
	end := strings.TrimSpace(action.Params["end_time"])
	desc := strings.TrimSpace(action.Params["description"])
	if title == "" || start == "" {
		return "", nil, fmt.Errorf("title and start_time are required")
	}
	// Validate datetimes up front so a malformed value the model produced
	// fails clearly here.
	st, perr := time.Parse(time.RFC3339, start)
	if perr != nil {
		return "", nil, fmt.Errorf("invalid start_time, expected RFC3339 (e.g. 2026-03-25T18:00:00Z)")
	}
	et := st.Add(30 * time.Minute)
	if end != "" {
		if et, perr = time.Parse(time.RFC3339, end); perr != nil {
			return "", nil, fmt.Errorf("invalid end_time, expected RFC3339 (e.g. 2026-03-25T19:00:00Z)")
		}
	}
	if !et.After(st) {
		return "", nil, fmt.Errorf("end_time must be after start_time")
	}

	userInfo, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}
	var guestIDs, guestNames []string
	seen := map[string]bool{userUUID: true}
	for _, ref := range splitPeople(action.Params["attendees"]) {
		if isAskerRef(ref) {
			continue
		}
		u, rerr := resolveAssigneeRef(ctx, userUUID, ref, true) // an invitation: exactly theirs
		if rerr != nil || u == nil {
			return "", nil, fmt.Errorf("couldn't tell which teammate %q is, so nothing was made. Name them by their @handle, then try again", ref)
		}
		if seen[u.Uuid] {
			continue
		}
		seen[u.Uuid] = true
		guestIDs = append(guestIDs, u.Uuid)
		guestNames = append(guestNames, helpers.PersonDisplayName(u.UserName, u.UserFullName, ""))
	}

	out, err := createCalendarEvent(ctx, userInfo, calendarAdapter.CreateOrUpdateEventInput{
		Title:                title,
		Description:          desc,
		StartTime:            st.Format(time.RFC3339),
		EndTime:              et.Format(time.RFC3339),
		Participants:         guestIDs,
		SyncToGoogleCalendar: true,
	})
	if err != nil || out == nil {
		return "", nil, fmt.Errorf("failed to create the event: %w", err)
	}

	loc, _ := readerZone(ctx)
	msg := fmt.Sprintf("✅ \"%s\" is on your calendar, %s", title, eventSpan(&dgraphStruct.DgraphEvent{StartTime: &st, EndTime: &et}, loc))
	if len(guestNames) > 0 {
		msg += ", with " + joinNames(guestNames)
	}
	return msg + ".", map[string]string{"tool": "calendar_create_event", "event_uuid": out.EventUuid}, nil
}

// isAskerRef is a guest named as the asker themself ("me").
func isAskerRef(ref string) bool {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case "me", "myself", "i", "you":
		return true
	}
	return false
}

// splitPeople reads a list of people the model gave: names, @handles or user
// ids, separated by commas, semicolons or "and". Pure.
func splitPeople(s string) []string {
	s = strings.NewReplacer(";", ",", " and ", ",", "\n", ",").Replace(s)
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// joinNames lists names as a sentence does: "Maya", "Maya and Jonas", "Maya,
// Jonas and Ana". Pure.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// --- GitHub ---

func executeGitHubListPRs(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	limit := parseIntDefault(action.Params["limit"], 10)

	items, err := connectorBusiness.GitHubListMyPRs(ctx, uid, limit)
	if isNotConnected(err) {
		return notConnectedMsg("GitHub"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("GitHub", err), nil, nil
	}
	if len(items) == 0 {
		return "You have no open pull requests right now.", nil, nil
	}
	return renderGitHubItems("Open pull requests involving you", items), nil, nil
}

func executeGitHubListIssues(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	limit := parseIntDefault(action.Params["limit"], 10)

	items, err := connectorBusiness.GitHubListMyIssues(ctx, uid, limit)
	if isNotConnected(err) {
		return notConnectedMsg("GitHub"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("GitHub", err), nil, nil
	}
	if len(items) == 0 {
		return "You have no open issues assigned to you.", nil, nil
	}
	return renderGitHubItems("Open issues assigned to you", items), nil, nil
}

func executeGitHubComment(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	owner := strings.TrimSpace(action.Params["owner"])
	repo := strings.TrimSpace(action.Params["repo"])
	number := parseIntDefault(action.Params["number"], 0)
	body := action.Params["body"]
	if owner == "" || repo == "" || number <= 0 || strings.TrimSpace(body) == "" {
		return "", nil, fmt.Errorf("owner, repo, number and body are required")
	}

	link, err := connectorBusiness.GitHubComment(ctx, uid, owner, repo, number, body)
	if isNotConnected(err) {
		return notConnectedMsg("GitHub"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("GitHub", err), nil, nil
	}
	return fmt.Sprintf("✅ Comment posted on %s/%s#%d", owner, repo, number),
		map[string]string{"tool": "github_comment", "link": link}, nil
}

// --- helpers ---

func renderGitHubItems(heading string, items []connectorBusiness.GitHubItem) string {
	var sb strings.Builder
	sb.WriteString(heading + ":\n")
	for _, it := range items {
		sb.WriteString(fmt.Sprintf("• %s#%d — %s\n", strings.TrimPrefix(it.Repo, "/"), it.Number, strings.TrimSpace(it.Title)))
	}
	return sb.String()
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

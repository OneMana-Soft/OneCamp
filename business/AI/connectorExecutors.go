package business

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
)

// connectorExecutors.go — AI tool executors for per-user connectors (Gmail,
// Google Calendar, GitHub). Each executor resolves the connector strictly from
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

// friendlyAPIError interprets a raw Google/GitHub API error into a short,
// actionable message the AI can show the user instead of dumping JSON. It
// prefers the typed googleapi.Error (robust) and falls back to string matching
// for other providers/transports.
func friendlyAPIError(provider string, err error) string {
	// Typed Google API error — classify by HTTP status + reason, which is far
	// more robust than substring matching on the message body.
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case 403:
			for _, e := range gerr.Errors {
				if e.Reason == "accessNotConfigured" || strings.Contains(e.Message, "has not been used in project") {
					return fmt.Sprintf("⚠️ The %s API isn't enabled in your workspace's Google Cloud project yet. Ask your admin to enable it in the Google Cloud Console, then try again.", provider)
				}
			}
			if strings.Contains(gerr.Message, "has not been used in project") || strings.Contains(gerr.Message, "SERVICE_DISABLED") {
				return fmt.Sprintf("⚠️ The %s API isn't enabled in your workspace's Google Cloud project yet. Ask your admin to enable it in the Google Cloud Console, then try again.", provider)
			}
			return fmt.Sprintf("⚠️ Your %s connection doesn't have the required permissions. Try reconnecting it under Settings → Connectors with the requested access.", provider)
		case 401:
			return fmt.Sprintf("⚠️ Your %s connection has expired. Please reconnect it under Settings → Connectors.", provider)
		case 429:
			return fmt.Sprintf("⚠️ %s is rate-limiting requests right now. Please wait a moment and try again.", provider)
		case 404:
			return fmt.Sprintf("⚠️ Couldn't find that on %s. It may have been moved or the connection lacks access to it.", provider)
		}
	}

	msg := err.Error()
	// String fallbacks for non-typed errors (GitHub, OAuth refresh, transport).
	switch {
	case strings.Contains(msg, "SERVICE_DISABLED") || strings.Contains(msg, "has not been used in project"):
		return fmt.Sprintf("⚠️ The %s API isn't enabled in your workspace's Google Cloud project yet. Ask your admin to enable it in the Google Cloud Console, then try again.", provider)
	case strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "expired or revoked"):
		return fmt.Sprintf("⚠️ Your %s connection has expired. Please reconnect it under Settings → Connectors.", provider)
	case strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized"):
		return fmt.Sprintf("⚠️ Your %s connection has expired. Please reconnect it under Settings → Connectors.", provider)
	case strings.Contains(msg, "403") || strings.Contains(msg, "Forbidden") || strings.Contains(msg, "insufficient"):
		return fmt.Sprintf("⚠️ Your %s connection doesn't have the required permissions. Try reconnecting it under Settings → Connectors.", provider)
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit"):
		return fmt.Sprintf("⚠️ %s is rate-limiting requests right now. Please wait a moment and try again.", provider)
	case strings.Contains(msg, "404") || strings.Contains(msg, "Not Found"):
		return fmt.Sprintf("⚠️ Couldn't find that on %s. It may be private or the connection lacks access.", provider)
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

func executeCalendarList(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	days := parseIntDefault(action.Params["days"], 7)

	events, err := connectorBusiness.CalendarList(ctx, uid, days, 20)
	if isNotConnected(err) {
		return notConnectedMsg("Google Calendar"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("Google Calendar", err), nil, nil
	}
	if len(events) == 0 {
		return fmt.Sprintf("You have no events in the next %d day(s).", days), nil, nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Your upcoming events (next %d day(s)):\n", days))
	for _, e := range events {
		sb.WriteString(fmt.Sprintf("• %s — %s\n", strings.TrimSpace(e.Title), strings.TrimSpace(e.Start)))
	}
	return sb.String(), nil, nil
}

func executeCalendarCreate(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	uid, err := parseUserUUID(userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid user")
	}
	title := strings.TrimSpace(action.Params["title"])
	start := action.Params["start_time"]
	end := action.Params["end_time"]
	desc := action.Params["description"]
	if title == "" || start == "" {
		return "", nil, fmt.Errorf("title and start_time are required")
	}
	// Validate datetimes up front (mirrors set_reminder) so a malformed value
	// the model produced fails clearly here instead of deep in the Google API.
	if _, perr := time.Parse(time.RFC3339, strings.TrimSpace(start)); perr != nil {
		return "", nil, fmt.Errorf("invalid start_time, expected RFC3339 (e.g. 2026-03-25T18:00:00Z)")
	}
	if s := strings.TrimSpace(end); s != "" {
		if _, perr := time.Parse(time.RFC3339, s); perr != nil {
			return "", nil, fmt.Errorf("invalid end_time, expected RFC3339 (e.g. 2026-03-25T19:00:00Z)")
		}
	}

	link, err := connectorBusiness.CalendarCreate(ctx, uid, title, start, end, desc)
	if isNotConnected(err) {
		return notConnectedMsg("Google Calendar"), nil, nil
	}
	if err != nil {
		return friendlyAPIError("Google Calendar", err), nil, nil
	}
	return fmt.Sprintf("✅ Event \"%s\" created.", title), map[string]string{"tool": "calendar_create_event", "link": link}, nil
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

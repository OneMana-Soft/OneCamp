package business

import (
	"context"
	"errors"
	"fmt"
	"strings"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
)

// init registers every built-in command handler. The catalog rows for these
// are seeded by SeedBuiltinCommands (called at startup) so the typeahead can
// surface them; this map is what actually executes them.
func init() {
	// Communication
	Register("dm", handleDM)
	Register("msg", handleMsg)
	Register("shrug", handleShrug)
	Register("me", handleMe)

	// Status & availability
	Register("away", handleAway)
	Register("active", handleActive)
	Register("dnd", handleDnd)
	Register("status", handleStatus)

	// Channel management (navigation handled client-side; membership server-side
	// is delegated to existing endpoints the FE already calls)
	Register("search", handleSearch)
	Register("shortcuts", handleShortcuts)
	Register("apps", handleApps)

	// Display
	Register("collapse", handleCollapse)
	Register("expand", handleExpand)

	// Reminders (deferred — see reminder.go)
	Register("remind", handleRemind)

	// Feedback
	Register("feedback", handleFeedback)

	// Polls (interactive — see poll.go)
	Register("poll", handlePoll)
	RegisterInteraction("poll", handlePollInteract)
}

// clientAction builds an ephemeral response carrying a FE directive.
func clientAction(actionType string, payload map[string]string) *commandAdapter.CommandResponse {
	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Ephemeral:    true,
		ClientAction: &commandAdapter.ClientAction{Type: actionType, Payload: payload},
	}
}

// --- Communication ---

// handleDM opens a DM with a user and optionally prefills/sends a message.
// Resolution of @name → user happens client-side (the FE has the mention
// directory cached), so we return a client action with the raw target.
func handleDM(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	args := splitArgs(cc.Text)
	if len(args) == 0 {
		return errorResponse("Usage: `/dm @person [message]`"), nil
	}
	target := strings.TrimPrefix(args[0], "@")
	message := strings.TrimSpace(strings.TrimPrefix(cc.Text, args[0]))
	return clientAction("open_dm", map[string]string{
		"target":  target,
		"message": message,
	}), nil
}

// handleMsg posts/opens a message to another channel.
func handleMsg(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	args := splitArgs(cc.Text)
	if len(args) == 0 {
		return errorResponse("Usage: `/msg #channel [message]`"), nil
	}
	channel := strings.TrimPrefix(args[0], "#")
	message := strings.TrimSpace(strings.TrimPrefix(cc.Text, args[0]))
	return clientAction("open_channel", map[string]string{
		"target":  channel,
		"message": message,
	}), nil
}

// handleShrug appends the classic shrug to the message and submits it as a
// normal channel/DM message via the composer (client posts it).
func handleShrug(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	text := strings.TrimSpace(cc.Text)
	body := strings.TrimSpace(text + ` ¯\_(ツ)_/¯`)
	return clientAction("post_message", map[string]string{"text": body}), nil
}

// handleMe italicizes the text and posts it (Slack /me semantics).
func handleMe(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	text := strings.TrimSpace(cc.Text)
	if text == "" {
		return errorResponse("Usage: `/me does something`"), nil
	}
	return clientAction("post_message", map[string]string{"html": "<p><em>" + escapeHTML(text) + "</em></p>"}), nil
}

// --- Status & availability ---

func handleAway(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("set_presence", map[string]string{"presence": "away"}), nil
}

func handleActive(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("set_presence", map[string]string{"presence": "active"}), nil
}

// handleDnd enables Do Not Disturb for a duration (e.g. "/dnd 30 minutes").
// "/dnd off" (or "/dnd 0") turns it off; a bare "/dnd" defaults to 1 hour.
// The FE applies the local mute window immediately and persists quiet hours.
func handleDnd(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	dur := strings.TrimSpace(cc.Text)
	low := strings.ToLower(dur)

	// Explicit off.
	if low == "off" || low == "0" || low == "clear" || low == "end" {
		if _, err := prefDomain.SetPause(ctx, cc.User.UserPostgresInfo.Id, nil); err != nil {
			return errorResponse("Couldn't resume your notifications. Try again."), nil
		}
		return clientAction("set_dnd", map[string]string{}), nil
	}

	// Bare /dnd → default to 1 hour (so it always turns DND ON, matching the
	// command's intent; use "/dnd off" to disable).
	if dur == "" {
		dur = "1 hour"
	}

	// Parse to an absolute end time so the FE doesn't re-implement parsing.
	when, err := ParseWhen("in "+dur, cc.Timezone)
	if err != nil {
		return errorResponse("Couldn't read that duration. Try `/dnd 30 minutes`, or `/dnd off`."), nil
	}
	// The server holds the pause, so phones and other tabs go quiet too; the
	// client action updates this tab at once.
	until := when.At
	if _, err := prefDomain.SetPause(ctx, cc.User.UserPostgresInfo.Id, &until); err != nil {
		var pe *prefDomain.PauseError
		if errors.As(err, &pe) {
			return errorResponse("%s", pe.Error()), nil
		}
		return errorResponse("Couldn't pause your notifications. Try again."), nil
	}
	return clientAction("set_dnd", map[string]string{
		"until":   when.At.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"display": when.Display,
	}), nil
}

// handleStatus sets a custom emoji status. "/status :rocket: shipping".
func handleStatus(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	text := strings.TrimSpace(cc.Text)
	if text == "" {
		return clientAction("open_status_picker", nil), nil
	}
	emoji := ""
	desc := text
	if strings.HasPrefix(text, ":") {
		if end := strings.Index(text[1:], ":"); end != -1 {
			emoji = text[:end+2]
			desc = strings.TrimSpace(text[end+2:])
		}
	}
	return clientAction("set_status", map[string]string{"emoji": emoji, "text": desc}), nil
}

// --- Search & navigation ---

func handleSearch(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	q := strings.TrimSpace(cc.Text)
	if q == "" {
		return errorResponse("Usage: `/search your query`"), nil
	}
	return clientAction("open_search", map[string]string{"query": q}), nil
}

func handleShortcuts(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("open_shortcuts", nil), nil
}

func handleApps(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("open_apps", map[string]string{"query": strings.TrimSpace(cc.Text)}), nil
}

// --- Display ---

func handleCollapse(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("toggle_media", map[string]string{"collapsed": "true"}), nil
}

func handleExpand(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	return clientAction("toggle_media", map[string]string{"collapsed": "false"}), nil
}

// --- Feedback ---

func handleFeedback(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	msg := strings.TrimSpace(cc.Text)
	if msg == "" {
		return errorResponse("Usage: `/feedback your message`"), nil
	}
	// Persist feedback by DM-ing the workspace admins is future work; for now
	// we record it as an ephemeral acknowledgement and let the FE optionally
	// route it. Kept intentionally minimal and non-destructive.
	return ephemeral(fmt.Sprintf("Thanks for the feedback! We've noted: “%s”", truncate(msg, 140))), nil
}

// escapeHTML is a minimal escaper for /me text inserted as HTML.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

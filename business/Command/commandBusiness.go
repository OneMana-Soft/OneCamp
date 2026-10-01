// Package business (Command) is the user-facing slash command framework.
//
// Design
// ------
// Everything is a Command with a typed contract. The difference between
// /shrug, /remind and /giphy is configuration + which handler runs, not a
// special case. A command resolves to one of four execution modes:
//
//   - inline      : handler runs synchronously, returns text/blocks now.
//   - interactive : handler returns an ephemeral Block Kit card; button
//     clicks round-trip through HandleInteract.
//   - deferred    : handler enqueues a scheduled_jobs row (/remind).
//   - external    : the dispatcher forwards to an installed app's handler_url
//     (Slack-compatible, SSRF-guarded, HMAC-signed).
//
// Built-in handlers are registered in builtins.go via Register(). They
// deliberately reuse the existing business layer (channel/user/AI executors)
// rather than duplicating logic.
package business

import (
	"context"
	"fmt"
	"strings"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// appCtx is the application-level context for background goroutines (deferred
// delivery). Set at startup via SetAppContext.
var appCtx = context.Background()

// SetAppContext wires the long-lived application context used by async paths.
func SetAppContext(ctx context.Context) {
	if ctx != nil {
		appCtx = ctx
	}
}

// CommandContext carries everything a handler needs about an invocation.
type CommandContext struct {
	User      userModels.UserInfo
	Command   string // canonical name without leading slash, lowercased
	Text      string // trimmed args
	ChannelID *uuid.UUID
	DmGroupID *string
	ThreadTs  *string
	Timezone  string
	TriggerID string
}

// Handler executes a built-in command and returns a response.
type Handler func(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error)

// InteractionHandler resumes an interactive card after a button/select click.
type InteractionHandler func(ctx context.Context, cc CommandContext, ir commandAdapter.InteractRequest) (*commandAdapter.CommandResponse, error)

// commandRegistry holds built-in command handlers keyed by canonical command name.
var commandRegistry = map[string]Handler{}

// interactionRegistry holds interaction handlers keyed by command name.
var interactionRegistry = map[string]InteractionHandler{}

// Register wires a built-in command handler. Called from builtins.go init().
func Register(command string, h Handler) {
	commandRegistry[normalizeCommand(command)] = h
}

// RegisterInteraction wires an interaction handler for a command.
func RegisterInteraction(command string, h InteractionHandler) {
	interactionRegistry[normalizeCommand(command)] = h
}

// AppTester runs a live credential/connectivity probe for a built-in app and
// returns a structured result. Built-in apps self-register one via
// RegisterAppTest so testApp.go stays generic (no per-slug switch).
type AppTester func(ctx context.Context, appID uuid.UUID) *commandAdapter.AppTestResult

// appTestRegistry holds per-app test probes keyed by app slug.
var appTestRegistry = map[string]AppTester{}

// RegisterAppTest wires a built-in app's "Test" probe. Called from the app's
// init() (e.g. giphy.go, ai.go).
func RegisterAppTest(slug string, t AppTester) {
	appTestRegistry[strings.ToLower(strings.TrimSpace(slug))] = t
}

// normalizeCommand strips a leading slash and lowercases.
func normalizeCommand(c string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c), "/"))
}

// ephemeral is a tiny helper for the common "only the invoker sees this" reply.
func ephemeral(text string) *commandAdapter.CommandResponse {
	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Text:         text,
		Ephemeral:    true,
	}
}

// errorResponse renders a consistent ephemeral error card.
func errorResponse(format string, args ...interface{}) *commandAdapter.CommandResponse {
	return ephemeral("⚠️ " + fmt.Sprintf(format, args...))
}

// splitArgs tokenizes the command text respecting double-quoted groups so
// `/poll "Question?" "A" "B"` parses correctly.
func splitArgs(text string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range text {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// logCtx is a small helper to keep error logs consistent.
func logErr(ctx context.Context, where string, err error) {
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Command/"+where+" err: %+v", err)
	}
}

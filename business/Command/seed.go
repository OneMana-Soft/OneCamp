package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
)

// builtinSpec describes a built-in command for catalog seeding.
type builtinSpec struct {
	command     string
	description string
	usageHint   string
	execMode    string
	response    string // ephemeral | in_channel
}

// builtinCatalog is the source of truth for the org-scoped built-in commands
// surfaced in the composer typeahead. Handlers are wired in builtins.go;
// reminder/poll have their own files. Keep this list in sync with Register().
var builtinCatalog = []builtinSpec{
	{"remind", "Set a reminder for yourself, a person, or a channel", "[me|@who|#channel] <what> <when>", slashModel.ExecDeferred, "ephemeral"},
	{"dm", "Send a direct message from anywhere", "@person [message]", slashModel.ExecInline, "ephemeral"},
	{"msg", "Send a message to another channel", "#channel [message]", slashModel.ExecInline, "ephemeral"},
	{"shrug", `Append ¯\_(ツ)_/¯ to your message`, "[message]", slashModel.ExecInline, "ephemeral"},
	{"me", "Display an action in italics", "<text>", slashModel.ExecInline, "ephemeral"},
	{"status", "Set a custom status with emoji", "[:emoji:] [text]", slashModel.ExecInline, "ephemeral"},
	{"away", "Mark yourself as away", "", slashModel.ExecInline, "ephemeral"},
	{"active", "Mark yourself as active", "", slashModel.ExecInline, "ephemeral"},
	{"dnd", "Pause notifications (Do Not Disturb)", "[duration | off]", slashModel.ExecInline, "ephemeral"},
	{"search", "Search messages in the workspace", "<query>", slashModel.ExecInline, "ephemeral"},
	{"shortcuts", "View keyboard shortcuts", "", slashModel.ExecInline, "ephemeral"},
	{"apps", "Browse and install apps", "[name]", slashModel.ExecInline, "ephemeral"},
	{"collapse", "Collapse inline images and link previews", "", slashModel.ExecInline, "ephemeral"},
	{"expand", "Expand collapsed images and link previews", "", slashModel.ExecInline, "ephemeral"},
	{"poll", "Create a quick poll", `"Question?" "Option 1" "Option 2"`, slashModel.ExecInteractive, "in_channel"},
	{"giphy", "Search and send a GIF", "<search term>", slashModel.ExecInteractive, "ephemeral"},
	{"ask", "Ask OneCamp AI a quick question", "<your question>", slashModel.ExecInline, "ephemeral"},
	{"feedback", "Send feedback to the workspace admins", "<message>", slashModel.ExecInline, "ephemeral"},
}

// SeedBuiltinCommands upserts the org-scoped built-in command catalog at
// startup. Idempotent: re-running updates descriptions/usage without
// duplicating rows (the unique index on command+scope handles conflicts).
func SeedBuiltinCommands(ctx context.Context) {
	for _, b := range builtinCatalog {
		usage := b.usageHint
		cmd := &slashModel.SlashCommand{
			Command:      b.command,
			Description:  b.description,
			ExecMode:     b.execMode,
			ScopeType:    "org",
			ResponseType: b.response,
			IsBuiltin:    true,
			IsEnabled:    true,
		}
		if usage != "" {
			cmd.UsageHint = &usage
		}
		if err := slashModel.UpsertCommand(ctx, cmd); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Command/SeedBuiltinCommands upsert %s err: %+v", b.command, err)
		}
	}
	helpers.MessageLogs.InfoLog.Printf("Seeded %d built-in slash commands", len(builtinCatalog))
}

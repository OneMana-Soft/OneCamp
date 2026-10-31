package business

// The Policy Registry: typed workspace policies, one middleware that enforces
// them, and every change recorded (docs programme, PROGRAM.md §4 #15).
//
// A policy's value lives in system_configs under the policy's key; a policy
// nobody set answers its default, and each default is what the server did before
// policies existed: members may export and publish, guest links carry no cap of
// their own, and no embed list is configured. Admins read and set them
// (Admin.List, Admin.Set; the /admin/policies routes), and every change is a row
// in the audit log, or it doesn't happen.
//
// A route behind a policy:
//
//	r.With(policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)).Post("/export", ...)
//
// Values are read through a short cache, so a change reaches every instance
// within cacheFor; a read that fails answers the default, today's behaviour.

import (
	"context"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// BoolPolicy is an on/off policy and its default.
type BoolPolicy struct {
	Key     string
	Default bool
}

// IntPolicy is a numeric policy and its default.
type IntPolicy struct {
	Key     string
	Default int
}

// ListPolicy is a list policy and its default.
type ListPolicy struct {
	Key     string
	Default []string
}

// ChoicePolicy is a policy that takes one of a few named values ("members",
// "assigned", "admins"), and its default, which is one of them.
type ChoicePolicy struct {
	Key     string
	Default string
	Choices []string
}

// Allows is whether v is one of the policy's choices. Pure.
func (p ChoicePolicy) Allows(v string) bool {
	for _, c := range p.Choices {
		if c == v {
			return true
		}
	}
	return false
}

// Scope is one thing a policy can be set for beside the workspace: a project,
// by its id. A value set for a scope beats the workspace's there, and the
// workspace's beats the default; only a policy whose spec names the scope's
// kind can be set for one (scoped.go).
type Scope struct {
	Kind string
	ID   string
}

// ScopeProject is a project's own value.
const ScopeProject = "project"

// ScopeChannel is a channel's own value (chat P3: "Allow @channel and @here").
const ScopeChannel = "channel"

var (
	// ChatLinesJoinsInLarge shows joins and leaves as lines in a public
	// channel of more than 20 people too (chat B28); private channels and
	// smaller ones always show them.
	ChatLinesJoinsInLarge = BoolPolicy{Key: "chat.lines.joins_large", Default: false}
	// DocsExportMembers lets members export docs; admins always may.
	DocsExportMembers = BoolPolicy{Key: "docs.export.members", Default: true}
	// GuestLinkMaxDays caps how long a guest link lasts; 0 sets no cap.
	GuestLinkMaxDays = IntPolicy{Key: "guest.link.max_days", Default: 0}
	// EmbedAllowlist lists the hosts docs may embed; nil means no list is
	// configured.
	EmbedAllowlist = ListPolicy{Key: "embed.allowlist"}
	// DocsHistoryDays is how long a doc's versions are kept (docs H1). Named
	// and pinned versions are always kept; the server's
	// DOC_SNAPSHOT_RETENTION_DAYS is a floor no setting goes below.
	DocsHistoryDays = IntPolicy{Key: "docs.history.days", Default: 365}

	// Tables (design/tables-vs-airtable/PROGRAM.md §5), declared at its day 0
	// with today's behaviour as the default, and shown to admins (specs) by
	// the package that enforces each. The "who may" policies (tables.create.who,
	// tables.export.who) and tables.share_guests, which follows guest.access,
	// are declared by A5 and B12 as choices (projects' ChoicePolicy).
	//
	// TablesEmbedOutside lets a view be embedded on other sites (B3).
	TablesEmbedOutside = BoolPolicy{Key: "tables.embed_outside", Default: false}
	// TablesScripts lets tables run scripts (B6).
	TablesScripts = BoolPolicy{Key: "tables.scripts", Default: false}
	// TablesAIFields lets tables have AI fields where AI is on (B8).
	TablesAIFields = BoolPolicy{Key: "tables.ai_fields", Default: true}
	// TablesTrashRetentionDays is how long deleted rows, fields and tables stay
	// in the trash; 0 keeps them, as today, until the owner decides (§13 O3).
	TablesTrashRetentionDays = IntPolicy{Key: "tables.trash.retention_days", Default: 0}
	// TablesHistoryRetentionDays is how long a table's change history is kept;
	// 0 keeps it, until the owner decides (§13 O3).
	TablesHistoryRetentionDays = IntPolicy{Key: "tables.history.retention_days", Default: 0}
)

// The projects programme's policies (design/projects-vs-asana-jira/PROGRAM.md
// §5), declared at its day 0 with today's behaviour as each default. A policy
// joins the admin's list (registry.go's specs) when the package that reads it
// lands; until then it answers its default.
var (
	// ProjectsEditWho is who changes a project's tasks: its members (since
	// A1), its members on the tasks assigned to them, or its admins only (the
	// rule before roles). Set for the workspace, and for each project.
	ProjectsEditWho = ChoicePolicy{Key: "projects.edit.who", Default: "members", Choices: []string{"members", "assigned", "admins"}}
	// ProjectsCreateWho is who makes projects in a team: the team's admins
	// (as before) or all its members.
	ProjectsCreateWho = ChoicePolicy{Key: "projects.create.who", Default: "team_admins", Choices: []string{"team_admins", "team_members"}}
	// ProjectsTriage turns a project's triage on (B1).
	ProjectsTriage = BoolPolicy{Key: "projects.triage", Default: false}
	// ProjectsWIPHard makes a board's work-in-progress limits hard (A7).
	ProjectsWIPHard = BoolPolicy{Key: "projects.wip.hard", Default: false}
	// ProjectsAutoCloseDays closes tasks nobody touched for this many days;
	// 0 never does.
	ProjectsAutoCloseDays = IntPolicy{Key: "projects.autoclose.days", Default: 0}
	// ProjectsGuests is whether a project may be shared with guests: as the
	// workspace's guest access says (follow), or on or off for it (B15).
	ProjectsGuests = ChoicePolicy{Key: "projects.guests", Default: "follow", Choices: []string{"follow", "on", "off"}}
	// ProjectsAITriage suggests a triage decision for a new task [AI] (B11).
	ProjectsAITriage = BoolPolicy{Key: "projects.ai.triage", Default: false}
)

// The chat programme's mention policies (design/chat-vs-slack/PROGRAM.md B5,
// chat P3), each default what a workspace expects of Slack.
var (
	// ChatBroadcastMembers lets members notify a whole channel (@channel,
	// @here, @everyone); a channel's managers and the workspace's admins
	// always may. Set for the workspace, and for each channel.
	ChatBroadcastMembers = BoolPolicy{Key: "chat.broadcast.members", Default: true}
	// ChatBroadcastConfirmAt is how many people a broadcast reaches before the
	// composer asks first ("This notifies 24 people in #launch"); 0 never asks.
	ChatBroadcastConfirmAt = IntPolicy{Key: "chat.broadcast.confirm_at", Default: 6}
	// ChatBroadcastAdminsOnlyAt is how many members a channel has beyond which
	// only its managers and the workspace's admins notify all of it; 0 sets no
	// such limit.
	ChatBroadcastAdminsOnlyAt = IntPolicy{Key: "chat.broadcast.admins_only_at", Default: 0}
)

// The calendar programme's policies (design/calendar-vs-gcal/PROGRAM.md A6).
var (
	// CalendarFreeBusy lets members see when each other is busy, and nothing
	// more, on a calendar not shared with them (owner decision O7: on).
	CalendarFreeBusy = BoolPolicy{Key: "calendar.freebusy.members", Default: true}
	// CalendarFeedsAllowed lets members make secret links that show a
	// calendar in another calendar app (calendar B8). Off, every link stops
	// answering at once.
	CalendarFeedsAllowed = BoolPolicy{Key: "calendar.feeds.allowed", Default: true}
)

// Store answers policies.
type Store interface {
	Bool(ctx context.Context, p BoolPolicy) bool
	Int(ctx context.Context, p IntPolicy) int
	List(ctx context.Context, p ListPolicy) []string
	Choice(ctx context.Context, p ChoicePolicy) string
	// BoolIn, IntIn and ChoiceIn answer a policy for one scope: the value set
	// for it, else the workspace's, else the default.
	BoolIn(ctx context.Context, p BoolPolicy, s Scope) bool
	IntIn(ctx context.Context, p IntPolicy, s Scope) int
	ChoiceIn(ctx context.Context, p ChoicePolicy, s Scope) string
	// PrimeScopes reads many scopes' values at once, ahead of asking each.
	PrimeScopes(ctx context.Context, kind string, ids []string)
	// Require refuses a request when p is off for the signed-in member. Admins
	// always pass.
	Require(p BoolPolicy) func(http.Handler) http.Handler
}

// Policies is where every policy is read: the registry.
var Policies Store = Admin

// isAdmin is whether the signed-in member is a workspace admin, as
// VerifyAuth put them on the request.
func isAdmin(r *http.Request) bool {
	u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return ok && u.UserPostgresInfo.IsAdmin
}

package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ToolDef defines a workspace action that the AI can propose.
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  []ToolParam `json:"parameters"`
	// ReadOnly marks tools with no side effects (search/list/summarize).
	// Read-only tools are auto-executed server-side inside the agent loop so
	// their results can be fed back to the model; write tools are never
	// auto-run — they are surfaced to the user for explicit confirmation.
	ReadOnly bool `json:"read_only"`
	// Destructive marks a write whose effect is irreversible/high-risk (delete,
	// drop, overwrite, force-push). Such a tool is never auto-run unattended:
	// the agent runner routes it through human approval even in full-autonomy
	// mode. Currently sourced from an MCP server's destructiveHint annotation,
	// so it is provider-agnostic (any conformant server can declare it).
	Destructive bool `json:"destructive,omitempty"`
	// ExternalEffect marks a write whose effect LEAVES this workspace and cannot
	// be recalled: an email delivered, a calendar invitation sent to attendees, a
	// comment published on a repository other people read. Nothing inside OneCamp
	// can undo it, and calling it twice does not repair it — it does it twice.
	//
	// Deliberately NOT folded into Destructive. Destructive is sourced from MCP's
	// destructiveHint, and under that spec a tool that only adds something (a new
	// email, a new event) is "additive" rather than destructive — so reusing the
	// flag would make OneCamp advertise a value the spec says is wrong. The two
	// properties are different facts that happen to share one consequence: a human
	// must decide before it happens unattended. ToolNeedsHumanBeforeUnattended is
	// where that shared consequence lives, and gates consult THAT, never this.
	ExternalEffect bool `json:"external_effect,omitempty"`
	// DefersResult marks a tool whose success means work has STARTED, not that
	// it is done: it hands off to a background job that posts the real outcome
	// later (code_pr opens its PR minutes afterwards, or reports why it could
	// not). A run whose only write was such a tool has not completed anything
	// yet, and must not say it has.
	DefersResult bool `json:"defers_result,omitempty"`
}

type contextKey string

const (
	LocalizationContextKey contextKey = "ai_localization"
)

// GetLocalization retrieves the localization map from the context.
func GetLocalization(ctx context.Context) map[string]string {
	if val := ctx.Value(LocalizationContextKey); val != nil {
		if m, ok := val.(map[string]string); ok {
			return m
		}
	}
	return nil
}

// ToolParam defines a parameter for a tool.
type ToolParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "string", "boolean"
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// ProposedAction is a parsed tool call from LLM output.
type ProposedAction struct {
	ToolName    string            `json:"tool_name"`
	Params      map[string]string `json:"params"`
	Description string            `json:"description"` // human-readable summary
}

// ToolRegistry holds all available workspace tools.
var ToolRegistry = []ToolDef{
	{
		Name:        "create_task",
		Description: "Create a new task in a project. Use when the user asks to create, add, or make a task/todo/ticket.",
		Parameters: []ToolParam{
			{Name: "task_name", Type: "string", Required: true, Description: "Name/title of the task"},
			{Name: "project_uuid", Type: "string", Required: true, Description: "UUID of the project to create the task in"},
			{Name: "description", Type: "string", Required: false, Description: "Task description/details"},
			{Name: "priority", Type: "string", Required: false, Description: "Priority: low, medium, high, urgent"},
			{Name: "assignee_uuid", Type: "string", Required: false, Description: "UUID of the user to assign the task to"},
		},
	},
	{
		Name:        "update_task_status",
		Description: "Change the status of an existing task. Use when the user asks to move/mark a task as done, in progress, in review, todo, backlog, or canceled, or to one of the project's own statuses (shown by name in task lists, e.g. QA (inReview)).",
		Parameters: []ToolParam{
			{Name: "task_uuid", Type: "string", Required: true, Description: "UUID of the task to update"},
			{Name: "status", Type: "string", Required: true, Description: "New status: one of todo, inProgress, inReview, done, backlog, canceled, or the name of one of the task's project's own statuses"},
		},
	},
	{
		Name:        "assign_task",
		Description: "Assign an existing task to a user (or unassign it). Use when the user asks to assign, reassign, or unassign a task.",
		Parameters: []ToolParam{
			{Name: "task_uuid", Type: "string", Required: true, Description: "UUID of the task to assign"},
			{Name: "assignee_uuid", Type: "string", Required: false, Description: "UUID of the user to assign; omit or leave empty to unassign"},
		},
	},
	{
		Name:        "set_task_due_date",
		Description: "Set or clear the due date of an existing task. Use when the user asks to set, change, or remove a task's deadline/due date.",
		Parameters: []ToolParam{
			{Name: "task_uuid", Type: "string", Required: true, Description: "UUID of the task"},
			{Name: "due_date", Type: "string", Required: false, Description: "Due date in RFC3339 (e.g. 2026-06-20T17:00:00Z); omit or leave empty to clear"},
		},
	},
	{
		Name:        "create_poll",
		Description: "Post a poll in a channel: a question everyone can answer in one click, with live results. Use when a decision needs the team's quick vote (pick a date, choose an option, gauge interest). Posts as the user, so they must be able to post in the channel.",
		Parameters: []ToolParam{
			{Name: "channel_uuid", Type: "string", Required: true, Description: "UUID of the channel to post the poll in"},
			{Name: "question", Type: "string", Required: true, Description: "The question, under 300 characters"},
			{Name: "options", Type: "string", Required: true, Description: "2 to 10 options separated by | (for example: Tuesday | Wednesday | Thursday)"},
			{Name: "multiple", Type: "boolean", Required: false, Description: "true lets people choose more than one option"},
			{Name: "open_hours", Type: "string", Required: false, Description: "Close voting after this many hours (max 720); omit to leave it open"},
		},
	},
	{
		Name:        "read_poll",
		Description: "Read a poll's question, options and current vote counts. Use to report results or check whether a poll is still open. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "poll_uuid", Type: "string", Required: true, Description: "UUID of the poll (the data-id of its block in the message)"},
		},
	},
	{
		Name:        "list_tasks",
		Description: "List the current user's own assigned tasks. Use when the user asks about their tasks/todos/work, what is open or overdue, or to find a specific task (by name) before updating it. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "status", Type: "string", Required: false, Description: "Filter by status: one of todo, inProgress, inReview, done, backlog, canceled (each includes the project statuses that count as it), or the name of a project's own status such as QA"},
			{Name: "filter", Type: "string", Required: false, Description: "Set to 'overdue' to show only overdue tasks that are not done"},
			{Name: "search", Type: "string", Required: false, Description: "Filter to tasks whose name contains this text"},
		},
	},
	{
		Name:        "list_projects",
		Description: "List the projects the user belongs to, including each project's UUID and whether the user can create/manage tasks in it (project admin). Use to resolve a project by name before creating a task, or when the user asks what projects they are in. Read-only.",
		ReadOnly:    true,
		Parameters:  []ToolParam{},
	},
	{
		Name:        "read_project",
		Description: "Read an overview of a single project the user is a member of: status, team, the user's role, and member names. Use when the user asks about a specific project's details, status, or who is on it. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "project_uuid", Type: "string", Required: true, Description: "UUID of the project to read"},
		},
	},
	{
		Name:        "list_project_tasks",
		Description: "List the tasks in a specific project the user is a member of (any task in the project, not just the user's own). Use when the user asks what's in a project, the project's backlog, or to find a task in a project before updating it. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "project_uuid", Type: "string", Required: true, Description: "UUID of the project whose tasks to list"},
			{Name: "status", Type: "string", Required: false, Description: "Filter by status: one of todo, inProgress, inReview, done, backlog, canceled (each includes the project statuses that count as it), or the name of a project's own status such as QA"},
			{Name: "filter", Type: "string", Required: false, Description: "Set to 'overdue' to show only overdue tasks that are not done"},
			{Name: "search", Type: "string", Required: false, Description: "Filter to tasks whose name contains this text"},
		},
	},
	{
		Name:        "list_teams",
		Description: "List the teams the user belongs to, including each team's UUID and whether the user can create projects in it (team admin). Use to resolve a team by name before creating a project, or when the user asks what teams they are in. Read-only.",
		ReadOnly:    true,
		Parameters:  []ToolParam{},
	},
	{
		Name:        "create_project",
		Description: "Create a new project inside a team. Only a TEAM admin can create a project. Use when the user explicitly asks to create/start a new project. If you don't know the team UUID, call list_teams first.",
		Parameters: []ToolParam{
			{Name: "name", Type: "string", Required: true, Description: "Project name"},
			{Name: "team_uuid", Type: "string", Required: true, Description: "UUID of the team the project belongs to (the user must be an admin of this team)"},
		},
	},
	{
		Name:        "create_doc",
		Description: "Create a new document, optionally with a full body. Use when the user asks to create, write, or draft a document/note/page, or to turn a discussion into a doc/PRD/spec/summary. Put the document content in 'body' as Markdown (headings, bullet lists, bold, etc.).",
		Parameters: []ToolParam{
			{Name: "title", Type: "string", Required: true, Description: "Document title"},
			{Name: "body", Type: "string", Required: false, Description: "Document body as Markdown. Supports # headings, - and 1. lists, **bold**, *italic*, `code`, > quotes, and links."},
			{Name: "is_private", Type: "boolean", Required: false, Description: "Whether the document is private (default: false)"},
		},
	},
	{
		Name: "read_meeting_transcript",
		Description: "Read what was said in the most recent call in a channel. Use when the user asks what was discussed, " +
			"decided or agreed in a meeting or call, or wants action items from one. Only works for channels the user can see.",
		ReadOnly: true,
		Parameters: []ToolParam{
			{Name: "channel_uuid", Type: "string", Required: true, Description: "UUID of the channel whose call to read"},
		},
	},
	{
		Name: "find_people",
		Description: "Look up workspace members by name, job title or department. Use when the user asks who someone is, " +
			"how to reach them, or who works on something. Returns each person's name, any title or department that is " +
			"set, their email and their user id, which you can pass to tools that need one.",
		ReadOnly: true,
		Parameters: []ToolParam{
			{Name: "query", Type: "string", Required: true, Description: "Name, job title or department to search for"},
		},
	},
	{
		Name:        "read_doc",
		Description: "Read the text content of a document so you can summarize it, answer questions about it, or draft from it. Use when the user references a specific doc (by UUID). Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "doc_uuid", Type: "string", Required: true, Description: "UUID of the document to read"},
		},
	},
	{
		Name:        "append_to_doc",
		Description: "Add content to the END of an existing document, as a live edit that everyone with the doc open sees at once. Use when asked to add, append or write something into a doc that already exists (steps, notes, a summary, action items). It never changes or removes what is already there. Write the content as Markdown. Find the doc_uuid with search_workspace, or from a doc linked in the task or message; never ask a person for it.",
		Parameters: []ToolParam{
			{Name: "doc_uuid", Type: "string", Required: true, Description: "UUID of the document to add to"},
			{Name: "content", Type: "string", Required: true, Description: "What to add, as Markdown. Supports # headings, - and 1. lists, **bold**, *italic*, `code`, > quotes, and links."},
		},
	},
	{
		Name:        "send_message",
		Description: "Send a message to a CHANNEL (e.g. #general, #private-team). Do NOT use this tool for sending messages to individual people or users.",
		Parameters: []ToolParam{
			{Name: "channel_uuid", Type: "string", Required: true, Description: "UUID of the channel to send the message in"},
			{Name: "text", Type: "string", Required: true, Description: "Message text to send"},
		},
	},
	{
		Name:        "send_dm",
		Description: "Send a direct message (DM) or private message to a specific USER or PERSON (e.g. @John, @cannabisd). Use this instead of send_message when sending to individuals.",
		Parameters: []ToolParam{
			{Name: "to_uuid", Type: "string", Required: true, Description: "UUID of the user to send the DM to"},
			{Name: "text", Type: "string", Required: true, Description: "Message text to send"},
		},
	},
	{
		Name:        "send_group_chat",
		Description: "Send a message in a group chat. Use when the user asks to send a message in a group chat/conversation.",
		Parameters: []ToolParam{
			{Name: "grp_id", Type: "string", Required: true, Description: "Group ID of the group chat"},
			{Name: "text", Type: "string", Required: true, Description: "Message text to send"},
		},
	},
	{
		Name:        "set_reminder",
		Description: "Create a reminder or calendar event. Use when the user asks to remind, set a reminder, schedule something, or create an event.",
		Parameters: []ToolParam{
			{Name: "title", Type: "string", Required: true, Description: "Title of the reminder/event"},
			{Name: "start_time", Type: "string", Required: true, Description: "Start time in RFC3339 format (e.g. 2026-03-25T18:00:00Z)"},
			{Name: "description", Type: "string", Required: false, Description: "Description or notes for the reminder"},
		},
	},
	{
		Name:        "summarize_channel",
		Description: "Summarize recent messages in a CHANNEL. Use when the user asks to summarize, catch up, or recap what happened in a specific channel.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "channel_uuid", Type: "string", Required: true, Description: "UUID of the channel to summarize"},
			{Name: "count", Type: "string", Required: false, Description: "Number of recent messages to summarize (default: 50, max: 200)"},
		},
	},
	{
		Name:        "summarize_dm",
		Description: "Summarize recent DM messages with a specific USER. Use when the user asks to summarize his chat with @user.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "to_user_uuid", Type: "string", Required: true, Description: "UUID of the user the DM is with"},
			{Name: "count", Type: "string", Required: false, Description: "Number of recent messages to summarize (default: 50, max: 200)"},
		},
	},
	{
		Name:        "summarize_group_chat",
		Description: "Summarize recent messages in a specific GROUP CHAT. Use when the user asks to summarize this group.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "grp_id", Type: "string", Required: true, Description: "Group ID of the group chat"},
			{Name: "count", Type: "string", Required: false, Description: "Number of recent messages to summarize (default: 50, max: 200)"},
		},
	},
	// --- Connector tools (per-user external accounts: Gmail, Calendar, GitHub).
	// These only work when the user has connected the relevant account; the
	// executor returns a friendly "connect first" message otherwise. Write
	// tools (gmail_send, calendar_create_event, github_comment) always run
	// behind the user-confirmation gate. ---
	{
		Name:        "gmail_search",
		Description: "Search the user's Gmail and return matching emails. Use when the user asks about their email/inbox (e.g. 'any emails from Sarah?', 'unread emails about the contract'). Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "query", Type: "string", Required: true, Description: "Gmail search query, e.g. 'from:sarah is:unread' or 'subject:invoice newer_than:7d'"},
			{Name: "limit", Type: "string", Required: false, Description: "Max results (default 10, max 25)"},
		},
	},
	{
		Name:        "gmail_send",
		Description: "Send an email on the user's behalf via Gmail. Use ONLY when the user explicitly asks to send/email someone. Requires confirmation.",
		// "Requires confirmation" above is a sentence in a prompt, and a prompt
		// cannot enforce anything. This flag is what makes it true.
		ExternalEffect: true,
		Parameters: []ToolParam{
			{Name: "to", Type: "string", Required: true, Description: "Recipient email address"},
			{Name: "subject", Type: "string", Required: true, Description: "Email subject"},
			{Name: "body", Type: "string", Required: true, Description: "Email body (plain text)"},
		},
	},
	{
		Name:        "calendar_list_events",
		Description: "List the user's upcoming Google Calendar events. Use when the user asks about their schedule/calendar/meetings ('what's on my calendar', 'am I free tomorrow'). Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "days", Type: "string", Required: false, Description: "How many days ahead to look (default 7)"},
		},
	},
	{
		Name:        "calendar_create_event",
		Description: "Create an event on the user's Google Calendar. Use when the user asks to schedule/add a meeting or event. Requires confirmation.",
		// Invitations reach attendees the moment it succeeds; deleting the event
		// afterwards does not unsend them.
		ExternalEffect: true,
		Parameters: []ToolParam{
			{Name: "title", Type: "string", Required: true, Description: "Event title"},
			{Name: "start_time", Type: "string", Required: true, Description: "Start time in RFC3339 (e.g. 2026-06-10T15:00:00Z)"},
			{Name: "end_time", Type: "string", Required: false, Description: "End time in RFC3339 (defaults to 30 min after start)"},
			{Name: "description", Type: "string", Required: false, Description: "Event description"},
		},
	},
	{
		Name:        "github_list_prs",
		Description: "List open pull requests that involve the user (authored or review-requested). Use when the user asks about their PRs/pull requests/code reviews. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "limit", Type: "string", Required: false, Description: "Max results (default 10, max 25)"},
		},
	},
	{
		Name:        "github_list_issues",
		Description: "List open GitHub issues assigned to the user. Use when the user asks about their issues/tickets/tasks on GitHub. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "limit", Type: "string", Required: false, Description: "Max results (default 10, max 25)"},
		},
	},
	{
		Name:        "github_comment",
		Description: "Post a comment on a GitHub issue or pull request on the user's behalf. Use ONLY when the user explicitly asks to comment/reply on a GitHub issue or PR. Requires confirmation.",
		// Published under the connected human's name, notified to subscribers, and
		// retained in the thread's history even if later deleted.
		ExternalEffect: true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "number", Type: "string", Required: true, Description: "Issue or PR number"},
			{Name: "body", Type: "string", Required: true, Description: "Comment text"},
		},
	},
	// --- Tables (first-class structured data). read_table reveals the field
	// ids the model must use as keys when writing rows. Writes require the
	// acting user's access to the table. ---
	{
		Name:        "list_tables",
		Description: "List the data tables the user can access (id, name, visibility). Use to find a table before reading or writing rows. Read-only.",
		ReadOnly:    true,
		Parameters:  []ToolParam{},
	},
	{
		Name:        "read_table",
		Description: "Read a data table's columns (with their field ids) and a sample of its rows. Use to understand a table's schema before creating or updating rows. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "table_uuid", Type: "string", Required: true, Description: "UUID of the table to read"},
		},
	},
	{
		Name:        "query_table",
		Description: "Answer a data question by aggregating a table's rows and get back a breakdown PLUS a ready-to-show chart. Use for 'how many/total/average by …', trends, and breakdowns. Group by a column, aggregate count/sum/avg/min/max of a column, and optionally filter. Columns are referenced by name or id (read_table shows them). Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "table_uuid", Type: "string", Required: true, Description: "UUID of the table to query"},
			{Name: "aggregate", Type: "string", Required: false, Description: "One of count (default), sum, avg, min, max. sum/avg/min/max require value_field."},
			{Name: "value_field", Type: "string", Required: false, Description: "Number column (name or id) to aggregate for sum/avg/min/max"},
			{Name: "group_by", Type: "string", Required: false, Description: "Column (name or id) to break the result down by. Omit for a single total. Array columns (multi-select/person) split into one group per value."},
			{Name: "filters", Type: "string", Required: false, Description: "Optional JSON array of {\"field\":name-or-id,\"op\":one of eq|ne|contains|gt|gte|lt|lte|empty|not_empty,\"value\":string}"},
			{Name: "limit", Type: "string", Required: false, Description: "Max number of groups to return (default 50, top by value)"},
			{Name: "order", Type: "string", Required: false, Description: "asc for smallest-first; default is largest-first"},
		},
	},
	{
		Name:        "query_plan",
		Description: "Answer a MULTI-STEP data question over one table with a typed, deterministic, re-runnable plan — when query_table's single aggregate isn't enough. Supports several metrics at once, filtering GROUPS by a computed metric (having), share-of-total (%), sorting by any metric, and a limit. Read-only; the plan is echoed back so the answer is explainable. Use for questions like 'top 5 owners by won amount, each as a % of total, with their average deal size'. Columns are referenced by name or id (read_table shows them).",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "table_uuid", Type: "string", Required: true, Description: "UUID of the table to query"},
			{Name: "plan", Type: "string", Required: true, Description: "JSON plan: {\"filters\":[{\"field\":..,\"op\":eq|ne|contains|gt|gte|lt|lte|empty|not_empty,\"value\":..}], \"group_by\":\"Column\", \"metrics\":[{\"aggregate\":count|sum|avg|min|max,\"value_field\":\"Column\",\"label\":\"optional\"}], \"having\":[{\"metric\":\"label\",\"op\":gt|gte|lt|lte|eq|ne,\"value\":10}], \"share_of\":\"metric-label\", \"sort_by\":\"metric-label\", \"ascending\":false, \"limit\":5}. Only metrics[] is required; group_by omitted = one total. count needs no value_field."},
		},
	},
	// --- External data sources (read-only warehouse/DB connectors). Same
	// governed shape as the table tools, pushed down to an external database.
	// Query access is per-user (the source's visibility), so these only ever
	// touch a source the acting user may query. The model never writes SQL. ---
	{
		Name:        "list_data_sources",
		Description: "List the external data sources (connected read-only databases/warehouses) the user can query (id, name, engine). Use to find a source before reading its schema or querying it. Read-only.",
		ReadOnly:    true,
		Parameters:  []ToolParam{},
	},
	{
		Name:        "read_data_source",
		Description: "Read an external data source's tables and columns (with their types) so you know the exact schema.table and column names before querying. Reference tables as schema.table. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "data_source_uuid", Type: "string", Required: true, Description: "UUID of the data source (from list_data_sources)"},
		},
	},
	{
		Name:        "query_data_source",
		Description: "Answer a data question from an external data source by aggregating one of its tables, and get back a breakdown PLUS a ready-to-show chart. Group by a column, aggregate count/sum/avg/min/max of a numeric column, optionally filter. Deterministic and read-only — you describe the query, never SQL. Use read_data_source first to see exact table/column names.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "data_source_uuid", Type: "string", Required: true, Description: "UUID of the data source to query"},
			{Name: "table", Type: "string", Required: true, Description: "Table to query, as schema.table (read_data_source lists them)"},
			{Name: "aggregate", Type: "string", Required: false, Description: "One of count (default), sum, avg, min, max. sum/avg/min/max require value_field (a numeric column)."},
			{Name: "value_field", Type: "string", Required: false, Description: "Numeric column to aggregate for sum/avg/min/max"},
			{Name: "group_by", Type: "string", Required: false, Description: "Column to break the result down by. Omit for a single total. A date column groups by day."},
			{Name: "filters", Type: "string", Required: false, Description: "Optional JSON array of {\"field\":column,\"op\":one of eq|ne|contains|gt|gte|lt|lte|empty|not_empty,\"value\":string}"},
			{Name: "limit", Type: "string", Required: false, Description: "Max number of groups to return (default 50, top by value)"},
			{Name: "order", Type: "string", Required: false, Description: "asc for smallest-first; default is largest-first"},
		},
	},
	{
		Name:        "query_data_source_plan",
		Description: "Answer a MULTI-STEP data question from an external data source with a typed, deterministic plan — when query_data_source's single aggregate isn't enough. Supports several metrics at once, filtering GROUPS by a computed metric (having), share-of-total (%), sorting by any metric, and a limit. Read-only; you describe the plan, never SQL. Use for 'top 5 owners by won amount, each as a % of total, with their average deal size'. Use read_data_source first for exact table/column names.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "data_source_uuid", Type: "string", Required: true, Description: "UUID of the data source to query"},
			{Name: "plan", Type: "string", Required: true, Description: "JSON plan: {\"table\":\"schema.table\", \"filters\":[{\"field\":..,\"op\":eq|ne|contains|gt|gte|lt|lte|empty|not_empty,\"value\":..}], \"group_by\":\"column\", \"metrics\":[{\"aggregate\":count|sum|avg|min|max,\"value_field\":\"column\",\"label\":\"optional\"}], \"having\":[{\"metric\":\"label\",\"op\":gt|gte|lt|lte|eq|ne,\"value\":10}], \"share_of\":\"metric-label\", \"sort_by\":\"metric-label\", \"ascending\":false, \"limit\":5}. table and metrics[] are required; count needs no value_field."},
		},
	},
	{
		Name:        "run_analysis",
		Description: "Run a short Python program in a locked-down sandbox (no network, ephemeral filesystem, hard resource limits) to analyze data and produce a result, a chart, or a file. Provide `code` and optional `inputs` (each a table exposed to the code as a pandas DataFrame). Use for computations beyond what query_table can do (custom math, multi-step transforms, correlations). Read-only side effects. (Available only when an admin has enabled the code sandbox.)",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "code", Type: "string", Required: true, Description: "Python code. Each input is preloaded into a variable of the same name (a pandas DataFrame for tables). Print results to stdout; to return a chart, write a chart-spec JSON to the ./out directory (e.g. out/chart.json); to return a file, write it under ./out (e.g. out/report.csv, out/plot.png)."},
			{Name: "inputs", Type: "string", Required: false, Description: "Optional JSON array of data bindings: [{\"name\":\"deals\",\"table_uuid\":\"<id>\",\"query\":{optional aggregate spec}}]. Omit `query` to load the table's rows; include it to load an aggregated summary."},
		},
	},
	{
		Name:        "create_table_row",
		Description: "Add a row to a data table. Use when the user asks to add an entry/record/item to a table. First read_table to learn the field ids.",
		Parameters: []ToolParam{
			{Name: "table_uuid", Type: "string", Required: true, Description: "UUID of the table"},
			{Name: "values", Type: "string", Required: true, Description: "JSON object mapping field id -> value, e.g. {\"<fieldId>\":\"Acme\",\"<fieldId2>\":42}"},
		},
	},
	{
		Name:        "update_table_row",
		Description: "Update an existing row in a data table. Use read_table first to get the row id and field ids.",
		Parameters: []ToolParam{
			{Name: "table_uuid", Type: "string", Required: true, Description: "UUID of the table"},
			{Name: "row_uuid", Type: "string", Required: true, Description: "UUID of the row to update"},
			{Name: "values", Type: "string", Required: true, Description: "JSON object mapping field id -> value for the updated row"},
		},
	},
	{
		Name:        "web_search",
		Description: "Search the public web for current/external information and get back titled results with URLs and snippets. Use ONLY when the answer needs up-to-date or external facts not in the workspace. Read-only. (Only available when an admin has configured a web search provider.)",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "query", Type: "string", Required: true, Description: "The web search query"},
		},
	},
	{
		Name:        "search_workspace",
		Description: "Search ACROSS the user's workspace and their connected accounts in one call: relevant messages/docs/tasks (semantic), distilled Memory facts (decisions/commitments/questions), and connected apps (Gmail, GitHub). Use to find where something was discussed or decided, or to pull related context before acting. Permission-scoped to what the user can already see. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "query", Type: "string", Required: true, Description: "What to search for across the workspace and connected apps"},
		},
	},
	// --- Code understanding (read-only) over the workspace's admin-connected
	// GitHub repo. These never write: they analyze and summarize for a human.
	// Writing code (commit/open a PR) is intentionally NOT a built-in tool —
	// route it through an admin-registered GitHub/coding MCP server so it stays
	// vendor-neutral and behind the same confirmation gate as any other write. ---
	{
		Name:        "code_analyze",
		Description: "Analyze a bug, error, or issue against the workspace's connected GitHub repo: retrieves the relevant source files and returns a root-cause explanation plus a PROPOSED fix as a unified diff for a human to review. Read-only (never commits). Use when asked to investigate a bug or propose a code fix for a linked repo.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "title", Type: "string", Required: true, Description: "Short summary of the bug/issue to analyze"},
			{Name: "body", Type: "string", Required: false, Description: "Details: error message, stack trace, steps, or file hints"},
			{Name: "ref", Type: "string", Required: false, Description: "Branch or commit SHA to read (omit for the default branch)"},
		},
	},
	{
		Name:        "repo_summary",
		Description: "Summarize the workspace's connected GitHub repo: its default branch, top-level structure, and a snapshot of recently merged work. Use to orient before analyzing, or when asked 'what is in this repo' / 'give me an overview of the repo'. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
		},
	},
	{
		Name:        "list_recent_changes",
		Description: "List the pull requests recently MERGED into the workspace's connected GitHub repo over the last N days (a 'what shipped via PRs' summary). Note: this only covers merged PRs — for raw commits on a branch (including direct pushes), use list_commits instead. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "days", Type: "string", Required: false, Description: "How many days back to look (default 14)"},
		},
	},
	{
		Name:        "list_commits",
		Description: "List commits on a branch of the workspace's connected GitHub repo, newest first, using GitHub's DIRECT commits endpoint (not search). Use this whenever asked 'what was committed', 'recent commits', 'what changed today/this week', or to check activity on a repo/branch. Works for PRIVATE repos and is real-time and date-accurate — always prefer this over any repository/commit SEARCH tool, which omits private repos and lags a search index. Read-only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "branch", Type: "string", Required: false, Description: "Branch name to read (omit for the default branch)"},
			{Name: "days", Type: "string", Required: false, Description: "Only commits within the last N days (omit for the latest commits regardless of date)"},
		},
	},
	{
		Name:        "read_repo_file",
		Description: "Read a single file from the workspace's connected GitHub repo (at an optional branch/tag/sha; default branch when omitted). Use to read the ACTUAL source before analyzing or proposing a change. Content is length-bounded. Read-only (never writes).",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "path", Type: "string", Required: true, Description: "File path within the repo, e.g. src/server/main.go"},
			{Name: "ref", Type: "string", Required: false, Description: "Branch, tag, or commit SHA to read (omit for the default branch)"},
		},
	},
	{
		Name:        "search_repo_code",
		Description: "Search code in the workspace's connected GitHub repo and return the matching file paths, so you can LOCATE where a symbol, string, or pattern lives before reading it with read_repo_file. Read-only; indexes the default branch only.",
		ReadOnly:    true,
		Parameters: []ToolParam{
			{Name: "owner", Type: "string", Required: true, Description: "Repository owner (org or user login)"},
			{Name: "repo", Type: "string", Required: true, Description: "Repository name"},
			{Name: "query", Type: "string", Required: true, Description: "Code search query, e.g. a function name, string literal, or identifier"},
			{Name: "limit", Type: "string", Required: false, Description: "Max results (default 10, max 20)"},
		},
	},
	{
		Name:         "code_pr",
		DefersResult: true,
		Description:  "Make a code change to the workspace's connected GitHub repo and open a pull request for a human to review. Provide a clear `instruction` describing the change (bug to fix, small feature, refactor). The change is made in an ISOLATED sandbox, verified against the repo's own build/tests, and opened as a reviewable PR on a fresh branch — it is NEVER merged automatically. This is the ONLY tool for writing code / creating a branch / opening a PR: when the user asks to change code or open a PR, call THIS and nothing else — do NOT use any other GitHub tool (e.g. create-branch, create-pull-request, add-review-comment) to make the branch or PR, or you'll leave a half-finished branch with no PR. This runs as a BACKGROUND job: call it once, then tell the user you're on it and will post the pull request link here when it's ready (do not wait or call it again for the same request). Use ONLY when explicitly asked to change/fix/implement code and open a PR — for read-only investigation use code_analyze instead. Available only when an admin has enabled code PRs and deployed a coding runner.",
		Parameters: []ToolParam{
			{Name: "instruction", Type: "string", Required: true, Description: "A clear, self-contained description of the change to make and open a PR for."},
			{Name: "repo", Type: "string", Required: false, Description: "The target repository as \"owner/name\" (e.g. \"octocat/hello-world\"). Provide it when the user names a specific repo; omit to use the workspace's linked repo. Only repositories the connected GitHub account can access are allowed."},
			{Name: "base_branch", Type: "string", Required: false, Description: "Branch to base the change on (omit for the repo's default branch)."},
		},
	},
}

// webSearchToolName is the registry name of the web-search tool, gated by
// admin enablement at prompt-build time.
const webSearchToolName = "web_search"

// runAnalysisToolName is the registry name of the code-sandbox tool, gated by
// the admin sandbox master switch at prompt-build time.
const runAnalysisToolName = "run_analysis"

// codePRToolName is the registry name of the code-PR tool, gated by the admin
// code-PR master switch at prompt-build time (hidden unless enabled, so the
// model is never offered a tool that would just refuse).
const codePRToolName = "code_pr"

// toolEnabled reports whether a registry tool should be offered to the model
// right now. Most tools are always on; web_search appears only when an admin
// has configured + enabled a provider, so the model is never told about a tool
// that would just error.
func toolEnabled(name string) bool {
	switch name {
	case webSearchToolName:
		return WebSearchEnabled()
	case runAnalysisToolName:
		// run_analysis is only offered when an admin has enabled the code
		// sandbox — otherwise the model would be told about a tool that just
		// refuses.
		return SandboxEnabled()
	case codePRToolName:
		// code_pr is only offered when an admin has enabled the code-PR feature
		// (and, in practice, deployed a coding runner). Hidden otherwise.
		return CodePREnabled()
	}
	return true
}

// BuildToolPrompt generates the system prompt section that describes available tools to the LLM.
// Structure: RULES → NEGATIVE EXAMPLES → FORMAT → POSITIVE EXAMPLES → TOOL LIST.
// Rules-first anchoring is critical for small models (3b-8b) that tend to pattern-match
// the dominant example type. By leading with explicit constraints and showing more no-tool
// examples than tool examples, we drastically reduce false-positive tool triggers.
func BuildToolPrompt() string {
	return buildToolPrompt(ToolRegistry)
}

// BuildToolPromptFiltered is like BuildToolPrompt but describes only the tools
// relevant to the user's request (tool routing). The full catalog dominates
// the prompt's token cost; sending only the tools a request implies keeps each
// agent call small enough to stay under tight provider token-per-minute
// limits, with no loss of capability for that request. If the request matches
// no group, the FULL catalog is used, so routing can never make the agent
// strictly worse.
func BuildToolPromptFiltered(question string) string {
	return buildToolPrompt(selectToolsForQuery(question))
}

// agentToolFullThreshold is the enabled-tool count at or below which an agent's
// tool prompt always renders EVERY tool with full parameter schemas (identical
// to the original behaviour). Above it — the large tool-sets that blow past
// tight provider per-minute token limits (e.g. Groq's 12k TPM) — only the tools
// relevant to the run's request get full parameter schemas; the rest are still
// listed by name + one-line description so nothing is hidden. Chosen so a
// typical, working agent (a handful of tools) is completely unaffected.
const agentToolFullThreshold = 16

// BuildAgentToolPromptForRun is BuildAgentToolPromptFor with query-aware
// compaction. When the agent's enabled tool count is small (<= threshold) or no
// query is supplied, it renders every tool with full parameters — byte-identical
// to the prior behaviour, so ordinary agents are unaffected. For a LARGE
// tool-set it renders full parameter schemas ONLY for the tools relevant to the
// run's request (query), while still listing every other enabled tool by name +
// one-line description so none is hidden. This keeps the per-request prompt
// small enough to stay under tight provider TPM limits without removing any
// capability — the agent can still call a name-only tool, and a broad/empty
// query safely degrades to full detail. Unknown names are ignored; an
// empty/!matching list yields "".
//
// Unlike BuildToolPromptFiltered (interactive assistant), this NEVER drops a
// tool from the agent's allow-list — it only compacts how much of each tool's
// schema is spelled out — so governance (the enabled-tools allow-list) is
// untouched.
func BuildAgentToolPromptForRun(names []string, query string) string {
	if len(names) == 0 {
		return ""
	}
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[n] = true
	}
	var tools []ToolDef
	for _, t := range ToolRegistry {
		if allow[t.Name] && toolEnabled(t.Name) {
			tools = append(tools, t)
		}
	}
	tools = appendDynamicAllowed(tools, allow)
	if len(tools) == 0 {
		return ""
	}

	detailed := selectDetailedTools(tools, query)
	subset := len(detailed) < len(tools)

	var sb strings.Builder
	sb.WriteString("\n\n## Tools\n\n")
	sb.WriteString("You can take actions in the workspace by emitting tool calls. Use a tool ONLY when it helps accomplish your assigned task. ")
	sb.WriteString("Copy any ids (task/project/doc/channel/user) EXACTLY from earlier tool results or the context; never invent or shorten one. ")
	sb.WriteString("When you have finished (or there is nothing to do), reply with a short plain-text summary and NO tool call.\n\n")
	sb.WriteString("TOOL CALL FORMAT (use this exact block, one per action):\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString("{\"tool\": \"TOOL_NAME\", \"params\": {\"param1\": \"value1\"}, \"description\": \"what this does\"}\n")
	sb.WriteString("</tool_call>\n\n")
	if subset {
		// Large tool-set: only the request-relevant tools carry full params to
		// stay under provider TPM limits. Tell the model the rest are still
		// callable so no capability is perceived as lost.
		sb.WriteString("The tools most relevant to this request are shown with their full parameters. ")
		sb.WriteString("The remaining tools are also available — if you need one, call it (a read/list tool result will show the exact parameter names to use).\n\n")
	}
	sb.WriteString("AVAILABLE TOOLS:\n")
	for _, tool := range tools {
		if !detailed[tool.Name] {
			// Name-only tool (large tool-set, not request-relevant): a short
			// one-line description keeps the roster scannable without spending
			// the tokens a full schema would — critical under tight provider TPM
			// limits when many tools (e.g. a 40+ tool MCP server) are enabled.
			sb.WriteString(fmt.Sprintf("- %s: %s\n", tool.Name, compactToolText(tool.Description, 80)))
			continue
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", tool.Name, compactToolText(tool.Description, 240)))
		var params []string
		for _, p := range tool.Parameters {
			req := ""
			if p.Required {
				req = " (required)"
			}
			params = append(params, fmt.Sprintf("%s%s — %s", p.Name, req, compactToolText(p.Description, 100)))
		}
		if len(params) > 0 {
			sb.WriteString("  Params: " + strings.Join(params, ", ") + "\n")
		}
	}
	return sb.String()
}

// selectDetailedTools decides which of an agent's enabled tools get their FULL
// parameter schema in the prompt. For a small tool-set (<= threshold) or no
// query, every tool is detailed (unchanged behaviour). For a large tool-set it
// details only the tools relevant to the query — matched via the same keyword
// groups as the interactive router (selectToolsForQuery) plus a word-overlap
// pass over each tool's name/description (which also covers MCP tools) — capped
// at the threshold and emitted in tool order for determinism. It never returns
// an empty set: if nothing matched, it details the first `threshold` tools so
// the agent always has fully-callable tools.
func selectDetailedTools(tools []ToolDef, query string) map[string]bool {
	detailed := make(map[string]bool, len(tools))
	if strings.TrimSpace(query) == "" || len(tools) <= agentToolFullThreshold {
		for _, t := range tools {
			detailed[t.Name] = true
		}
		return detailed
	}

	inSet := make(map[string]bool, len(tools))
	for _, t := range tools {
		inSet[t.Name] = true
	}

	relevant := make(map[string]bool)
	// 1) keyword-group relevance over the built-in catalog.
	for _, t := range selectToolsForQuery(query) {
		if inSet[t.Name] {
			relevant[t.Name] = true
		}
	}
	// 2) word-overlap relevance over every enabled tool (covers MCP/dynamic
	//    tools and anything the keyword groups miss).
	words := significantWords(query)
	for _, t := range tools {
		if relevant[t.Name] {
			continue
		}
		if toolMatchesWords(t, words) {
			relevant[t.Name] = true
		}
	}

	// Emit in tool order, capped, so a broad match can't re-inflate the prompt.
	n := 0
	for _, t := range tools {
		if relevant[t.Name] {
			detailed[t.Name] = true
			n++
			if n >= agentToolFullThreshold {
				break
			}
		}
	}
	// Never strand the agent with zero fully-callable tools.
	if n == 0 {
		for i, t := range tools {
			if i >= agentToolFullThreshold {
				break
			}
			detailed[t.Name] = true
		}
	}
	return detailed
}

// toolQueryStopWords are common words with no tool-selection signal; skipped so
// word-overlap matching keys on meaningful terms.
var toolQueryStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "you": true, "your": true,
	"please": true, "that": true, "this": true, "have": true, "from": true, "about": true,
	"into": true, "just": true, "then": true, "them": true, "they": true, "want": true,
	"need": true, "make": true, "should": true, "would": true, "could": true, "when": true,
	"what": true, "where": true, "which": true, "there": true, "their": true, "here": true,
	"message": true, "reply": true, "mentioned": true, "channel": true, "group": true,
}

// significantWords extracts the meaningful, de-duplicated lowercase words from a
// query for tool word-overlap matching: alphanumeric tokens of length >= 4 that
// are not stop-words.
func significantWords(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, w := range fields {
		if len(w) < 4 || toolQueryStopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// toolMatchesWords reports whether any significant query word appears in the
// tool's name or description (case-insensitive substring).
func toolMatchesWords(t ToolDef, words []string) bool {
	if len(words) == 0 {
		return false
	}
	hay := strings.ToLower(t.Name + " " + t.Description)
	for _, w := range words {
		if strings.Contains(hay, w) {
			return true
		}
	}
	return false
}

// ToolSpecsForRun builds NATIVE function-calling specs (JSON-Schema) for an
// agent run over its enabled tools, applying the SAME query-aware selection as
// the text prompt: for a large tool-set only the request-relevant tools are
// sent (capped), which both keeps the request under provider token limits and
// focuses the model. Unlike the text path there is no "name-only" tier —
// native tools must carry a full schema to be callable — so a large tool-set
// sends the detailed subset only. Returns nil when nothing is enabled.
func ToolSpecsForRun(names []string, query string) []ToolSpec {
	if len(names) == 0 {
		return nil
	}
	allow := make(map[string]bool, len(names))
	for _, n := range names {
		allow[n] = true
	}
	var tools []ToolDef
	for _, t := range ToolRegistry {
		if allow[t.Name] && toolEnabled(t.Name) {
			tools = append(tools, t)
		}
	}
	tools = appendDynamicAllowed(tools, allow)
	if len(tools) == 0 {
		return nil
	}
	detailed := selectDetailedTools(tools, query)
	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		if !detailed[t.Name] {
			continue
		}
		specs = append(specs, ToolSpec{
			Name:        t.Name,
			Description: compactToolText(t.Description, 240),
			Parameters:  toolParamsSchema(t.Parameters),
		})
	}
	return specs
}

// toolParamsSchema renders a tool's parameters as a JSON-Schema object suitable
// for a provider's native function-calling `parameters` field. All params are
// modeled as strings (the executors coerce), matching the text protocol's
// contract, so behavior is identical across the two paths.
func toolParamsSchema(params []ToolParam) map[string]interface{} {
	props := map[string]interface{}{}
	var required []string
	for _, p := range params {
		typ := "string"
		if p.Type == "boolean" {
			typ = "string" // executors parse booleans from strings; keep the wire type uniform
		}
		props[p.Name] = map[string]interface{}{
			"type":        typ,
			"description": compactToolText(p.Description, 200),
		}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// ToolCallToAction converts a provider's structured tool call into the same
// ProposedAction the text parser produces, so the agent loop's governance and
// executors are shared byte-for-byte across the native and text paths. The
// call's JSON arguments are coerced to the map[string]string params the
// executors expect (non-string values are stringified). A malformed arguments
// blob yields an action with empty params (validation then rejects it), never
// a panic.
func ToolCallToAction(tc ToolCall) ProposedAction {
	params := map[string]string{}
	raw := strings.TrimSpace(tc.Arguments)
	if raw != "" {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			for k, v := range obj {
				params[k] = stringifyArg(v)
			}
		}
	}
	return ProposedAction{ToolName: tc.Name, Params: params}
}

// stringifyArg renders a JSON argument value as the string the executors
// expect. Strings pass through; numbers/bools/objects are formatted compactly.
func stringifyArg(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		// Avoid scientific notation / trailing ".0" for integer-valued numbers.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	}
}

// compactToolText trims a tool/param description for the agent prompt: it keeps
// the first sentence (or a hard char cap) and collapses whitespace. Long,
// multi-paragraph tool descriptions (common on MCP servers) otherwise balloon
// the system prompt — enough to blow tight provider per-minute token limits
// (e.g. Groq's 12k TPM) once several tools are enabled. Keeping just the
// leading, most informative text preserves tool selection while shrinking the
// request. Generic: applied to every tool/param uniformly.
func compactToolText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return s
	}
	// Prefer cutting at the first sentence boundary when it is reasonably early.
	if i := strings.IndexAny(s, "."); i >= 0 && i+1 <= max {
		return s[:i+1]
	}
	if len(s) > max {
		return strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// ToolIsReadOnly reports whether a registered tool has no side effects. Unknown
// tools are treated as writes (fail-safe). Used by the agent runner's dry-run.
func ToolIsReadOnly(name string) bool {
	for _, t := range ToolRegistry {
		if t.Name == name {
			return t.ReadOnly
		}
	}
	if d, ok := dynamicToolDef(name); ok {
		return d.ReadOnly
	}
	return false
}

// ToolIsDestructive reports whether a registered tool performs an
// irreversible/high-risk mutation (per its MCP destructiveHint). Unknown tools
// are NOT assumed destructive (only readOnly is fail-safe); the value is
// advisory governance used to keep such tools out of unattended auto-run.
func ToolIsDestructive(name string) bool {
	for _, t := range ToolRegistry {
		if t.Name == name {
			return t.Destructive
		}
	}
	if d, ok := dynamicToolDef(name); ok {
		return d.Destructive
	}
	return false
}

// ToolHasExternalEffect reports whether a registered tool's effect leaves this
// workspace irrecoverably (see ToolDef.ExternalEffect). Unknown tools are NOT
// assumed to (only readOnly is fail-safe), matching ToolIsDestructive: an
// unknown name reaches no executor, so there is nothing to gate.
func ToolHasExternalEffect(name string) bool {
	for _, t := range ToolRegistry {
		if t.Name == name {
			return t.ExternalEffect
		}
	}
	if d, ok := dynamicToolDef(name); ok {
		return d.ExternalEffect
	}
	return false
}

// ToolDefersResult reports whether a tool's success only starts background work
// whose outcome is reported later. Unknown tools are not assumed to: their
// success is taken at its word, as before.
func ToolDefersResult(name string) bool {
	for _, t := range ToolRegistry {
		if t.Name == name {
			return t.DefersResult
		}
	}
	return false
}

// ToolNeedsHumanBeforeUnattended reports whether a tool must not run without a
// human approving it first, however much autonomy the caller has been granted.
//
// This is the ONE predicate every unattended execution path should ask, and the
// reason it exists is that its two inputs are different facts that must not be
// conflated at each call site: Destructive is about wrecking state that exists
// (and is what a remote MCP server declares via destructiveHint), ExternalEffect
// is about an effect escaping the workspace and never coming back. A caller does
// not care which — it cares only whether a person has to say yes. Adding a third
// such fact later means editing this function, not hunting for gates.
func ToolNeedsHumanBeforeUnattended(name string) bool {
	return ToolIsDestructive(name) || ToolHasExternalEffect(name)
}

func buildToolPrompt(tools []ToolDef) string {
	var sb strings.Builder

	// Drop admin-gated tools that aren't currently available (e.g. web_search
	// when no provider is configured) so the model is never told about a tool
	// it can't use.
	if len(tools) > 0 {
		filtered := tools[:0:0]
		for _, t := range tools {
			if toolEnabled(t.Name) {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	// ── 1. CRITICAL RULES (read first, anchor behavior before examples) ──
	sb.WriteString("\n\n## Actions\n\n")
	sb.WriteString("CRITICAL RULES — you MUST follow these before anything else:\n")
	sb.WriteString("1. DEFAULT BEHAVIOR: Reply with plain text. Most user messages do NOT need a tool_call.\n")
	sb.WriteString("2. ONLY use <tool_call> when the user gives an EXPLICIT action command containing verbs like: \"send\", \"post\", \"create\", \"DM\", \"message\", \"remind\", \"write\", \"draft\".\n")
	sb.WriteString("3. If the user is greeting, chatting, asking questions, discussing, or sharing thoughts — NEVER use <tool_call>. Just reply with text.\n")
	sb.WriteString("4. Greetings (hi, hello, hey, namaste, good morning, what's up, love, etc.) → ALWAYS reply with friendly text, NEVER use <tool_call>.\n")
	sb.WriteString("5. Questions about workspace content → Answer from context, NEVER use <tool_call>.\n")
	sb.WriteString("6. NEVER proactively send messages, create tasks, or perform actions the user did not explicitly ask for.\n")
	sb.WriteString("7. When you DO use a tool, write a short text response FIRST, then the <tool_call> block.\n")
	sb.WriteString("8. IDs: When the user names a task, project, doc, or channel to act on (e.g. \"mark my login bug task done\"), use the matching id from the workspace context provided to you and put it in the write tool's params in ONE step. Copy ids EXACTLY, character for character; NEVER invent or shorten one. Only if the id is not present in the context, call the matching read tool (list_tasks, list_projects, read_doc, ...) first to look it up.\n\n")

	// ── 2. NEGATIVE EXAMPLES (no-tool responses — shown FIRST to anchor default) ──
	sb.WriteString("EXAMPLES OF MESSAGES THAT DO NOT USE TOOLS (just reply with text):\n\n")
	sb.WriteString("User: \"hi\"\nAssistant: Hello! How can I help you with your workspace today?\n\n")
	sb.WriteString("User: \"hello\"\nAssistant: Hi there! What can I do for you?\n\n")
	sb.WriteString("User: \"namaste\"\nAssistant: Namaste! How can I assist you today?\n\n")
	sb.WriteString("User: \"how are you?\"\nAssistant: I'm doing great! What can I help you with in your workspace?\n\n")
	sb.WriteString("User: \"what's happening in #general?\"\nAssistant: Based on your workspace, here's what's been discussed in #general... [answer from context]\n\n")
	sb.WriteString("User: \"tell me about the project\"\nAssistant: Here's what I found about your project... [answer from context]\n\n")

	// ── 3. TOOL CALL FORMAT ──
	sb.WriteString("TOOL CALL FORMAT — when (and ONLY when) the user gives an explicit action command, use this exact format:\n\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString("{\"tool\": \"TOOL_NAME\", \"params\": {\"param1\": \"value1\", \"param2\": \"value2\"}, \"description\": \"What this action will do\"}\n")
	sb.WriteString("</tool_call>\n\n")

	// ── 4. POSITIVE EXAMPLES (tool-call responses — fewer than negative) ──
	sb.WriteString("EXAMPLES OF MESSAGES THAT DO USE TOOLS:\n\n")
	sb.WriteString("User: \"post status report in #engineering\" (channel #engineering has id abc-123)\n")
	sb.WriteString("Assistant: I'll post that report for you.\n\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString("{\"tool\": \"send_message\", \"params\": {\"channel_uuid\": \"abc-123\", \"text\": \"Status report: all systems go\"}, \"description\": \"Send status report to #engineering\"}\n")
	sb.WriteString("</tool_call>\n\n")
	sb.WriteString("User: \"DM John about the project\" (John's uuid is def-456)\n")
	sb.WriteString("Assistant: I'll DM John for you.\n\n")
	sb.WriteString("<tool_call>\n")
	sb.WriteString("{\"tool\": \"send_dm\", \"params\": {\"to_uuid\": \"def-456\", \"text\": \"Can we talk about the project?\"}, \"description\": \"DM John about project\"}\n")
	sb.WriteString("</tool_call>\n\n")

	// ── 5. AVAILABLE TOOLS ──
	sb.WriteString("AVAILABLE TOOLS:\n")
	for _, tool := range tools {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", tool.Name, tool.Description))
		sb.WriteString("  Params: ")
		var params []string
		for _, p := range tool.Parameters {
			req := ""
			if p.Required {
				req = " (required)"
			}
			params = append(params, fmt.Sprintf("%s%s — %s", p.Name, req, p.Description))
		}
		sb.WriteString(strings.Join(params, ", "))
		sb.WriteString("\n")
	}

	return sb.String()
}

// toolGroup bundles tools by domain with the keywords that imply that domain.
// Read dependencies are included with their domain (e.g. the task group
// carries list_projects so "create a task in <project>" can resolve the
// project), so routing never strands a write tool without the read it needs.
type toolGroup struct {
	keywords []string
	tools    []string
}

var toolGroups = []toolGroup{
	{
		keywords: []string{"task", "tasks", "todo", "todos", "ticket", "tickets", "assign", "assignee", "reassign", "unassign", "due", "deadline", "backlog", "overdue", "mark", "done", "in progress", "inprogress", "in review", "status"},
		tools:    []string{"list_tasks", "list_project_tasks", "create_task", "update_task_status", "assign_task", "set_task_due_date", "list_projects"},
	},
	{
		keywords: []string{"project", "projects"},
		tools:    []string{"list_projects", "read_project", "create_project", "list_project_tasks", "list_teams"},
	},
	{
		keywords: []string{"team", "teams"},
		tools:    []string{"list_teams", "create_project"},
	},
	{
		keywords: []string{"doc", "docs", "document", "documents", "note", "notes", "page", "prd", "spec", "draft", "write up", "write-up"},
		tools:    []string{"read_doc", "create_doc"},
	},
	{
		keywords: []string{"message", "send", "post", "dm", "channel", "channels", "notify", "tell", "ping", "announce", "reply"},
		tools:    []string{"send_message", "send_dm", "send_group_chat"},
	},
	{
		keywords: []string{"summarize", "summarise", "summary", "recap", "catch up", "catch me up", "what happened", "what did i miss"},
		tools:    []string{"summarize_channel", "summarize_dm", "summarize_group_chat"},
	},
	{
		keywords: []string{"remind", "reminder", "schedule", "event", "meeting", "calendar", "invite", "appointment"},
		tools:    []string{"set_reminder", "calendar_list_events", "calendar_create_event"},
	},
	{
		keywords: []string{"email", "emails", "gmail", "inbox", "mail"},
		tools:    []string{"gmail_search", "gmail_send"},
	},
	{
		keywords: []string{"pr", "prs", "pull request", "pull requests", "issue", "issues", "github", "code review", "merge"},
		tools:    []string{"github_list_prs", "github_list_issues", "github_comment"},
	},
	{
		keywords: []string{"repo", "repository", "codebase", "code", "bug", "fix", "stack trace", "commit", "commits", "diff", "shipped", "merged", "changelog", "release notes"},
		tools:    []string{"code_analyze", "repo_summary", "list_recent_changes", "list_commits"},
	},
	{
		keywords: []string{"search the web", "web search", "google", "look up online", "on the internet", "latest", "current", "news", "today's", "recent news", "online"},
		tools:    []string{"web_search"},
	},
}

// selectToolsForQuery returns the subset of the catalog relevant to the user's
// request (tool routing), preserving registry order. When the request matches
// no group it returns the FULL catalog as a safe fallback, so a missed keyword
// degrades to "send everything" (today's behaviour) rather than stranding a
// needed tool.
func selectToolsForQuery(question string) []ToolDef {
	lower := strings.ToLower(question)
	picked := make(map[string]bool)
	for _, g := range toolGroups {
		if containsWholeWord(lower, g.keywords) {
			for _, name := range g.tools {
				picked[name] = true
			}
		}
	}
	if len(picked) == 0 {
		return ToolRegistry
	}
	out := make([]ToolDef, 0, len(picked))
	for _, t := range ToolRegistry {
		if picked[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// reasoningBlockRe matches a complete chain-of-thought block emitted by
// reasoning models (qwen3, deepseek-r1, gemma "thinking", …): <think>…</think>
// or <thinking>…</thinking>, case-insensitive and spanning newlines.
var reasoningBlockRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)

// StripReasoning removes a reasoning model's chain-of-thought from its output so
// the visible answer (and the tool-call parser) never sees it. Reasoning models
// wrap their private deliberation in <think>…</think>; if that leaks through it
// gets posted verbatim as the agent's reply AND, when a turn is ONLY reasoning,
// the tool-loop mistakes it for a final answer and stops before acting. We strip
// complete blocks, and also handle the common partial cases where a provider has
// already consumed one side of the delimiter:
//   - a dangling "</think>" with no opening → keep only what follows it,
//   - an opening "<think>" with no closing → drop it (reasoning with no answer yet).
func StripReasoning(s string) string {
	out := reasoningBlockRe.ReplaceAllString(s, "")
	// Orphan closing tag (opening already stripped by the provider): the real
	// answer is whatever comes AFTER the last close.
	if idx := lastIndexFold(out, "</think>"); idx != -1 {
		out = out[idx+len("</think>"):]
	} else if idx := lastIndexFold(out, "</thinking>"); idx != -1 {
		out = out[idx+len("</thinking>"):]
	}
	// Orphan opening tag (no close): everything after it is unfinished reasoning.
	if idx := indexFold(out, "<think>"); idx != -1 {
		out = out[:idx]
	} else if idx := indexFold(out, "<thinking>"); idx != -1 {
		out = out[:idx]
	}
	return strings.TrimSpace(out)
}

// indexFold / lastIndexFold are case-insensitive substring locators (the tag
// casing varies across providers).
func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(sub))
}

func lastIndexFold(s, sub string) int {
	return strings.LastIndex(strings.ToLower(s), strings.ToLower(sub))
}

// ParseToolCalls extracts <tool_call> JSON blocks from LLM output.
// Includes safety guards: max 10 iterations, bounds checking, nil Params protection.
func ParseToolCalls(response string) (cleanResponse string, actions []ProposedAction) {
	cleanResponse = response
	maxIterations := 10 // Guard against infinite loops from malformed LLM output

	for i := 0; i < maxIterations; i++ {
		startIdx := strings.Index(cleanResponse, "<tool_call>")
		if startIdx == -1 {
			break
		}

		restAfterStart := cleanResponse[startIdx:]
		endIdx := strings.Index(restAfterStart, "</tool_call>")
		if endIdx == -1 {
			// Malformed: opening tag without closing tag — strip it and stop
			cleanResponse = cleanResponse[:startIdx] + cleanResponse[startIdx+len("<tool_call>"):]
			break
		}

		// Calculate absolute end position
		absEnd := startIdx + endIdx + len("</tool_call>")

		// Extract JSON between tags
		jsonStart := startIdx + len("<tool_call>")
		jsonEnd := startIdx + endIdx
		if jsonStart >= jsonEnd || jsonEnd > len(cleanResponse) {
			// Bounds check failed — strip the block and continue
			cleanResponse = cleanResponse[:startIdx] + cleanResponse[absEnd:]
			continue
		}

		jsonBlock := strings.TrimSpace(cleanResponse[jsonStart:jsonEnd])

		var raw struct {
			Tool        string            `json:"tool"`
			Params      map[string]string `json:"params"`
			Description string            `json:"description"`
		}
		if err := json.Unmarshal([]byte(jsonBlock), &raw); err == nil {
			// Ensure Params is never nil
			if raw.Params == nil {
				raw.Params = make(map[string]string)
			}
			if raw.Tool != "" {
				actions = append(actions, ProposedAction{
					ToolName:    raw.Tool,
					Params:      raw.Params,
					Description: raw.Description,
				})
			}
		}

		// Remove the tool_call block from the response
		cleanResponse = cleanResponse[:startIdx] + cleanResponse[absEnd:]
	}

	cleanResponse = strings.TrimSpace(cleanResponse)

	// Deduplicate: small models sometimes echo the prompt example as a second
	// action, or repeat the exact same call. Dedupe by full signature (tool +
	// params), NOT by tool name, so genuinely distinct calls of the same tool
	// (e.g. "DM John AND DM Sarah", or list tasks in two projects) are all
	// preserved while exact duplicates are dropped.
	seen := map[string]bool{}
	dedupedActions := make([]ProposedAction, 0, len(actions))
	for _, a := range actions {
		// GUARD against prompt echoing: if it uses the exact dummy values from the example, drop it
		if a.Params["channel_uuid"] == "abc-123" || a.Params["to_uuid"] == "def-456" {
			continue
		}

		sig := ActionSignature(a)
		if !seen[sig] {
			seen[sig] = true
			dedupedActions = append(dedupedActions, a)
		}
	}
	actions = dedupedActions

	return
}

// ValidateAction checks that a proposed action uses a known tool with required params.
func ValidateAction(action ProposedAction) error {
	for _, tool := range ToolRegistry {
		if tool.Name == action.ToolName {
			// Check required params
			for _, param := range tool.Parameters {
				if param.Required {
					val, ok := action.Params[param.Name]
					if !ok || val == "" {
						return fmt.Errorf("missing required parameter: %s", param.Name)
					}
				}
			}
			return nil
		}
	}
	if tool, ok := dynamicToolDef(action.ToolName); ok {
		for _, param := range tool.Parameters {
			if param.Required {
				val, ok := action.Params[param.Name]
				if !ok || val == "" {
					return fmt.Errorf("missing required parameter: %s", param.Name)
				}
			}
		}
		return nil
	}
	return fmt.Errorf("unknown tool: %s", action.ToolName)
}

// ExecuteTool runs a validated action. This is called after user confirmation.
// It returns a human-readable result message and optional metadata for FE state updates.
type ToolExecutor func(ctx context.Context, action ProposedAction, userUUID string) (string, map[string]string, error)

// Executors maps tool names to their execution functions.
// These are set during initialization to avoid circular imports.
var Executors = map[string]ToolExecutor{}

// MetaAgentFinal is a tool-executor metadata key: when an executor returns it
// set to "true" (with no error), the agent run ENDS this turn and the tool's
// own result string becomes the final user-facing reply — the model is not
// asked to summarize again. Use it for fire-and-forget / background tools (e.g.
// code_pr) whose honest acknowledgement ("On it — I'll post the result here")
// must not be overwritten by a model-fabricated "Done."
const MetaAgentFinal = "agent_final"

// MetaStructuredJSON is a tool-executor metadata key carrying the result as JSON,
// for callers that render it rather than read it: an MCP host shows it in an
// interactive view (MCP Apps), and the public API returns it under data. The
// text result stays the model's version; this one is for a screen.
const MetaStructuredJSON = "structured_json"

// ActionSignature returns a deterministic identity for an action — its tool
// name plus its params in sorted order. Used to dedupe repeated tool calls
// within an agent loop so a model that ignores the "don't repeat" instruction
// can never trigger the same (potentially expensive, paid) read twice.
func ActionSignature(a ProposedAction) string {
	keys := make([]string, 0, len(a.Params))
	for k := range a.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString(a.ToolName)
	for _, k := range keys {
		sb.WriteString("|")
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(a.Params[k])
	}
	return sb.String()
}

// IsReadOnlyTool reports whether a tool has no side effects (search/list/
// summarize). Read-only tools are safe to auto-execute inside the agent loop;
// write tools are always surfaced for explicit user confirmation. Unknown
// tools are treated as NOT read-only (fail safe — never auto-run something
// we don't recognize).
func IsReadOnlyTool(toolName string) bool {
	for _, tool := range ToolRegistry {
		if tool.Name == toolName {
			return tool.ReadOnly
		}
	}
	return false
}

// ClassifyActions splits a set of proposed actions into the read-only ones
// (safe to auto-execute) and the write ones (must be confirmed by the user).
// Unknown/invalid tools are dropped from both lists so they can never be run
// or surfaced.
func ClassifyActions(actions []ProposedAction) (reads []ProposedAction, writes []ProposedAction) {
	for _, a := range actions {
		if ValidateAction(a) != nil {
			continue // unknown tool or missing required params — drop
		}
		if IsReadOnlyTool(a.ToolName) {
			reads = append(reads, a)
		} else {
			writes = append(writes, a)
		}
	}
	return reads, writes
}

// ShouldIncludeTools decides whether the tool catalog should be described to
// the model for a given question. Tools are included unless the question is a
// pure read/summary intent — EXCEPT when the question also contains a WRITE
// verb (e.g. "summarize #eng AND dm John") or references an entity our read
// tools cover (tasks/projects/teams), where the tools must be present so the
// agent loop can read first and then answer or act. A pure activity recap
// ("catch me up on #general") keeps tools excluded to save tokens.
func ShouldIncludeTools(question string) bool {
	if !IsSummaryOrReadIntent(question) {
		return true
	}
	if containsWriteVerb(question) {
		return true
	}
	// A current/external-info question ("latest version of X", "search the web
	// for ...") is a read intent that our WORKSPACE read tools can't answer —
	// but web_search can. Expose the catalog so the model can reach for it,
	// but only when web search is actually configured (else it'd be offered a
	// tool that can't run, and the buildToolPrompt gate would strip it anyway).
	if WebSearchEnabled() && mentionsWebSearchIntent(question) {
		return true
	}
	return mentionsToolEntity(question)
}

// webSearchIntentKeywords signal a question that needs current or external
// information (kept in sync with the "web" tool-routing group).
var webSearchIntentKeywords = []string{
	"search the web", "web search", "google", "look up online", "on the internet",
	"latest", "current", "news", "today's", "recent news", "online", "up to date",
	"up-to-date", "this year", "right now", "stock price", "weather",
}

// mentionsWebSearchIntent reports whether the text implies a need for current/
// external information that only web search can satisfy.
func mentionsWebSearchIntent(text string) bool {
	return containsWholeWord(text, webSearchIntentKeywords)
}

// toolEntityNouns are the workspace nouns our read tools can resolve. When a
// summary-style question names one of these, we expose the tools so the model
// can fetch authoritative data (e.g. "summarize my tasks" -> list_tasks).
var toolEntityNouns = []string{
	"task", "tasks", "todo", "todos",
	"project", "projects", "team", "teams",
}

// mentionsToolEntity reports whether the text names a task/project/team entity.
func mentionsToolEntity(text string) bool {
	return containsWholeWord(text, toolEntityNouns)
}

// writeActionVerbs are verbs that imply a side-effecting action (a write),
// as opposed to read verbs like "summarize"/"recap" which are already in
// actionVerbs. Used by ShouldIncludeTools so a combined "read then act"
// request still exposes the tools needed for the write step.
var writeActionVerbs = []string{
	"send", "post", "create", "dm", "message", "remind",
	"write", "draft", "make", "add", "set", "schedule",
	"assign", "share",
}

// containsWriteVerb reports whether the text contains a whole-word write verb.
func containsWriteVerb(text string) bool {
	return containsWholeWord(text, writeActionVerbs)
}

// RegisterExecutor registers an execution function for a tool.
func RegisterExecutor(toolName string, executor ToolExecutor) {
	Executors[toolName] = executor
}

// actionVerbs are explicit command verbs that signal the user wants the AI to act.
var actionVerbs = []string{
	"send", "post", "create", "dm", "message", "remind", "write", "draft",
	"make", "add", "set", "schedule", "assign", "share", "summarise", "summarize", "catch up", "recap",
}

// greetingPatterns are common greetings and casual phrases that should never trigger tools.
var greetingPatterns = []string{
	"hi", "hello", "hey", "namaste", "good morning", "good afternoon",
	"good evening", "good night", "howdy", "yo", "sup", "hola",
	"what's up", "whats up", "how are you", "how's it going",
	"how are things", "love", "thanks", "thank you", "bye",
	"goodbye", "see you", "ok", "okay", "sure", "yes", "no",
	"cool", "nice", "great", "awesome", "wow", "lol", "haha",
	"jai shree ram", "jai shri ram",
}

// containsWholeWord reports whether any of words appears in text as a whole
// word — bounded by start/whitespace before and whitespace or sentence
// punctuation after. Case-insensitive. This is the single matcher shared by
// every verb/keyword check so the boundary semantics stay consistent.
func containsWholeWord(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		idx := strings.Index(lower, w)
		if idx == -1 {
			continue
		}
		// Boundary before: start of string or whitespace.
		if idx > 0 && lower[idx-1] != ' ' && lower[idx-1] != '\n' && lower[idx-1] != '\t' {
			continue
		}
		// Boundary after: end of string, whitespace, or sentence punctuation.
		end := idx + len(w)
		if end < len(lower) && lower[end] != ' ' && lower[end] != '\n' && lower[end] != '\t' && lower[end] != '.' && lower[end] != ',' && lower[end] != '!' && lower[end] != '?' {
			continue
		}
		return true
	}
	return false
}

// containsActionVerb checks if the text contains any explicit action verb.
func containsActionVerb(text string) bool {
	return containsWholeWord(text, actionVerbs)
}

// IsConversational returns true if the user's question is clearly conversational
// (greeting, casual chat) and should NOT use workspace context (RAG).
// This is a deterministic, fast heuristic that acts as a backend safety net
// for unreliable small LLMs that may hallucinate tool calls.
//
// IMPORTANT: This function must be VERY conservative. When in doubt, return false
// (which triggers RAG). The cost of a false negative (unnecessary RAG) is just latency.
// The cost of a false positive (skipping RAG for a workspace question) is a broken answer.
func IsConversational(question string) bool {
	q := strings.ToLower(strings.TrimSpace(question))

	// Empty or single-character inputs are never actions
	if len(q) <= 1 {
		return true
	}

	// If the question contains any workspace-intent keyword, it's NEVER conversational.
	// These words signal the user is asking about their workspace — RAG is mandatory.
	workspaceKeywords := []string{
		"team", "project", "task", "channel", "chat", "dm", "message",
		"post", "doc", "comment", "member", "update", "discuss",
		"pending", "assigned", "overdue", "deadline", "meeting",
		"workspace", "recent", "today", "yesterday", "week",
		"status", "progress", "report", "activity", "notification",
	}
	for _, kw := range workspaceKeywords {
		if strings.Contains(q, kw) {
			return false
		}
	}

	// Check known greeting/casual patterns (exact match or prefix match)
	for _, g := range greetingPatterns {
		if q == g {
			return true
		}
		// Match greetings with trailing punctuation: "hi!", "hello.", "hey?"
		if len(q) <= len(g)+3 && strings.HasPrefix(q, g) {
			suffix := q[len(g):]
			if suffix == "" || strings.Trim(suffix, "!?.,~ ") == "" {
				return true
			}
		}
	}

	// Very short inputs (< 15 chars) without action verbs are likely casual
	if len(q) < 15 && !containsActionVerb(q) {
		return true
	}

	return false
}

// summaryIntentPatterns are phrases that signal the user wants to know what happened
// in their workspace. These trigger chronological context fetching.
var summaryIntentPatterns = []string{
	"summarize", "summarise", "summary",
	"catch up", "catch me up", "catchup",
	"what happened", "what's happened",
	"recap", "recent", "updates",
	"what's new", "whats new",
	"what did", "what has",
	"any news", "anything new",
	"what's going on", "whats going on",
	"what have i missed", "what did i miss",
	"bring me up to speed",
	"overview", "highlights",
}

// IsSummaryOrReadIntent returns true if the question is asking about workspace state
// (updates, summaries, what happened) rather than requesting a write action.
// Used to:
//   - Skip tool definitions in the system prompt (saves ~500 tokens)
//   - Trigger broad chronological context fetching
func IsSummaryOrReadIntent(question string) bool {
	q := strings.ToLower(strings.TrimSpace(question))
	for _, pattern := range summaryIntentPatterns {
		if strings.Contains(q, pattern) {
			return true
		}
	}
	return false
}

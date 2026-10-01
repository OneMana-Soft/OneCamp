package adapter

// DTOs for the Workspace Memory Layer API. These are the border types
// between the HTTP layer and the FE "what does my workspace know" surface.

// MemoryItemView is one memory item as shown to a user.
type MemoryItemView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // decision | commitment | question | glossary
	Content     string `json:"content"`
	Status      string `json:"status"` // open | resolved | superseded | dismissed
	OwnerID     string `json:"owner_user_id,omitempty"`
	DueAt       string `json:"due_at,omitempty"`
	ChannelUUID string `json:"channel_uuid,omitempty"`
	ProjectUUID string `json:"project_uuid,omitempty"`
	ChatGrpID   string `json:"chat_grp_id,omitempty"`
	SourceType  string `json:"source_type"`
	SourceUUID  string `json:"source_uuid,omitempty"`
	Confidence  int    `json:"confidence"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`

	// ── Resolved scope display (server-side, permission-safe) ──────────
	// The list endpoint only returns UUIDs; resolving human names here —
	// once, against the caller's own accessible resources — keeps every
	// client from re-implementing it and guarantees a name is only shown
	// for a scope the user can actually see. ScopeType tells the FE how to
	// render and route the backlink; ScopeLabel is the display string
	// (channel/project name, group participant list, or DM peer name).
	ScopeType   string `json:"scope_type,omitempty"`   // channel | project | group | dm
	ScopeLabel  string `json:"scope_label,omitempty"`  // e.g. "design", "Q3 Launch", "Alice, Bob"
	ChannelName string `json:"channel_name,omitempty"` // when scope_type=channel
	ProjectName string `json:"project_name,omitempty"` // when scope_type=project
}

// MemoryListResponse is returned by GET /ai/memory.
type MemoryListResponse struct {
	Items  []MemoryItemView `json:"items"`
	Counts map[string]int   `json:"counts"` // open count per kind
}

// UpdateMemoryStatusRequest changes a memory item's lifecycle status.
type UpdateMemoryStatusRequest struct {
	Status string `json:"status"`
}

// UpdateMemoryDueRequest sets or clears a commitment's due date. An empty
// `due` clears the deadline (writes SQL NULL); otherwise it must be an ISO
// date "YYYY-MM-DD".
type UpdateMemoryDueRequest struct {
	Due string `json:"due"` // "YYYY-MM-DD", or "" to clear
}

// CaptureMemoryRequest is a user-initiated "save this message to memory"
// action. The user picks the kind; the backend verifies access to the
// scope and persists a high-confidence item linked to the source message
// (so the deletion cascade cleans it up if the message is later deleted).
type CaptureMemoryRequest struct {
	Kind       string `json:"kind"`    // decision | commitment | question
	Content    string `json:"content"` // the message text (plain)
	SourceType string `json:"source_type"`
	SourceUUID string `json:"source_uuid"`
	// Optional due date for a commitment ("YYYY-MM-DD"). Ignored for other
	// kinds. Lets a user set a deadline at capture time so the overdue nudge
	// can fire.
	Due string `json:"due,omitempty"`
	// Exactly one scope is expected, mirroring how content is scoped.
	ChannelUUID string `json:"channel_uuid,omitempty"`
	ChatGrpID   string `json:"chat_grp_id,omitempty"`
}

// CreateTaskFromMemoryRequest converts a memory item into a project task.
type CreateTaskFromMemoryRequest struct {
	ProjectUUID  string `json:"project_uuid"`
	AssigneeUUID string `json:"assignee_uuid,omitempty"`
	Priority     string `json:"priority,omitempty"`
}

// RemindAboutMemoryRequest creates a calendar reminder for a memory item.
type RemindAboutMemoryRequest struct {
	StartTime string `json:"start_time"` // RFC3339
}

// BriefingHighlight is one recent-activity item in the personal briefing.
type BriefingHighlight struct {
	ContentType string `json:"content_type"`
	ContentUUID string `json:"content_uuid"`
	ChannelUUID string `json:"channel_uuid,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	AuthorName  string `json:"author_name,omitempty"`
	Snippet     string `json:"snippet"`

	// Chat routing fields — let the FE deep-link a chat/DM highlight to the
	// right conversation. ChatGrpID is a 32-char id for group chats; for DMs
	// it contains a space and the FE derives the other participant from
	// ChatByUserID/ChatToUserID.
	ChatGrpID    string `json:"chat_grp_id,omitempty"`
	ChatByUserID string `json:"chat_by_user_id,omitempty"`
	ChatToUserID string `json:"chat_to_user_id,omitempty"`

	// Parent FKs for a "comment" highlight, so it deep-links to its parent
	// (post/task/doc). Empty for non-comment types.
	PostUUID string `json:"post_uuid,omitempty"`
	TaskUUID string `json:"task_uuid,omitempty"`
	DocUUID  string `json:"doc_uuid,omitempty"`
}

// BriefingDayItem is one entry in the cross-connector "Your day" section of
// the briefing: a calendar event, a pull request needing attention, or an
// important email. Source identifies which connector it came from so the FE
// can icon/route it; URL deep-links to the item in the external tool.
type BriefingDayItem struct {
	Source   string `json:"source"`             // "calendar" | "github" | "gmail"
	Kind     string `json:"kind"`               // "event" | "pr" | "issue" | "email"
	Title    string `json:"title"`              // event title / PR title / email subject
	Subtitle string `json:"subtitle,omitempty"` // time / repo#num / sender
	URL      string `json:"url,omitempty"`      // deep link into the external tool
}

// BriefingResponse powers the home-screen "Your briefing" card: the user's
// own open items + recent workspace highlights + a cross-connector "Your day"
// section (calendar/PRs/email) when the user has connected those accounts.
// Enabled=false → hide card.
type BriefingResponse struct {
	Enabled    bool                `json:"enabled"`
	OpenItems  []MemoryItemView    `json:"open_items"`
	Highlights []BriefingHighlight `json:"highlights"`
	// DayItems is the merged cross-connector agenda. Empty when no connectors
	// are linked, so the FE hides that section without extra signalling.
	DayItems []BriefingDayItem `json:"day_items"`
}

// AttentionItem is one entry in the cross-surface "what needs me now" queue:
// a single thing that requires the member's action, drawn from any surface
// (a pending approval, an overdue task, an overdue commitment, an open
// question, or an upcoming calendar event). It is a thin, render-ready DTO so
// the FE can list everything in one prioritized view and deep-link each item
// to its source.
type AttentionItem struct {
	Source   string `json:"source"`             // approval | task | commitment | question | calendar
	Kind     string `json:"kind"`               // a short label for the row (e.g. "Overdue task")
	Title    string `json:"title"`              // the thing itself
	Subtitle string `json:"subtitle,omitempty"` // context (due date, scope, "Needs your approval")
	URL      string `json:"url,omitempty"`      // deep link into the source surface
	DueAt    string `json:"due_at,omitempty"`   // YYYY-MM-DD when dated; drives sort within a tier
	// DueTime is the exact moment (RFC 3339, UTC), so the client can say it in
	// the viewer's own time zone. Subtitle carries a server-formatted date too,
	// in the server's zone, and is kept for clients that predate this field.
	DueTime string `json:"due_time,omitempty"`
	// Context is what the row belongs to (a task's project), without the date.
	Context  string `json:"context,omitempty"`
	Priority int    `json:"priority"` // lower = more urgent (sort key, tier)
	// RefID carries the source entity id when the FE needs to act on it inline
	// without navigating (e.g. an approval's pending-action id for Approve/Deny).
	RefID string `json:"ref_id,omitempty"`
}

// AttentionResponse powers the unified "what needs me now" surface: one
// prioritized list spanning every surface the member can see, plus per-source
// counts for badges. Enabled=false → the FE hides the surface (AI off).
type AttentionResponse struct {
	Enabled bool            `json:"enabled"`
	Items   []AttentionItem `json:"items"`
	Counts  map[string]int  `json:"counts"` // per-source counts for headers/badges
}

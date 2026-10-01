package adapter

// SummarizeChannelRequest is the request body for POST /ai/summarize/channel.
type SummarizeChannelRequest struct {
	ChannelUUID  string `json:"channel_uuid"`
	MessageCount int    `json:"message_count,omitempty"` // default 50
	Timezone     string `json:"timezone,omitempty"`
	Location     string `json:"location,omitempty"`
	LocalTime    string `json:"local_time,omitempty"`
}

// SummarizeDMRequest is the request body for POST /ai/summarize/dm.
type SummarizeDMRequest struct {
	ToUserUUID   string `json:"to_user_uuid"`
	MessageCount int    `json:"message_count,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Location     string `json:"location,omitempty"`
	LocalTime    string `json:"local_time,omitempty"`
}

// SummarizeGroupRequest is the request body for POST /ai/summarize/group.
type SummarizeGroupRequest struct {
	ChatGrpID    string `json:"chat_grp_id"`
	MessageCount int    `json:"message_count,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	Location     string `json:"location,omitempty"`
	LocalTime    string `json:"local_time,omitempty"`
}

// SummarizeResponse is returned by the summarize endpoint.
type SummarizeResponse struct {
	Summary      string `json:"summary"`
	MessageCount int    `json:"message_count"`
	ChannelName  string `json:"channel_name,omitempty"`
	Provider     string `json:"provider"`
}

// CatchUpRequest is the request body for POST /ai/catch-up. The scope is one
// of: a channel (channel_uuid), a DM (to_user_uuid → grouping id resolved
// server-side), a group chat (chat_grp_id), or workspace-wide (none set).
type CatchUpRequest struct {
	ScopeType   string `json:"scope_type"` // "channel" | "chat" | "workspace"
	ChannelUUID string `json:"channel_uuid,omitempty"`
	ChatGrpID   string `json:"chat_grp_id,omitempty"`
	ToUserUUID  string `json:"to_user_uuid,omitempty"` // DM peer; grouping id derived server-side
	Timezone    string `json:"timezone,omitempty"`
	Location    string `json:"location,omitempty"`
	LocalTime   string `json:"local_time,omitempty"`
}

// CatchUpResponse powers the "Catch me up" surface: an AI summary of just
// what the user missed since they last looked at a scope. Enabled=false →
// AI/feature off; HasUnread=false → nothing missed, so the FE hides it.
type CatchUpResponse struct {
	Enabled      bool   `json:"enabled"`
	HasUnread    bool   `json:"has_unread"`
	Summary      string `json:"summary"`
	MessageCount int    `json:"message_count"`
	ScopeType    string `json:"scope_type"`
	ScopeName    string `json:"scope_name,omitempty"`
	SinceISO     string `json:"since,omitempty"` // unread boundary, RFC3339
	Provider     string `json:"provider,omitempty"`

	// ScopesAllowed is how many conversations the recap was PERMITTED to read.
	//
	// Catch-up is the most-demoed AI surface in the product and, until this
	// field, the least honest one about what it is: a permission-filtered
	// read. The retrieval has always been confined to the reader's own
	// memberships, but the recap arrived looking like the assistant had read
	// the workspace, so the one thing OneCamp sells as evidence was the one
	// thing the busiest AI surface never mentioned.
	//
	// It is a count and not a list on purpose. Naming the conversations tells
	// the reader nothing they cannot already see in their own sidebar, and the
	// inverse — what was NOT read — must never be named, because the existence
	// of a private channel is itself something a non-member should not learn.
	ScopesAllowed int `json:"scopes_allowed,omitempty"`
}

// AnalyzeImageRequest asks the vision model to describe an image attachment.
// All identifiers come from the FE (the rendered message), never the user:
// obj_uuid is the attachment's object uuid; src_key + src_ref identify the
// parent (src_ref is the same id the FE uses to fetch the media - channel
// uuid, the other user's uuid for a DM, the group id, or the doc uuid). The
// server resolves src_ref to the real src_value and enforces access.
type AnalyzeImageRequest struct {
	ObjUuid string `json:"obj_uuid"`
	SrcKey  string `json:"src_key"`
	SrcRef  string `json:"src_ref"`
	Prompt  string `json:"prompt,omitempty"` // optional user question about the image
}

// AskAIRequest is the request body for POST /ai/ask.
type AskAIRequest struct {
	Question  string `json:"question"`
	SessionID string `json:"session_id,omitempty"` // optional: continue existing conversation
	Timezone  string `json:"timezone,omitempty"`   // e.g. "Asia/Kolkata"
	Location  string `json:"location,omitempty"`   // e.g. "Mumbai, India"
	LocalTime string `json:"local_time,omitempty"` // e.g. "2026-03-26T19:28:30+05:30"
}

// AnalyzeCodeRequest asks the code-aware agent to analyse a bug/issue against a
// linked GitHub repo and propose a fix. owner/repo identify the repo; title and
// body carry the issue text (or a pasted error/stack trace); ref is an optional
// branch/sha (empty = the repo default branch).
type AnalyzeCodeRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Ref   string `json:"ref,omitempty"`
	// Deep widens the analysis to look at more files for this one run (the
	// member's "analyze deeper" action when a first pass was partial).
	Deep bool `json:"deep,omitempty"`
}

// AnalyzeCodeResponse returns the agent's root-cause + proposed patch and the
// files it grounded the analysis in.
type AnalyzeCodeResponse struct {
	Answer          string   `json:"answer"`
	FilesConsidered []string `json:"files_considered"`
	// Partial is true when the repo was too large to retrieve fully, so the
	// analysis may have missed relevant code.
	Partial bool `json:"partial"`
}

// ReleaseNotesRequest drafts user-facing release notes from PRs merged on a
// repo in the last `days`.
type ReleaseNotesRequest struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Days  int    `json:"days,omitempty"`
}

// ReleaseNotesResponse returns the drafted markdown and how it was scoped.
type ReleaseNotesResponse struct {
	Notes   string `json:"notes"`
	PRCount int    `json:"pr_count"`
	Days    int    `json:"days"`
}

// SocialPostsRequest drafts platform-tailored social copy for a topic.
type SocialPostsRequest struct {
	Topic     string   `json:"topic"`
	Platforms []string `json:"platforms,omitempty"`
}

// SocialPostView is one drafted social variant.
type SocialPostView struct {
	Platform string `json:"platform"`
	Label    string `json:"label"`
	Content  string `json:"content"`
}

// SourceRef links an AI answer back to original content.
type SourceRef struct {
	ContentType string  `json:"content_type"` // "post", "chat", "doc", "task", "comment"
	ContentUUID string  `json:"content_uuid"`
	ChannelUUID string  `json:"channel_uuid,omitempty"`
	ChannelName string  `json:"channel_name,omitempty"`
	Snippet     string  `json:"snippet,omitempty"`
	Score       float64 `json:"score,omitempty"`

	// Chat routing fields — let the FE deep-link a chat/DM source back to the
	// right conversation. ChatGrpID is a 32-char id for group chats; for DMs
	// it contains a space and the FE derives the other participant from
	// ChatByUserID/ChatToUserID.
	ChatGrpID    string `json:"chat_grp_id,omitempty"`
	ChatByUserID string `json:"chat_by_user_id,omitempty"`
	ChatToUserID string `json:"chat_to_user_id,omitempty"`

	// Parent FKs — let a "comment" source deep-link to the parent it belongs
	// to (post in a channel, task, or doc). Empty for non-comments.
	PostUUID string `json:"post_uuid,omitempty"`
	TaskUUID string `json:"task_uuid,omitempty"`
	DocUUID  string `json:"doc_uuid,omitempty"`
}

// ProposedAction represents a workspace action the AI suggests.
type ProposedAction struct {
	ToolName    string            `json:"tool_name"`
	Params      map[string]string `json:"params"`
	Description string            `json:"description"` // human-readable summary
}

// AskAIResponse is returned by the AI Q&A endpoint.
type AskAIResponse struct {
	Answer          string           `json:"answer"`
	Sources         []SourceRef      `json:"sources,omitempty"`
	ProposedActions []ProposedAction `json:"proposed_actions,omitempty"`
	SessionID       string           `json:"session_id"` // always returned for continuation
	Provider        string           `json:"provider"`
}

// ExecuteActionRequest is the request body for POST /ai/action/execute.
type ExecuteActionRequest struct {
	ToolName  string            `json:"tool_name"`
	Params    map[string]string `json:"params"`
	Timezone  string            `json:"timezone,omitempty"`
	Location  string            `json:"location,omitempty"`
	LocalTime string            `json:"local_time,omitempty"`
}

// ExecuteActionResponse is returned after executing an action.
type ExecuteActionResponse struct {
	Success    bool              `json:"success"`
	Message    string            `json:"message"`
	ResultUUID string            `json:"result_uuid,omitempty"`
	ActionData map[string]string `json:"action_data,omitempty"` // Tool-specific metadata for FE state updates
	Provider   string            `json:"provider"`
}

// AIStatusResponse is returned by GET /ai/status.
type AIStatusResponse struct {
	Enabled            bool   `json:"enabled"`
	Provider           string `json:"provider"`
	Model              string `json:"model"`
	EmbeddingModel     string `json:"embedding_model"`
	CircuitState       string `json:"circuit_state"`
	RateLimitRemaining int    `json:"rate_limit_remaining"`
	// WebSearchEnabled tells the FE whether the assistant can search the web
	// (an admin has configured + enabled a provider).
	WebSearchEnabled bool `json:"web_search_enabled"`
	// SandboxEnabled tells the FE whether the code-analysis sandbox is on, so
	// the agent builder shows/hides the run_analysis tool.
	SandboxEnabled bool `json:"sandbox_enabled"`
	// CodePREnabled tells the FE whether the agent code-PR feature is on (admin
	// enabled it + a coding runner is configured), so the agent builder
	// shows/hides the code_pr tool. Without this the tool exists on the server
	// but can never be granted to an agent from the UI.
	CodePREnabled bool `json:"code_pr_enabled"`
}

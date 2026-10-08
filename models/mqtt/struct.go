package models

import (
	"encoding/json"
	"time"

	models "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	TYPE_CREATE = iota
	TYPE_UPDATE
	TYPE_DELETE
)

const (
	MESSAGE_POST = iota
	MESSAGE_POST_REACTION
	MESSAGE_POST_COMMENT_REACTION
	MESSAGE_POST_TYPING
	MESSAGE_CHAT
	MESSAGE_CHAT_REACTION
	MESSAGE_CHAT_COMMENT_REACTION
	MESSAGE_CHAT_TYPING
	MESSAGE_POST_COMMENT
	MESSAGE_CHAT_COMMENT
	MESSAGE_USER_EMOJI_STATUS
	MESSAGE_USER_STATUS
	MESSAGE_USER_DEVICE
	MESSAGE_TASK_COMMENT_REACTION
	MESSAGE_TASK_COMMENT
	MESSAGE_DOC_COMMENT
	MESSAGE_DOC_COMMENT_REACTION
	MESSAGE_ACTIVITY
	MESSAGE_CHANNEL_CALL
	MESSAGE_CHAT_CALL
	MESSAGE_GITHUB_SYNC
	MESSAGE_ARCHIVE_JOB_STATUS
	MESSAGE_SLACK_IMPORT_PROGRESS
	MESSAGE_COMMAND_EPHEMERAL
	MESSAGE_AI_NUDGE
	// MESSAGE_CHANNEL_UPDATE notifies channel members that the channel's
	// metadata changed (post policy / archive / name / privacy / membership /
	// moderators) so their cached channel state revalidates live instead of
	// after a manual refresh. Numeric codes are shared with the FE
	// MqttMessageType enum, so only ever APPEND new entries below — never
	// reorder or insert, which would shift existing codes.
	MESSAGE_CHANNEL_UPDATE
	// MESSAGE_TABLE_ROW notifies viewers of a data table that a row was
	// created/updated/deleted, so open grid/board/calendar views update live.
	MESSAGE_TABLE_ROW
	// MESSAGE_AI_PENDING_ACTION notifies the requester that a durable AI write
	// approval was created or resolved, so the in-thread Approve/Deny card
	// appears/updates live (and disappears once resolved on another tab).
	MESSAGE_AI_PENDING_ACTION
	// MESSAGE_AI_AGENT_WORK notifies a surface (the channel thread, chat or task
	// an agent is working on) and the people involved that a durable agent job
	// changed state — started, was asked to stop, finished. It is what lets the
	// in-thread "working… / Stop" strip be live WITHOUT polling: without it every
	// client watching a thread had to ask the API on a timer, which on an
	// installed PWA means waking a phone to re-read a list that rarely changes.
	MESSAGE_AI_AGENT_WORK
	// MESSAGE_SAVED_ITEM_DUE tells a member that something they saved for later
	// has come due, so an open app can say so and move it to the top of Later.
	// MUST stay last (append-only rule above).
	MESSAGE_SAVED_ITEM_DUE
	// A poll's votes or state changed; clients refetch that poll.
	MESSAGE_POLL_UPDATE
	// A member's scheduled message was sent, failed, or changed; an open app
	// refreshes its scheduled list (and the conversation, when it sent).
	MESSAGE_SCHEDULED_MESSAGE
	// A task's dates changed (a timeline move, a panel edit, a dependency
	// moving it along); the project's open boards, lists and timelines show it.
	MESSAGE_TASK_DATES
	// A task's value of a custom field changed; the project's open lists,
	// boards and panels show it.
	MESSAGE_TASK_FIELD
	// Someone in a DM or group chat has seen it up to a time (MqttChatSeen),
	// sent to each other person there who'd see the read receipt.
	MESSAGE_CHAT_SEEN
)

const (
	MESSAGE_ACTIVITY_MENTION  = "MENTION"
	MESSAGE_ACTIVITY_COMMENT  = "COMMENT"
	MESSAGE_ACTIVITY_REACTION = "REACTION"
)

const (
	MESSAGE_CALL_ACTIVE   = true
	MESSAGE_CALL_INACTIVE = false
)

type MqttPost struct {
	Type             int8                       `json:"type"`
	PostUuid         string                     `json:"post_uuid,omitempty"`
	PostHtmlText     string                     `json:"post_html_text,omitempty"`
	PostByUserUuid   string                     `json:"user_uuid,omitempty"`
	PostChannelUuid  string                     `json:"post_channel_uuid,omitempty"`
	PostCreatedAt    *time.Time                 `json:"post_created_at,omitempty"`
	PostUpdatedAt    *time.Time                 `json:"post_updated_at,omitempty"`
	PostFwdPost      *models.DgraphPost         `json:"post_fwd_msg_post,omitempty"`
	PostFwdChat      *models.DgraphChat         `json:"post_fwd_msg_chat,omitempty"`
	PostReplyTo      *models.DgraphPost         `json:"post_reply_to,omitempty"`
	PostByProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	PostByUserName   string                     `json:"user_full_name,omitempty"`
	PostAttachments  []*models.DgraphAttachment `json:"post_attachments,omitempty"`
	// PostByIsBot marks an AI-authored post so the FE renders the "AI" badge
	// live (matching the persisted read path). Append-only field.
	PostByIsBot bool `json:"is_bot,omitempty"`
}

type MqttChannelCall struct {
	CallActive  bool   `json:"call_status"`
	ChannelUUID string `json:"channel_uuid,omitempty"`
}

// MqttChannelUpdate is published on a channel's message topic whenever the
// channel's metadata changes (announcement/post-policy toggle, archive /
// unarchive, name / privacy / about edit, member added / removed, moderator
// added / removed). Subscribers revalidate their cached channel state so the
// composer, header and lists reflect the change without a page refresh.
//
// Action is advisory (for logging/UX); the FE revalidates channel state on any
// value, so adding new actions never requires a coordinated FE release.
type MqttChannelUpdate struct {
	Type        int8   `json:"type"`
	ChannelUUID string `json:"channel_uuid,omitempty"`
	Action      string `json:"action,omitempty"`
}

type MqttChatCall struct {
	CallActive bool   `json:"call_status"`
	GrpId      string `json:"grpId,omitempty"`
}

type MqttChat struct {
	Type             int8                       `json:"type"`
	ChatUuid         string                     `json:"chat_uuid,omitempty"`
	ChatHtmlText     string                     `json:"chat_html_text,omitempty"`
	ChatByUserUuid   string                     `json:"user_uuid,omitempty"`
	ChatCreatedAt    *time.Time                 `json:"chat_created_at,omitempty"`
	ChatUpdatedAt    *time.Time                 `json:"chat_updated_at,omitempty"`
	ChatByProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	ChatByUserName   string                     `json:"user_full_name,omitempty"`
	ChatAttachments  []*models.DgraphAttachment `json:"chat_attachments,omitempty"`
	ChatGrpId        string                     `json:"chat_grp_id,omitempty"`
	ChatFwdPost      *models.DgraphPost         `json:"chat_fwd_msg_post,omitempty"`
	ChatFwdChat      *models.DgraphChat         `json:"chat_fwd_msg_chat,omitempty"`
	ChatReplyTo      *models.DgraphChat         `json:"chat_reply_to,omitempty"`
	// ChatByIsBot marks an AI-authored chat so the FE renders the "AI" badge
	// live (matching the persisted read path). Append-only field.
	ChatByIsBot bool `json:"is_bot,omitempty"`
}

type MqttChatReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	ChatUuid        string `json:"chat_uuid,omitempty"`
	ChatGrpId       string `json:"chat_grp_id,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

type MqttChatCommentReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	ChatUuid        string `json:"message_uuid,omitempty"`
	CommentUuid     string `json:"comment_uuid,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

type MqttChatTyping struct {
	UserUUID    string `json:"user_uuid,omitempty"`
	UserProfile string `json:"user_profile,omitempty"`
	UserName    string `json:"user_name,omitempty"`
	ChatGrpId   string `json:"chat_grp_id,omitempty"`
}

type MqttPostReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	PostUuid        string `json:"post_uuid,omitempty"`
	ChannelUuid     string `json:"channel_id,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

type MqttPostCommentReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	PostUuid        string `json:"post_uuid,omitempty"`
	CommentUuid     string `json:"comment_uuid,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

type MqttDocCommentReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	DocUuid         string `json:"doc_uuid,omitempty"`
	CommentUuid     string `json:"comment_uuid,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

// MqttTaskDates is a task's new dates, sent to its project's members. An
// unset date is "". By is who changed them. Every app applies it, the
// mover's included: their other tabs need it too.
type MqttTaskDates struct {
	TaskUuid    string `json:"task_uuid"`
	ProjectUuid string `json:"project_uuid"`
	StartDate   string `json:"task_start_date"`
	DueDate     string `json:"task_due_date"`
	By          string `json:"by"`
}

// MqttChatSeen is a read receipt: UserUuid has seen the conversation
// ChatGrpId up to SeenAt.
type MqttChatSeen struct {
	ChatGrpId string    `json:"chat_grp_id"`
	UserUuid  string    `json:"user_uuid"`
	SeenAt    time.Time `json:"seen_at"`
}

// MqttTaskField is a task's value of one of its project's custom fields,
// null once it's taken off, sent to the project's members. By is who set it.
type MqttTaskField struct {
	TaskUuid    string          `json:"task_uuid"`
	ProjectUuid string          `json:"project_uuid"`
	FieldID     string          `json:"field_id"`
	Value       json.RawMessage `json:"value"`
	By          string          `json:"by"`
}

type MqttTaskCommentReaction struct {
	Type            int8   `json:"type"`
	EmojiReactionId string `json:"reaction_emoji_id,omitempty"`
	AddedByUuid     string `json:"user_uuid,omitempty"`
	AddedByUserName string `json:"user_name,omitempty"`
	TaskUuid        string `json:"task_uuid,omitempty"`
	CommentUuid     string `json:"comment_uuid,omitempty"`
	ReactionUuid    string `json:"reaction_id,omitempty"`
}

type MqttUserEmojiStatus struct {
	Type        int8                          `json:"type"`
	UserUuid    string                        `json:"user_uuid,omitempty"`
	EmojiStatus *models.DgraphUserStatusEmoji `json:"user_emoji_status,omitempty"`
}

type MqttUserStatus struct {
	Type     int8   `json:"type"`
	UserUuid string `json:"user_uuid,omitempty"`
	Status   string `json:"user_status,omitempty"`
}

type MqttUserDevice struct {
	Type     int8   `json:"type"`
	UserUuid string `json:"user_uuid,omitempty"`
	Device   int    `json:"user_device_connected,omitempty"`
}

type MqttChatComment struct {
	Type           int8                       `json:"type"`
	ChatUuid       string                     `json:"messaage_id,omitempty"`
	ChatGrpId      string                     `json:"chat_grp_id,omitempty"`
	ChatCreatedAt  *time.Time                 `json:"created_at,omitempty"`
	HTMLText       string                     `json:"body_text,omitempty"`
	CommentUuid    string                     `json:"comment_uuid,omitempty"`
	UserUuid       string                     `json:"user_uuid,omitempty"`
	UserName       string                     `json:"user_name,omitempty"`
	UserProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	Attachments    []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
	// IsBot marks a comment authored by an AI teammate so the FE renders an
	// "AI" badge, matching how bot messages are badged.
	IsBot bool `json:"is_bot,omitempty"`
}

type MqttTaskComment struct {
	Type           int8                       `json:"type"`
	CommentUuid    string                     `json:"comment_uuid,omitempty"`
	TaskUuid       string                     `json:"task_id,omitempty"`
	UserUuid       string                     `json:"user_uuid,omitempty"`
	UserName       string                     `json:"user_name,omitempty"`
	UserProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	CreatedAt      *time.Time                 `json:"created_at,omitempty"`
	UpdatedAt      *time.Time                 `json:"updated_at,omitempty"`
	HTMLText       string                     `json:"body_text,omitempty"`
	Attachments    []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
}

type MqttPostComment struct {
	Type           int8                       `json:"type"`
	CommentUuid    string                     `json:"comment_uuid,omitempty"`
	PostUuid       string                     `json:"post_id,omitempty"`
	UserUuid       string                     `json:"user_uuid,omitempty"`
	UserName       string                     `json:"user_name,omitempty"`
	UserProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	CreatedAt      *time.Time                 `json:"created_at,omitempty"`
	UpdatedAt      *time.Time                 `json:"updated_at,omitempty"`
	HTMLText       string                     `json:"body_text,omitempty"`
	ChannelUuid    string                     `json:"channel_id,omitempty"`
	Attachments    []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
	// IsBot marks a comment authored by an AI teammate (agent/coworker) so the
	// FE renders an "AI" badge, matching how bot posts are badged.
	IsBot bool `json:"is_bot,omitempty"`
}

type MqttDocComment struct {
	Type           int8                       `json:"type"`
	CommentUuid    string                     `json:"comment_uuid,omitempty"`
	DocUuid        string                     `json:"doc_id,omitempty"`
	UserUuid       string                     `json:"user_uuid,omitempty"`
	UserName       string                     `json:"user_name,omitempty"`
	UserProfileKey *string                    `json:"user_profile_object_key,omitempty"`
	CreatedAt      *time.Time                 `json:"created_at,omitempty"`
	UpdatedAt      *time.Time                 `json:"updated_at,omitempty"`
	HTMLText       string                     `json:"body_text,omitempty"`
	Attachments    []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
	// IsGuest marks a comment authored by an external guest (no member). The FE
	// renders a "Guest" badge and never resolves the author to a member profile.
	IsGuest bool `json:"is_guest,omitempty"`
}

type MqttChannelTyping struct {
	UserUUID    string `json:"user_uuid,omitempty"`
	UserProfile string `json:"user_profile,omitempty"`
	UserName    string `json:"user_name,omitempty"`
	ChannelUuid string `json:"channel_uuid,omitempty"`
}

type MqttActivity struct {
	UserUuid string      `json:"user_uuid,omitempty"`
	Activity interface{} `json:"activity,omitempty"`
}

type MqttGitHubSync struct {
	TaskUuid    string                 `json:"task_uuid,omitempty"`
	SyncType    string                 `json:"sync_type,omitempty"` // comment, status, name, description, assignee, label, pr_state, check_run, etc.
	ProjectUuid string                 `json:"project_uuid,omitempty"`
	Payload     map[string]interface{} `json:"payload,omitempty"` // carries the actual changed field values for Redux updates
}

// MqttArchiveJobStatus carries archive job lifecycle changes to admin clients.
// Published on the admin broadcast topic whenever a job transitions state
// (created → running → completed/failed) so the admin panel can update
// without polling.
type MqttArchiveJobStatus struct {
	JobId          string `json:"job_id,omitempty"`
	EntityType     string `json:"entity_type,omitempty"`
	Status         string `json:"status,omitempty"` // "pending" | "running" | "completed" | "failed" | "cancelled"
	ItemsProcessed int    `json:"items_processed,omitempty"`
	ItemsArchived  int    `json:"items_archived,omitempty"`
	ItemsFailed    int    `json:"items_failed,omitempty"`
	ErrorMessage   string `json:"error_message,omitempty"`
}

type Message struct {
	Type int8        `json:"type"`
	Data interface{} `json:"data,omitempty"`
}

// MqttNudge is published to a user's activity topic when the proactive-nudge
// engine creates (or refreshes) a nudge for them, so the bell/badge updates in
// real time without a poll. Action ∈ "new" | "cleared".
type MqttNudge struct {
	Action    string `json:"action"`
	NudgeID   string `json:"nudge_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"body,omitempty"`
	CTAURL    string `json:"cta_url,omitempty"`
	CTAText   string `json:"cta_text,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	OpenCount int    `json:"open_count"`
	CreatedAt string `json:"created_at,omitempty"`
}

// MqttPendingAction is published to a user's activity topic when a durable AI
// write approval is created or resolved, so the in-thread Approve/Deny card
// renders / updates / disappears live across the user's open tabs (the durable
// record is the source of truth; this is just the realtime nudge). Action ∈
// "created" | "resolved". On "resolved", Status carries the terminal state
// (executed | failed | rejected | expired) and Result/Error the outcome.
type MqttPendingAction struct {
	Action      string `json:"action"`
	ID          string `json:"id,omitempty"`
	SurfaceType string `json:"surface_type,omitempty"`
	SurfaceID   string `json:"surface_id,omitempty"`
	ToolName    string `json:"tool_name,omitempty"`
	Description string `json:"description,omitempty"`
	// Destructive flags an irreversible/high-risk write so the live card can
	// warn the approver (derived from the tool registry). Append-only field.
	Destructive bool   `json:"destructive,omitempty"`
	Status      string `json:"status,omitempty"`
	Result      string `json:"result,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// MqttSlackImportProgress is published periodically (throttled) while a
// slack import job is running, plus once at every lifecycle transition.
// Subscribers: admin clients on the admin broadcast topic.
type MqttSlackImportProgress struct {
	JobId              string `json:"job_id,omitempty"`
	SlackWorkspaceName string `json:"slack_workspace_name,omitempty"`
	Status             string `json:"status,omitempty"` // pending|running|completed|...
	Stage              string `json:"stage,omitempty"`  // extract|users|channels|messages|threads|files|reactions|finalize
	ChunksTotal        int    `json:"chunks_total,omitempty"`
	ChunksDone         int    `json:"chunks_done,omitempty"`
	ChunksFailed       int    `json:"chunks_failed,omitempty"`
	ItemsImported      int    `json:"items_imported,omitempty"`
	ErrorsTotal        int    `json:"errors_total,omitempty"`
	ErrorMessage       string `json:"error_message,omitempty"`
}

// MqttAgentWork is published when a durable AI-agent job changes state, so the
// surface the agent is working on can show (and stop) it live instead of asking
// the API on a timer.
//
// Payload discipline: it carries only what the in-thread strip already shows to
// anyone who can see that surface — which agent, what state, whether a stop is
// pending. It deliberately does NOT carry whether the receiver may stop the job
// (that is per-person, re-checked server-side) or anything about the work's
// content, so the same message is safe to broadcast to a channel/project topic.
//
// Subscribers: the surface's message topic (channel / chat / project) plus the
// activity topics of the people involved in the job.
type MqttAgentWork struct {
	// EntityID is the surface entity the work is attached to (channel post uuid,
	// chat message uuid, or task uuid) — how a client matches it to what it shows.
	EntityID string `json:"entity_id,omitempty"`
	// TaskID is the durable job's id, so a client can update the row it already
	// has instead of re-reading the list.
	TaskID    string `json:"task_id,omitempty"`
	AgentId   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	// State is the coarse user-facing state: queued | working | blocked |
	// stopping | stopped | done. A client that doesn't recognise a value should
	// re-read rather than guess.
	State string `json:"state,omitempty"`
	// Open reports whether the job is still somebody's problem. False means the
	// row can simply be dropped — no follow-up request needed.
	Open bool `json:"open"`
	// UpdatedAt is when the job last moved (RFC3339), for ordering.
	UpdatedAt string `json:"updated_at,omitempty"`
}

// MqttPollUpdate says a poll in a channel changed. It carries only the id: a
// client refetches the poll, which answers with that reader's own choices.
type MqttPollUpdate struct {
	Type        int8   `json:"type"`
	PollUUID    string `json:"poll_uuid"`
	ChannelUUID string `json:"channel_uuid"`
}

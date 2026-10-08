package models

import (
	"fmt"
	"time"
)

const (
	USER_OPT_STATUS_ONLINE  = "online"
	USER_OPT_STATUS_OFFLINE = "offline"
)

type DgraphTranscript struct {
	Uid       string      `json:"uid,omitempty"`
	DType     []string    `json:"dgraph.type,omitempty"`
	From      *DgraphUser `json:"transcript_from,omitempty"`
	Text      string      `json:"transcript_text,omitempty"`
	TimeStamp *int64      `json:"transcript_timestamp,omitempty"`
	// OffsetMs is the utterance start measured in milliseconds from recording
	// start, computed in the SAME clock domain as the producer (the speaker's
	// browser in frontend mode, the agent in backend mode). Storing the offset
	// instead of relying on (absolute_ts − recording_start) at playback time
	// removes cross-clock skew between the browser/agent and the LiveKit egress
	// server. Nil for legacy rows (playback falls back to the subtraction).
	OffsetMs *int64 `json:"transcript_offset_ms,omitempty"`
}

type DgraphRecording struct {
	Uid                string              `json:"uid,omitempty"`
	DType              []string            `json:"dgraph.type,omitempty"`
	EgressId           string              `json:"recording_egress_id,omitempty"`
	StartedAt          *time.Time          `json:"recording_stared_at,omitempty"`
	EndedAt            *time.Time          `json:"recording_ended_at,omitempty"`
	Duration           float64             `json:"recording_duration,omitempty"`
	ObjectKey          string              `json:"recording_obj_key,omitempty"`
	TranscriptCount    uint64              `json:"recording_transcript_count,omitempty"`
	Transcript         []*DgraphTranscript `json:"recording_transcript,omitempty"`
	RecordingSize      int64               `json:"recording_size,omitempty"`
	RecordingStartedBy *DgraphUser         `json:"recording_started_by,omitempty"`
	Channel            *DgraphChannel      `json:"recording_channel,omitempty"`
	Dm                 *DgraphDm           `json:"recording_dm,omitempty"`
	DeletedAt          *time.Time          `json:"recording_deleted_at,omitempty"`

	// TranscriptOnly marks a node that exists ONLY to hold the transcript of a
	// call nobody recorded. It has no media, no duration and nothing to play.
	//
	// The node type is shared with real recordings because the transcript edges,
	// the channel/DM linking and the read path are identical, and duplicating
	// all three to express "same thing without a video" would be worse. What
	// separates them is this flag, and every surface that lists recordings for a
	// human filters it out.
	//
	// Absence means a real recording, so nothing existing needs backfilling.
	TranscriptOnly bool `json:"recording_transcript_only,omitempty"`
}

type DgraphComment struct {
	Uid            string              `json:"uid,omitempty"`
	Uuid           string              `json:"comment_uuid,omitempty"`
	Text           string              `json:"comment_text,omitempty"`
	Attachments    []*DgraphAttachment `json:"comment_attachments,omitempty"`
	Reactions      []*DgraphReaction   `json:"comment_reactions,omitempty"`
	Mentions       *DgraphMentions     `json:"comment_mentions,omitempty"`
	Post           *DgraphPost         `json:"comment_post,omitempty"`
	Doc            *DgraphDoc          `json:"comment_doc,omitempty"`
	Board          *DgraphBoard        `json:"comment_board,omitempty"`
	ChatGroupingId string              `json:"comment_chat_grouping_id,omitempty"`
	Chat           *DgraphChat         `json:"comment_chat,omitempty"`
	Task           *DgraphTask         `json:"comment_task,omitempty"`
	CommentBy      *DgraphUser         `json:"comment_by,omitempty"`
	CreatedAt      *time.Time          `json:"comment_created_at,omitempty"`
	UpdatedAt      *time.Time          `json:"comment_updated_at,omitempty"`
	Replies        []*DgraphComment    `json:"comment_replies_at,omitempty"`
	DeletedAt      *time.Time          `json:"comment_deleted_at,omitempty"`
	ContentAddedBy *DgraphUser         `json:"comment_on_content_added_by,omitempty"`
	DType          []string            `json:"dgraph.type,omitempty"`
}

type ReactionCount struct {
	EmojiID string `json:"reaction_emoji_id"`
	Count   int    `json:"count"`
}

type DgraphPost struct {
	Uid            string              `json:"uid,omitempty"`
	Uuid           string              `json:"post_uuid,omitempty"`
	Text           string              `json:"post_text,omitempty"`
	ReactionCount  []ReactionCount     `json:"post_reaction_count,omitempty"` // used only in query
	MediaObj       []*DgraphAttachment `json:"post_attachments,omitempty"`
	Mentions       *DgraphMentions     `json:"post_mentions,omitempty"`
	Comments       []*DgraphComment    `json:"post_comments,omitempty"`
	Reactions      []*DgraphReaction   `json:"post_reactions,omitempty"`
	Channel        *DgraphChannel      `json:"post_channel,omitempty"`
	Doc            *DgraphDoc          `json:"post_doc,omitempty"`
	PostBy         *DgraphUser         `json:"post_by,omitempty"`
	Likes          uint64              `json:"post_likes,omitempty"`
	CommentCount   *uint64             `json:"post_comment_count,omitempty"`
	CreatedAt      *time.Time          `json:"post_created_at,omitempty"`
	UpdatedAt      *time.Time          `json:"post_updated_at,omitempty"`
	DeletedAt      *time.Time          `json:"post_deleted_at,omitempty"`
	ForwaredPost   *DgraphPost         `json:"post_fwd_msg_post,omitempty"`
	ForwarededChat *DgraphChat         `json:"post_fwd_msg_chat,omitempty"`
	// ReplyToPost is the Discord-style inline reply reference: the post this
	// post replies to, in the SAME channel. One level deep (a reply-to-a-reply
	// resolves only its immediate parent). Mirrors the forward-message edge.
	ReplyToPost *DgraphPost `json:"post_reply_to,omitempty"`
	DType       []string    `json:"dgraph.type,omitempty"`
}

type DgraphChannel struct {
	Uid              string             `json:"uid,omitempty"`
	Uuid             string             `json:"ch_uuid,omitempty"`
	Name             string             `json:"ch_name,omitempty"`
	About            string             `json:"ch_about,omitempty"`
	Moderators       []*DgraphUser      `json:"ch_moderators,omitempty"`
	Members          []*DgraphUser      `json:"ch_members,omitempty"`
	MemberCount      uint64             `json:"ch_member_count,omitempty"`
	Posts            []*DgraphPost      `json:"ch_posts,omitempty"`
	Post             *DgraphPost        `json:"ch_post,omitempty"`
	AfterPosts       []*DgraphPost      `json:"ch_post_after,omitempty"`
	BeforePosts      []*DgraphPost      `json:"ch_post_before,omitempty"`
	IsMember         uint8              `json:"ch_is_member,omitempty"`
	IsAdmin          uint8              `json:"ch_is_admin,omitempty"`
	IsUserFav        uint8              `json:"ch_is_user_fav,omitempty"`
	CreatedBy        *DgraphUser        `json:"ch_created_by,omitempty"`
	IconObj          string             `json:"ch_icon,omitempty"`
	PosterObj        string             `json:"ch_poster,omitempty"`
	IsPrivate        *bool              `json:"ch_private,omitempty"`
	PostPolicy       string             `json:"ch_post_policy,omitempty"`
	CreatedAt        *time.Time         `json:"ch_created_at,omitempty"`
	Recordings       []*DgraphRecording `json:"ch_recording,omitempty"`
	UpdatedAt        *time.Time         `json:"ch_updated_at,omitempty"`
	DeletedAt        *time.Time         `json:"ch_deleted_at,omitempty"`
	UnreadPostCount  uint64             `json:"unread_post_count,omitempty"`
	CallActive       bool               `json:"ch_call_active"`
	NotificationType string             `json:"notification_type,omitempty"` // not used in dgraph or postgres (Only used in returning for FE)
	DType            []string           `json:"dgraph.type,omitempty"`
}

type DgraphUser struct {
	Uid                 string           `json:"uid,omitempty"`
	Uuid                string           `json:"user_uuid,omitempty"`
	UserName            string           `json:"user_name,omitempty"`
	UserFullName        string           `json:"user_full_name,omitempty"`
	AppLang             string           `json:"user_app_lang,omitempty"`
	Title               string           `json:"user_job_title,omitempty"`
	Department          string           `json:"user.department,omitempty"`
	Hobbies             string           `json:"user_hobbies,omitempty"`
	EmailID             string           `json:"user_email_id,omitempty"`
	Teams               []*DgraphTeam    `json:"user_teams,omitempty"`
	Projects            []*DgraphProject `json:"user_projects,omitempty"`
	TaskCount           uint64           `json:"user_task_count,omitempty"`
	IncompleteTaskCount uint64           `json:"user_incomplete_task_count,omitempty"`
	OverdueTaskCount    uint64           `json:"user_overdue_task_count,omitempty"`
	Tasks               []*DgraphTask    `json:"user_tasks,omitempty"`
	TasksTodo           []*DgraphTask    `json:"user_tasks_todo,omitempty"`        // not used in DB only used in query
	TasksInProgress     []*DgraphTask    `json:"user_tasks_in_progress,omitempty"` // not used in DB only used in query
	TasksBacklog        []*DgraphTask    `json:"user_tasks_backlog,omitempty"`     // not used in DB only used in query
	TasksInReview       []*DgraphTask    `json:"user_tasks_in_review,omitempty"`   // not used in DB only used in query
	TasksCanceled       []*DgraphTask    `json:"user_tasks_canceled,omitempty"`    // not used in DB only used in query
	TasksDone           []*DgraphTask    `json:"user_tasks_done,omitempty"`        // not used in DB only used in query
	TasksDoneCount      int              `json:"user_tasks_done_count,omitempty"`
	TasksCanceledCount  int              `json:"user_tasks_canceled_count,omitempty"`

	Events      []*DgraphEvent   `json:"user_events,omitempty"`
	IsModerator bool             `json:"user_moderator,omitempty"` // not used in dgraph or postgres (Only used in returning channelInfo)
	Posts       []*DgraphPost    `json:"user_posts,omitempty"`
	Channels    []*DgraphChannel `json:"user_channels,omitempty"`
	FavChannels []*DgraphChannel `json:"user_fav_channels,omitempty"`
	IsAdmin     bool             `json:"user_is_admin,omitempty"`
	IsExternal  bool             `json:"is_external,omitempty"`
	IsBot       bool             `json:"is_bot,omitempty"`
	// BotKind says WHICH kind of bot this principal is. Computed at response
	// time from the email and never stored, the same shape as IsModerator
	// above. is_bot alone cannot carry it: the assistant, an agent's principal
	// and the AI-free automation bot are all is_bot, and describing them
	// identically told users an agent could do things only the assistant does.
	BotKind                  string                   `json:"user_bot_kind,omitempty"`
	Doc                      []*DgraphDoc             `json:"user_docs,omitempty"`
	Board                    []*DgraphBoard           `json:"user_boards,omitempty"`
	CreatedAt                *time.Time               `json:"user_created_at,omitempty"`
	UpdatedAt                *time.Time               `json:"user_updated_at,omitempty"`
	DeletedAt                *time.Time               `json:"user_deleted_at,omitempty"`
	ProfileKey               *string                  `json:"user_profile_object_key,omitempty"`
	DevicesConnected         *int                     `json:"user_device_connected,omitempty"`
	NotificationType         string                   `json:"notification_type,omitempty"` // not used in dgraph or postgres (Only used in returning for FE)
	TotalUnreadActivityCount uint64                   `json:"user_total_unread_activity_count,omitempty"`
	Status                   string                   `json:"user_status,omitempty"`
	StatusEmoji              []*DgraphUserStatusEmoji `json:"user_emoji_statuses,omitempty"`
	CallActive               bool                     `json:"user_call_active,omitempty"`
	DMs                      []*DgraphDm              `json:"user_dms,omitempty"`
	ThemeColor               string                   `json:"user_theme_color,omitempty"`
	ThemeMode                string                   `json:"user_theme_mode,omitempty"`
	// WeeklyCapacity is how many tasks a week they take on (the workload
	// view); nil, the default applies.
	WeeklyCapacity *int `json:"user_weekly_capacity,omitempty"`
	// WeeklyHours is how many hours a week they work (the workload in
	// hours); nil, the default applies.
	WeeklyHours *int     `json:"user_weekly_hours,omitempty"`
	DType       []string `json:"dgraph.type,omitempty"`
}

type DgraphChat struct {
	Uid              string              `json:"uid,omitempty"`
	Uuid             string              `json:"chat_uuid,omitempty"`
	DType            []string            `json:"dgraph.type,omitempty"`
	From             *DgraphUser         `json:"chat_from,omitempty"`
	To               *DgraphUser         `json:"chat_to,omitempty"`
	DM               *DgraphDm           `json:"chat_dm,omitempty"`
	CreatedAt        *time.Time          `json:"chat_created_at,omitempty"`
	UpdatedAt        *time.Time          `json:"chat_updated_at,omitempty"`
	DeletedAt        *time.Time          `json:"chat_deleted_at,omitempty"`
	Mentions         *DgraphMentions     `json:"chat_mentions,omitempty"`
	ChatCommentCount *uint64             `json:"chat_comment_count,omitempty"`
	Body             string              `json:"chat_body_text,omitempty"`
	Comments         []*DgraphComment    `json:"chat_comments,omitempty"`
	Reactions        []*DgraphReaction   `json:"chat_reactions,omitempty"`
	MediaObj         []*DgraphAttachment `json:"chat_attachments,omitempty"`
	FwdMsgPost       *DgraphPost         `json:"chat_fwd_msg_post,omitempty"`
	FwdMsgChat       *DgraphChat         `json:"chat_fwd_msg_chat,omitempty"`
	// ReplyToChat is the Discord-style inline reply reference: the chat this
	// chat replies to, in the SAME DM/group grouping. One level deep. Mirrors
	// the forward-message edge; used for both DMs and group chats.
	ReplyToChat *DgraphChat `json:"chat_reply_to,omitempty"`
}

type DgraphAttachment struct {
	Uid       string         `json:"uid,omitempty"`
	Uuid      string         `json:"attachment_uuid,omitempty"`
	FileName  string         `json:"attachment_file_name,omitempty"`
	Project   *DgraphProject `json:"attachment_project,omitempty"`
	ObjectKey string         `json:"attachment_obj_key,omitempty"`
	Type      string         `json:"attachment_type,omitempty"`
	RawType   string         `json:"attachment_raw_type,omitempty"`
	Task      *DgraphTask    `json:"attachment_task,omitempty"`
	Width     int            `json:"attachment_width,omitempty"`
	Size      int            `json:"attachment_size,omitempty"`
	Hight     int            `json:"attachment_height,omitempty"`
	Duration  float64        `json:"attachment_duration,omitempty"`
	DType     []string       `json:"dgraph.type,omitempty"`
	DeletedAt *time.Time     `json:"attachment_deleted_at,omitempty"`
	CreatedAt *time.Time     `json:"attachment_created_at,omitempty"`
	CreatedBy *DgraphUser    `json:"attachment_created_by,omitempty"`
}

type DgraphReaction struct {
	Uid            string      `json:"uid,omitempty"`
	AddedAt        *time.Time  `json:"reaction_added_at,omitempty"`
	EmojiUuid      string      `json:"reaction_emoji_id,omitempty"`
	AddedBy        *DgraphUser `json:"reaction_added_by,omitempty"`
	ContentAddedBy *DgraphUser `json:"reaction_on_content_added_by,omitempty"`
	DType          []string    `json:"dgraph.type,omitempty"`
}

type DgraphDm struct {
	Uid                 string             `json:"uid,omitempty"`
	DType               []string           `json:"dgraph.type,omitempty"`
	GroupingId          string             `json:"dm_grouping_id,omitempty"`
	Participants        []*DgraphUser      `json:"dm_participants,omitempty"`
	ParticipantIsMember uint64             `json:"dm_is_member,omitempty"`
	Chats               []*DgraphChat      `json:"dm_chats,omitempty"`
	NotificationType    string             `json:"dm_notification_type,omitempty"` // not used in dgraph or postgres (Only used in returning for FE)
	UnreadMessageCount  uint64             `json:"dm_unread,omitempty"`
	CallActive          bool               `json:"dm_call_active"`
	Recordings          []*DgraphRecording `json:"dm_recording,omitempty"`
}

type DgraphMentions struct {
	Uid         string         `json:"uid,omitempty"`
	DType       []string       `json:"dgraph.type,omitempty"`
	Mentions    []*DgraphUser  `json:"mention_users,omitempty"`
	ChatUuid    string         `json:"mention_chat_uuid,omitempty"`
	PostUuid    string         `json:"mention_post_uuid,omitempty"`
	CommentUuid string         `json:"mention_comment_uuid,omitempty"`
	TaskUuid    string         `json:"mention_task_uuid,omitempty"`
	DocUuid     string         `json:"mention_doc_uuid,omitempty"`
	Post        *DgraphPost    `json:"mention_post,omitempty"`
	Chat        *DgraphChat    `json:"mention_chat,omitempty"`
	Comment     *DgraphComment `json:"mention_comment,omitempty"`
	Task        *DgraphTask    `json:"mention_task,omitempty"`
	Doc         *DgraphDoc     `json:"mention_doc,omitempty"`
	CreatedAt   *time.Time     `json:"mention_created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"mention_updated_at,omitempty"`
}

type DgraphDoc struct {
	Uid              string           `json:"uid,omitempty"`
	Uuid             string           `json:"doc_uuid,omitempty"`
	DType            []string         `json:"dgraph.type,omitempty"`
	Title            string           `json:"doc_title,omitempty"`
	Body             string           `json:"doc_body,omitempty"`
	Snippet          string           `json:"doc_snippet,omitempty"`
	IsPrivate        *bool            `json:"doc_private,omitempty"`
	MqttTopic        string           `json:"doc_mqtt_topic,omitempty"`
	PublicComment    *bool            `json:"doc_public_comment,omitempty"`
	CreatedBy        *DgraphUser      `json:"doc_created_by,omitempty"`
	EditingUser      []*DgraphUser    `json:"doc_editing_users,omitempty"`
	ReadingUser      []*DgraphUser    `json:"doc_reading_users,omitempty"`
	CommentingUser   []*DgraphUser    `json:"doc_commenting_users,omitempty"`
	HasReadAccess    uint8            `json:"doc_read_access,omitempty"`
	HasEditAccess    uint8            `json:"doc_edit_access,omitempty"`
	HasCommentAccess uint8            `json:"doc_comment_access,omitempty"`
	Comments         []*DgraphComment `json:"doc_comments,omitempty"`
	CommentCount     uint64           `json:"doc_comment_count,omitempty"`
	CreatedAt        *time.Time       `json:"doc_created_at,omitempty"`
	UpdatedAt        *time.Time       `json:"doc_updated_at,omitempty"`
	DeletedAt        *time.Time       `json:"doc_deleted_at,omitempty"`
	LinkedTasks      []*DgraphTask    `json:"linked_tasks,omitempty"`    // query-only: tasks linking this doc
	LinkedProjects   []*DgraphProject `json:"linked_projects,omitempty"` // query-only: projects linking this doc
}

type DgraphDocList struct {
	Docs  []*DgraphDoc `json:"docs"`
	Count uint64       `json:"count"`
}

type DgraphBoard struct {
	Uid   string   `json:"uid,omitempty"`
	Uuid  string   `json:"board_uuid,omitempty"`
	DType []string `json:"dgraph.type,omitempty"`
	Title string   `json:"board_title,omitempty"`
	State string   `json:"board_state,omitempty"`
	// StateKey points to the gzipped Yjs canvas state stored in object storage
	// (MinIO). Large boards keep their blob out of dgraph; State is resolved
	// from this object at read time. board_state remains only as a legacy
	// fallback for boards saved before the object-storage migration.
	StateKey         string           `json:"board_state_key,omitempty"`
	Snippet          string           `json:"board_snippet,omitempty"`
	ThumbnailKey     string           `json:"board_thumbnail_key,omitempty"`
	IsPrivate        *bool            `json:"board_private,omitempty"`
	MqttTopic        string           `json:"board_mqtt_topic,omitempty"`
	CreatedBy        *DgraphUser      `json:"board_created_by,omitempty"`
	EditingUser      []*DgraphUser    `json:"board_editing_users,omitempty"`
	ReadingUser      []*DgraphUser    `json:"board_reading_users,omitempty"`
	CommentingUser   []*DgraphUser    `json:"board_commenting_users,omitempty"`
	HasReadAccess    uint8            `json:"board_read_access,omitempty"`
	HasEditAccess    uint8            `json:"board_edit_access,omitempty"`
	HasCommentAccess uint8            `json:"board_comment_access,omitempty"`
	CreatedAt        *time.Time       `json:"board_created_at,omitempty"`
	UpdatedAt        *time.Time       `json:"board_updated_at,omitempty"`
	DeletedAt        *time.Time       `json:"board_deleted_at,omitempty"`
	LinkedTasks      []*DgraphTask    `json:"linked_tasks,omitempty"`    // query-only: tasks linking this board
	LinkedProjects   []*DgraphProject `json:"linked_projects,omitempty"` // query-only: projects linking this board
}

type DgraphBoardList struct {
	Boards []*DgraphBoard `json:"boards"`
	Count  uint64         `json:"count"`
}

type DgraphRecordingList struct {
	Recordings []*DgraphRecording `json:"recordings"`
	Count      uint64             `json:"count"`
}

type DgraphTaskActivity struct {
	Uid       string      `json:"uid,omitempty"`
	Uuid      string      `json:"activity_uuid,omitempty"`
	DType     []string    `json:"dgraph.type,omitempty"`
	CreatedBy *DgraphUser `json:"activity_by,omitempty"`
	Type      string      `json:"activity_type,omitempty"`
	LogTime   *time.Time  `json:"activity_time,omitempty"`
	PrevState string      `json:"activity_prev_state,omitempty"`
	NextState string      `json:"activity_next_state,omitempty"`
}

type DgraphTask struct {
	Uid                   string                `json:"uid,omitempty"`
	Uuid                  string                `json:"task_uuid,omitempty"`
	DType                 []string              `json:"dgraph.type,omitempty"`
	Type                  string                `json:"task_type,omitempty"`
	Id                    string                `json:"id,omitempty"`
	Name                  string                `json:"task_name,omitempty"`
	ParentTask            *DgraphTask           `json:"task_parent_task,omitempty"`
	Status                string                `json:"task_status,omitempty"`
	Label                 *string               `json:"task_label,omitempty"`
	Priority              string                `json:"task_priority,omitempty"`
	Mentions              *DgraphMentions       `json:"task_mentions,omitempty"`
	Description           *string               `json:"task_description,omitempty"`
	Assignee              *DgraphUser           `json:"task_assignee,omitempty"`
	GoogleCalendarEventId *string               `json:"task_google_calendar_id,omitempty"`
	GitHubIssueNumber     *int                  `json:"task_github_issue_number,omitempty"`
	GitHubIssueURL        *string               `json:"task_github_issue_url,omitempty"`
	GitHubPRNumber        *int                  `json:"task_github_pr_number,omitempty"`
	GitHubPRURL           *string               `json:"task_github_pr_url,omitempty"`
	GitHubBranch          *string               `json:"task_github_branch,omitempty"`
	GitHubPRState         *string               `json:"task_github_pr_state,omitempty"`
	GitHubPRCheckStatus   *string               `json:"task_github_pr_check_status,omitempty"`
	GitHubPRReviewState   *string               `json:"task_github_pr_review_state,omitempty"`
	GitHubPRIsDraft       *bool                 `json:"task_github_pr_is_draft,omitempty"`
	SubTaskCount          uint32                `json:"task_sub_task_count,omitempty"`
	SubTasks              []*DgraphTask         `json:"task_sub_tasks,omitempty"`
	DueDate               *time.Time            `json:"task_due_date,omitempty"`
	StartDate             *time.Time            `json:"task_start_date,omitempty"`
	Project               *DgraphProject        `json:"task_project,omitempty"`
	Activity              []*DgraphTaskActivity `json:"task_activities,omitempty"`
	Team                  *DgraphTeam           `json:"task_team,omitempty"`
	Attachments           []*DgraphAttachment   `json:"task_attachments,omitempty"`
	CommentCount          uint32                `json:"task_comment_count,omitempty"`
	Comments              []*DgraphComment      `json:"task_comments,omitempty"`
	CreatedBy             *DgraphUser           `json:"task_created_by,omitempty"`
	CreatedAt             *time.Time            `json:"task_created_at,omitempty"`
	UpdatedAt             *time.Time            `json:"task_updated_at,omitempty"`
	// When the task entered its current status: "time in status" on a board.
	StatusSince  *time.Time     `json:"task_status_since,omitempty"`
	DeletedAt    *time.Time     `json:"task_deleted_at,omitempty"`
	LinkedDocs   []*DgraphDoc   `json:"linked_docs,omitempty"`
	LinkedBoards []*DgraphBoard `json:"linked_boards,omitempty"`
	// Rank orders the task within its kanban column; see business/TaskRank.
	Rank *float64 `json:"task_rank,omitempty"`
	// The tasks it waits on (finish-to-start) and, read through the reverse
	// edge, the tasks waiting on it; see business/Task/taskDependency.go.
	BlockedBy []*DgraphTask `json:"task_blocked_by,omitempty"`
	Blocks    []*DgraphTask `json:"task_blocks,omitempty"`
	// How many of the tasks it waits on are still open (TASK_BLOCKED_OPEN).
	BlockedOpen uint32 `json:"task_blocked_open,omitempty"`
	// EstimateMinutes is how long the task should take; 0 or unset is none.
	EstimateMinutes *int `json:"task_estimate_minutes,omitempty"`
	// The project's own status the task is in, if any, and its name at the
	// time it was set (kept in step on rename). Status above holds the built-in
	// category it belongs to; see business/TaskStatus.
	CustomStatus     *string `json:"task_custom_status,omitempty"`
	CustomStatusName *string `json:"task_custom_status_name,omitempty"`
}

// DgraphMemoryItem is the GraphRAG projection of a workspace_memory_items
// row. It exists so the structured knowledge layer is queryable as a GRAPH
// — "open commitments owned by @x in #channel", "decisions linked to this
// project" — joining memory to the people/channels/projects it concerns.
// Postgres remains the system of record; this is a denormalized, linked
// mirror keyed by mem_uuid (the Postgres row id).
type DgraphMemoryItem struct {
	Uid        string         `json:"uid,omitempty"`
	Uuid       string         `json:"mem_uuid,omitempty"`
	DType      []string       `json:"dgraph.type,omitempty"`
	Kind       string         `json:"mem_kind,omitempty"`
	Content    string         `json:"mem_content,omitempty"`
	Status     string         `json:"mem_status,omitempty"`
	Confidence int            `json:"mem_confidence,omitempty"`
	DueAt      *time.Time     `json:"mem_due_at,omitempty"`
	GrpID      string         `json:"mem_grp_id,omitempty"`
	Owner      *DgraphUser    `json:"mem_owner,omitempty"`
	Channel    *DgraphChannel `json:"mem_channel,omitempty"`
	Project    *DgraphProject `json:"mem_project,omitempty"`
	CreatedAt  *time.Time     `json:"mem_created_at,omitempty"`
	UpdatedAt  *time.Time     `json:"mem_updated_at,omitempty"`
}

// BoardClosedLimit is how many of a board's done (and cancelled) tasks come
// back, newest first. Those columns only grow, and a project with years of
// finished work sent all of it to every board load; the *_count fields carry
// the real totals.
const BoardClosedLimit = 200

// BoardClosedMax is the most a board may ask for with "show more".
const BoardClosedMax = 2000

// ClosedFirst is the Dgraph paging for a board's closed columns: the newest
// limit of them, or all of them when limit is 0 (the time report needs every
// task's name).
func ClosedFirst(limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit > BoardClosedMax {
		limit = BoardClosedMax
	}
	return fmt.Sprintf(", first: %d", limit)
}

type DgraphEvent struct {
	Uid                   string        `json:"uid,omitempty"`
	Uuid                  string        `json:"event_uuid,omitempty"`
	DType                 []string      `json:"dgraph.type,omitempty"`
	Title                 string        `json:"event_title,omitempty"`
	Description           string        `json:"event_description,omitempty"`
	StartTime             *time.Time    `json:"event_start_time,omitempty"`
	EndTime               *time.Time    `json:"event_end_time,omitempty"`
	CreatedBy             *DgraphUser   `json:"event_created_by,omitempty"`
	GoogleCalendarEventId *string       `json:"event_google_calendar_id,omitempty"`
	Participants          []*DgraphUser `json:"event_participants,omitempty"`
	CreatedAt             *time.Time    `json:"event_created_at,omitempty"`
	UpdatedAt             *time.Time    `json:"event_updated_at,omitempty"`
	DeletedAt             *time.Time    `json:"event_deleted_at,omitempty"`
	// IsFocus marks focus time: the creator's notifications pause while it runs.
	IsFocus *bool `json:"event_is_focus,omitempty"`
	// IsAway marks time off: the creator is away, and the workload counts
	// those working days out of their capacity.
	IsAway *bool `json:"event_is_away,omitempty"`
}

// const TASK_STATUS_TODO = "todo"
// const TASK_STATUS_INPROGRESS = "inProgress"
// const TASK_STATUS_BACKLOG = "backlog"
// const TASK_STATUS_INREVIEW = "inReview"
// const TASK_STATUS_CANCELED = "canceled"
// const TASK_STATUS_DONE = "done"

type DgraphProject struct {
	Uid              string        `json:"uid,omitempty"`
	Uuid             string        `json:"project_uuid,omitempty"`
	DType            []string      `json:"dgraph.type,omitempty"`
	Name             string        `json:"project_name,omitempty"`
	Status           string        `json:"project_status,omitempty"`
	Tasks            []*DgraphTask `json:"project_tasks,omitempty"`
	TasksTodo        []*DgraphTask `json:"project_tasks_todo,omitempty"`        // not used in DB only used in query
	TasksInProgresss []*DgraphTask `json:"project_tasks_in_progress,omitempty"` // not used in DB only used in query
	TasksBacklog     []*DgraphTask `json:"project_tasks_backlog,omitempty"`     // not used in DB only used in query
	TasksInReview    []*DgraphTask `json:"project_tasks_in_review,omitempty"`   // not used in DB only used in query
	TasksCanceled    []*DgraphTask `json:"project_tasks_canceled,omitempty"`    // not used in DB only used in query
	TasksDone        []*DgraphTask `json:"project_tasks_done,omitempty"`        // not used in DB only used in query
	// The real totals of the two closed columns, which a board loads only the newest of.
	TasksDoneCount     int                 `json:"project_tasks_done_count,omitempty"`
	TasksCanceledCount int                 `json:"project_tasks_canceled_count,omitempty"`
	Attachments        []*DgraphAttachment `json:"project_attachments,omitempty"`
	Team               *DgraphTeam         `json:"project_team,omitempty"`
	TaskCount          uint64              `json:"project_task_count,omitempty"`
	IsProjectAdmin     uint8               `json:"project_is_admin,omitempty"`  // not used in DB only used in query
	IsProjectMember    uint8               `json:"project_is_member,omitempty"` // not used in DB only used in query
	Members            []*DgraphUser       `json:"project_members,omitempty"`
	MemberCount        uint32              `json:"project_member_count,omitempty"`
	NotificationType   string              `json:"notification_type,omitempty"`
	Admins             []*DgraphUser       `json:"project_admins,omitempty"`
	CreatedBy          *DgraphUser         `json:"project_created_by,omitempty"`
	CreatedAt          *time.Time          `json:"project_created_at,omitempty"`
	UpdatedAt          *time.Time          `json:"project_updated_at,omitempty"`
	DeletedAt          *time.Time          `json:"project_deleted_at,omitempty"`
	LinkedDocs         []*DgraphDoc        `json:"linked_docs,omitempty"`
	LinkedBoards       []*DgraphBoard      `json:"linked_boards,omitempty"`
}

type DgraphTeam struct {
	Uid         string           `json:"uid,omitempty"`
	Uuid        string           `json:"team_uuid,omitempty"`
	DType       []string         `json:"dgraph.type,omitempty"`
	Projects    []*DgraphProject `json:"team_projects,omitempty"`
	Name        string           `json:"team_name,omitempty"`
	Members     []*DgraphUser    `json:"team_members,omitempty"`
	MemberCount uint32           `json:"team_member_count,omitempty"`
	IsAdmin     uint8            `json:"team_is_admin,omitempty"`
	IsMember    uint8            `json:"team_is_member,omitempty"`
	Admins      []*DgraphUser    `json:"team_admins,omitempty"`
	CreatedBy   *DgraphUser      `json:"team_created_by,omitempty"`
	CreatedAt   *time.Time       `json:"team_created_at,omitempty"`
	UpdatedAt   *time.Time       `json:"team_updated_at,omitempty"`
	DeletedAt   *time.Time       `json:"team_deleted_at,omitempty"`
}

type DgraphUserStatusEmoji struct {
	Uid       string     `json:"uid,omitempty"`
	DType     []string   `json:"dgraph.type,omitempty"`
	EmojiUuid string     `json:"status_user_emoji_id,omitempty"`
	EmojiDesc string     `json:"status_user_emoji_desc,omitempty"`
	ExpiryAt  *time.Time `json:"status_user_emoji_expiry_at,omitempty"`
	ExpiryIn  string     `json:"status_user_emoji_expiry_in,omitempty"`
}

// status_user_emoji_id
// 			status_user_emoji_desc
// 			status_user_emoji_expiry

const TASK_STATUS_TODO = "todo"
const TASK_STATUS_INPROGRESS = "inProgress"
const TASK_STATUS_BACKLOG = "backlog"
const TASK_STATUS_INREVIEW = "inReview"
const TASK_STATUS_CANCELED = "canceled"
const TASK_STATUS_DONE = "done"

var VALID_TASK_STATUSES = []string{TASK_STATUS_TODO, TASK_STATUS_INPROGRESS, TASK_STATUS_BACKLOG, TASK_STATUS_CANCELED, TASK_STATUS_DONE, TASK_STATUS_INREVIEW}

// TASK_OPEN_FILTER matches tasks still to do: not done and not canceled. A
// canceled task is finished too, so it is never overdue, never incomplete and
// never upcoming. A project's own statuses count as their category, which is
// what task_status holds, so this covers them. Every "open", "overdue" and
// "upcoming" query uses it rather than spelling its own.
const TASK_OPEN_FILTER = `not eq(task_status, "` + TASK_STATUS_DONE + `") AND not eq(task_status, "` + TASK_STATUS_CANCELED + `")`

// TASK_LIVE_FILTER keeps the tasks nobody deleted (a live task's
// task_deleted_at is the zero time).
const TASK_LIVE_FILTER = `not gt(task_deleted_at, "1970-01-01T00:00:00Z")`

// TASK_BLOCKED_OPEN counts the live, open tasks a task waits on: its
// "blocked" badge on a board, a list and the timeline.
const TASK_BLOCKED_OPEN = `task_blocked_open: count(task_blocked_by @filter(` + TASK_LIVE_FILTER + ` AND ` + TASK_OPEN_FILTER + `))`

const TASK_PRIORITY_HIGH = "high"
const TASK_PRIORITY_MEDIUM = "medium"
const TASK_PRIORITY_LOW = "low"

var VALID_TASK_PRIORITIES = []string{TASK_PRIORITY_LOW, TASK_PRIORITY_MEDIUM, TASK_PRIORITY_HIGH}

const ACTIVITY_TYPE_NAME = "nameUpdate"
const ACTIVITY_TYPE_LABEL = "labelUpdate"
const ACTIVITY_TYPE_STATUS = "statusUpdate"
const ACTIVITY_TYPE_PRIORITY = "priorityUpdate"
const ACTIVITY_TYPE_ASSIGNEE = "assigneeUpdate"
const ACTIVITY_TYPE_START_DATE = "startDateUpdate"
const ACTIVITY_TYPE_END_DATE = "endDateUpdate"
const ACTIVITY_TYPE_DESC = "descUpdate"
const ACTIVITY_TYPE_ADD_ATACHMENT = "attachmentAdd"
const ACTIVITY_TYPE_REMOVE_ATACHMENT = "attachmentRemove"
const ACTIVITY_TYPE_ADD_SUB_TASK = "subTaskAdd"
const ACTIVITY_TYPE_DELETE_SUB_TASK = "subTaskDelete"
const ACTIVITY_TYPE_UNDELETE_SUB_TASK = "subTaskUnDelete"
const ACTIVITY_TYPE_ADD_COMMENT = "commentAdd"
const ACTIVITY_TYPE_UPDATE_COMMENT = "commentUpdate"
const ACTIVITY_TYPE_DELETE_COMMENT = "commentDelete"
const ACTIVITY_TYPE_DELETE_TASK = "taskDelete"
const ACTIVITY_TYPE_UNDELETE_TASK = "taskUnDelete"
const ACTIVITY_TYPE_CREATE_TASK = "taskCreate"

// A dependency added to or taken off the task that waits: NextState (added)
// or PrevState (taken off) holds the other task's uuid.
const ACTIVITY_TYPE_ADD_DEPENDENCY = "dependencyAdd"
const ACTIVITY_TYPE_ESTIMATE = "estimate"
const ACTIVITY_TYPE_REMOVE_DEPENDENCY = "dependencyRemove"

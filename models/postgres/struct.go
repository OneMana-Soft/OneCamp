package models

import "time"

const (
	NOTIFICATION_TYPE_ALL     = "all"
	NOTIFICATION_TYPE_MENTION = "mention"
	NOTIFICATION_TYPE_BLOCK   = "block"
)

type OrganizationMembership struct {
	ID                 int64      `json:"id"`
	PublicID           string     `json:"public_id"`
	OrganizationID     int64      `json:"organization_id"`
	UserID             int64      `json:"user_id"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DiscardedAt        *time.Time `json:"discarded_at,omitempty"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	RoleName           string     `json:"role_name"`
	LatestStatusID     *int64     `json:"latest_status_id,omitempty"`
	Position           *int       `json:"position,omitempty"`
	LastViewedPostsAt  *time.Time `json:"last_viewed_posts_at,omitempty"`
	HomeLastSeenAt     *time.Time `json:"home_last_seen_at,omitempty"`
	ActivityLastSeenAt *time.Time `json:"activity_last_seen_at,omitempty"`
}

const (
	ATTACHMENT_SRC_PROJECT  = "project"
	ATTACHMENT_SRC_CHANNEL  = "channel"
	ATTACHMENT_SRC_PUBLIC   = "public"
	ATTACHMENT_SRC_CHAT     = "chat"
	ATTACHMENT_SRC_DOC      = "doc"
	ATTACHMENT_SRC_BOARD    = "board"
	ATTACHMENT_SRC_GRP_CHAT = "grpChat"
	// Added for the generic Import pipeline (Asana/Jira/Trello/Notion/
	// Todoist task attachments and task-comment attachments). Stored as
	// the same `src_key` column on the attachments row.
	ATTACHMENT_SRC_TASK    = "task"
	ATTACHMENT_SRC_COMMENT = "comment"
)

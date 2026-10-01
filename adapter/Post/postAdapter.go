package adapter

import (
	"time"

	models "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

type InputCreateOrUpdatePostInfo struct {
	HTMLText    string                     `json:"post_text_html,omitempty"`
	MediaObj    []*models.DgraphAttachment `json:"post_attachments,omitempty"`
	ChannelUuid string                     `json:"channel_id,omitempty"`
	ChannelUUID uuid.UUID
	Uuid        string `json:"post_id,omitempty"`
	// ReplyToUUID, when set, makes this post a Discord-style inline reply to
	// another post in the SAME channel. Optional and additive: an empty value
	// behaves exactly as a normal post. The business layer validates the parent
	// (exists, not deleted, same channel) before setting the reply edge.
	ReplyToUUID string `json:"reply_to_uuid,omitempty"`
}

type InputCreateOrUpdateCommentToPost struct {
	MediaObj []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
	HTMLText string                     `json:"comment_text_html,omitempty"`
	PostUuid string                     `json:"post_id,omitempty"`
	Uuid     string                     `json:"comment_id,omitempty"`
	// AlsoSendToChannel mirrors Slack's "Also send to #channel" — when true,
	// the reply is additionally posted as a top-level message in the thread's
	// channel so people not in the thread still see it.
	AlsoSendToChannel bool `json:"also_send_to_channel,omitempty"`
}

type InputUpdateReactionForPost struct {
	EmojiUuid         string `json:"reaction_emoji_id,omitempty"`
	Uuid              string `json:"post_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputUpdateReactionForCommentInPost struct {
	EmojiUuid         string `json:"reaction_emoji_id,omitempty"`
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputDeleteReactionForCommentInPost struct {
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputDeleteReactionForPostComment struct {
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputDeleteReactionForPost struct {
	Uuid              string `json:"post_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type OutputCreatePostForPost struct {
	Uuid        string    `json:"uuid,omitempty"`
	PostCreated time.Time `json:"post_created_at,omitempty"`
}

type OutputCreateCommentForTask struct {
	Uuid             string    `json:"uuid,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

type OutputCreatePostCommentForPost struct {
	Uuid             string    `json:"comment_id,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

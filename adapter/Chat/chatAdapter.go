package adapter

import (
	"time"

	models "github.com/akashc777/OneCamp/models/dgraph"
)

type ChatInfo struct {
	Uuid         string                     `json:"chat_id,omitempty"`
	MediaObjects []*models.DgraphAttachment `json:"media_attachments,omitempty"`
	TextHtml     string                     `json:"text_html,omitempty"`
	GrpUuid      string                     `json:"grp_id,omitempty"`
	ToUuid       string                     `json:"to_uuid,omitempty"`
	Participants []string                   `json:"participants,omitempty"`
	// ReplyToUuid is the optional Discord-style inline reply target: the
	// chat_uuid of a message in the SAME conversation this message replies to.
	// One level deep; validated server-side (same grouping id, not deleted).
	ReplyToUuid string `json:"reply_to_uuid,omitempty"`
}

type SearchUserName struct {
	SearchText string `json:"search_text,omitempty"`
}

type InputUpdateReactionForChat struct {
	EmojiReactionUuid string `json:"reaction_emoji_id,omitempty"`
	Uuid              string `json:"chat_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputCreateOrUpdateCommentToChat struct {
	MediaObj []*models.DgraphAttachment `json:"comment_attachments,omitempty"`
	HTMLText string                     `json:"comment_text_html,omitempty"`
	ChatUuid string                     `json:"chat_id,omitempty"`
	Uuid     string                     `json:"comment_id,omitempty"`
}

type OutputCreateChatCommentForPost struct {
	Uuid             string    `json:"comment_id,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

type InputUpdateReactionForCommentInChat struct {
	EmojiReactionUuid string `json:"reaction_emoji_id,omitempty"`
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputDmSearchText struct {
	SearchText string `json:"search_text,omitempty"`
}

type OutputCreateChatForChat struct {
	Uuid        string    `json:"uuid,omitempty"`
	ChatCreated time.Time `json:"chat_created_at,omitempty"`
}

type InputPublishTypingInChat struct {
	UserUuid string `json:"user_uuid,omitempty"`
	GrpUuid  string `json:"grp_id,omitempty"`
}

type InputAddParticipantToGroupChat struct {
	GrpUuid  string `json:"grp_id,omitempty"`
	UserUuid string `json:"user_uuid,omitempty"`
}

type UserTokenOutput struct {
	TokenString    string `json:"token"`
	AlreadyExisted bool   `json:"already_existed"`
}

type InputMakeVideoChatCall struct {
	UserUuid     string `json:"user_uuid,omitempty"`
	GrpUuid      string `json:"grp_id,omitempty"`
	AudioEnabled bool   `json:"audio_enabled,omitempty"`
	VideoEnabled bool   `json:"video_enabled,omitempty"`
}

type InputStartVideoChatCallRecording struct {
	UserUuid string `json:"user_uuid,omitempty"`
	GrpUuid  string `json:"grp_id,omitempty"`
}

package adapter

import (
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

type CreateOrUpdateDocCommentInput struct {
	UUID        string                           `json:"doc_comment_uuid,omitempty"`
	CommentBody string                           `json:"doc_comment_body,omitempty"`
	DocUuid     string                           `json:"doc_uuid,omitempty"`
	Attachments []*dgraphStruct.DgraphAttachment `json:"doc_comment_attachments,omitempty"`
}

type OutputCreateCommentForDoc struct {
	Uuid             string    `json:"comment_id,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

type OutputCreateChatCommentForPost struct {
	Uuid             string    `json:"uuid,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

type InputUpdateDocPermissions struct {
	DocId            string   `json:"doc_uuid,omitempty"`
	AddEditors       []string `json:"add_editors,omitempty"`       // User UUIDs
	RemoveEditors    []string `json:"remove_editors,omitempty"`    // User UUIDs
	AddViewers       []string `json:"add_viewers,omitempty"`       // User UUIDs
	RemoveViewers    []string `json:"remove_viewers,omitempty"`    // User UUIDs
	AddCommenters    []string `json:"add_commenters,omitempty"`    // User UUIDs
	RemoveCommenters []string `json:"remove_commenters,omitempty"` // User UUIDs
}

type InputUpdateDoc struct {
	DocId         string  `json:"doc_uuid,omitempty"`
	Body          *string `json:"doc_body,omitempty"`
	Title         *string `json:"doc_title,omitempty"`
	IsPrivate     *bool   `json:"doc_private,omitempty"`
	PublicComment *bool   `json:"doc_public_comment,omitempty"`
}

type InputDocCollabUpdate struct {
	DocUuid     string `json:"documentId"`
	Content     any    `json:"content"`
	TextContent string `json:"textContent"`
	HtmlContent string `json:"htmlContent"`
	// YjsState is the collaboration service's Yjs state for HtmlContent,
	// base64. Saved beside it (business UpdateDocFromCollab).
	YjsState string `json:"yjsState,omitempty"`
	// Contributors is the set of editor user uuids who changed the doc since the
	// last persist, used to attribute the version snapshot. May be empty.
	Contributors []string `json:"contributors,omitempty"`
}

// InputRestoreDocSnapshot restores a doc to a chosen snapshot from its history.
type InputRestoreDocSnapshot struct {
	DocId      string `json:"doc_uuid"`
	SnapshotId string `json:"snapshot_id"`
}

// InputRecordDocView records that the caller opened the doc ("Viewed by").
type InputRecordDocView struct {
	DocId string `json:"doc_uuid"`
}

type InputUpdateReactionForCommentInDoc struct {
	EmojiReactionUuid string `json:"reaction_emoji_id,omitempty"`
	CommentId         string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputSearchDocName struct {
	SearchText string `json:"search_text"`
	PageIndex  int    `json:"page_index"`
	PageSize   int    `json:"page_size"`
}

type InputCreateDoc struct {
	DocTitle   string `json:"doc_title"`
	DocPrivate bool   `json:"doc_private"`
}

type SearchInputDocUser struct {
	SearchText string `json:"searchText"`
}

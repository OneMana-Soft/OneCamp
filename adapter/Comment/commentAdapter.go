package adapter

type UpdateCommentInfo struct {
	Text      string `json:"comment_text_html,omitempty"`
	CommentID string `json:"comment_id,omitempty"`
}

type ReplyOnCommentInfo struct {
	PostID       string   `json:"post_id,omitempty"`
	MediaObjects []string `json:"media_objects,omitempty"`
	Text         string   `json:"text,omitempty"`
}

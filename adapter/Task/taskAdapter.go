package adapter

import (
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

type CreateOrUpdateTaskInput struct {
	Uuid             string                           `json:"task_uuid,omitempty"`
	TaskName         string                           `json:"task_name,omitempty"`
	TaskDescription  string                           `json:"task_description,omitempty"`
	DueDate          string                           `json:"task_due_date,omitempty"`
	StartDate        string                           `json:"task_start_date,omitempty"`
	ProjectUuid      string                           `json:"task_project_uuid,omitempty"`
	AssigneeUuid     string                           `json:"task_assignee_uuid,omitempty"`
	Label            string                           `json:"task_label,omitempty"`
	Status           string                           `json:"task_status,omitempty"`
	Priority         string                           `json:"task_priority,omitempty"`
	Attachments      []*dgraphStruct.DgraphAttachment `json:"task_attachments,omitempty"`
	AttachmentObjKey string                           `json:"task_attachment_obj_key,omitempty"`
	GitHubIssueNum   *int                             `json:"task_github_issue_number,omitempty"`
	GitHubIssueURL   *string                          `json:"task_github_issue_url,omitempty"`
	GitHubPRNum      *int                             `json:"task_github_pr_number,omitempty"`
	GitHubPRURL      *string                          `json:"task_github_pr_url,omitempty"`
	GitHubBranch     *string                          `json:"task_github_branch,omitempty"`
}

type InputUpdateReactionForCommentInTask struct {
	EmojiUuid         string `json:"reaction_emoji_id,omitempty"`
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type InputDeleteReactionForTaskComment struct {
	Uuid              string `json:"comment_id,omitempty"`
	ReactionDgraphUid string `json:"reaction_dgraph_id,omitempty"`
}

type CreateOrUpdateTaskCommentInput struct {
	Uuid           string                           `json:"task_comment_uuid,omitempty"`
	CommentBody    string                           `json:"task_comment_body,omitempty"`
	TaskUuid       string                           `json:"task_uuid,omitempty"`
	Attachments    []*dgraphStruct.DgraphAttachment `json:"task_comment_attachments,omitempty"`
	SkipGitHubSync bool                             `json:"skip_github_sync,omitempty"`
	CreatedAt      *time.Time                       `json:"created_at,omitempty"` // Override creation time (e.g. for backfill from GitHub)
}

type RemoveTaskAttachmentInput struct {
	AttachmentObjKey string `json:"attachment_obj_key,omitempty"`
	TaskUuid         string `json:"task_uuid,omitempty"`
}

type OutputCreateCommentForTask struct {
	Uuid             string    `json:"comment_id,omitempty"`
	CommentCreatedAt time.Time `json:"comment_created_at,omitempty"`
}

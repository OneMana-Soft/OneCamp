package models

type OpenSearchPost struct {
	Uuid               string  `json:"post_id,omitempty"`
	PostBody           string  `json:"post_body,omitempty"`
	PostByUserUuid     string  `json:"post_by_user_id,omitempty"`
	PostByUserFullName string  `json:"post_by_user_full_name,omitempty"`
	PostByProfile      *string `json:"post_by_profile,omitempty"`
	PostChannelUuid    string  `json:"post_ch_id,omitempty"`
	PostCreatedAt      int64   `json:"created_date,omitempty"`
	PostUpdatedAt      int64   `json:"updated_date,omitempty"`
	PostDeletedAt      *int64  `json:"deleted_date"`
	PostChannelName    string  `json:"post_ch_name,omitempty"`
}

type OpenSearchChatParticipants struct {
	Uuid       string  `json:"user_uuid,omitempty"`
	ProfileKey *string `json:"user_profile_object_key,omitempty"`
	Name       string  `json:"user_name,omitempty"`
}

type OpenSearchChat struct {
	Uuid               string                        `json:"chat_id,omitempty"`
	ChatBody           string                        `json:"chat_body,omitempty"`
	ChatByUserUuid     string                        `json:"chat_by_user_id,omitempty"`
	ChatByUserFullName string                        `json:"chat_by_user_full_name,omitempty"`
	ChatParticipants   []*OpenSearchChatParticipants `json:"chat_participants,omitempty"`
	ChatByProfile      *string                       `json:"chat_by_profile,omitempty"`
	ChatGrpId          string                        `json:"chat_grp_id,omitempty"`
	ChatCreatedAt      int64                         `json:"created_date,omitempty"`
	ChatUpdatedAt      int64                         `json:"updated_date,omitempty"`
	ChatDeletedAt      *int64                        `json:"deleted_date"`
}

type OpenSearchComment struct {
	Uuid                        string                        `json:"comment_id,omitempty"`
	CommentBody                 string                        `json:"comment_body,omitempty"`
	CommentByUserUuid           string                        `json:"comment_by_user_id,omitempty"`
	CommentByUserFullName       string                        `json:"comment_by_user_full_name,omitempty"`
	CommentByProfile            *string                       `json:"comment_by_profile,omitempty"`
	CommentChatGrpId            string                        `json:"comment_chat_grp_id,omitempty"`
	CommentChatFromUserUuid     string                        `json:"comment_chat_from_user_id,omitempty"`
	CommentChatFromUserFullName string                        `json:"comment_chat_from_user_full_name,omitempty"`
	CommentChatParticipants     []*OpenSearchChatParticipants `json:"comment_chat_participants,omitempty"`
	CommentDocUuid              string                        `json:"comment_doc_id,omitempty"`
	CommentDocTitle             string                        `json:"comment_doc_title,omitempty"`
	CommentChannelUuid          string                        `json:"comment_channel_id,omitempty"`
	CommentChannelName          string                        `json:"comment_channel_name,omitempty"`
	CommentPostUuid             string                        `json:"comment_post_id,omitempty"`
	CommentProjectUuid          string                        `json:"comment_project_id,omitempty"`
	CommentProjectName          string                        `json:"comment_project_name,omitempty"`
	CommentTaskUuid             string                        `json:"comment_task_id,omitempty"`
	CommentChatUuid             string                        `json:"comment_chat_id,omitempty"`
	CommentDocPrivate           bool                          `json:"comment_doc_private,omitempty"`
	CommentDocReadingUsers      []string                      `json:"comment_doc_reading_users,omitempty"`
	CommentDocEditingUsers      []string                      `json:"comment_doc_editing_users,omitempty"`
	CommentDocCommentingUsers   []string                      `json:"comment_doc_commenting_users,omitempty"`
	CommentDocCreatedByUserUuid string                        `json:"comment_doc_created_by_user_id,omitempty"`
	// Board comment permission fields. CommentBoardPublic is set true only for
	// a comment on a public board so the search permission clause can grant it
	// to everyone without a blanket term that would leak other comment types.
	CommentBoardUuid              string   `json:"comment_board_id,omitempty"`
	CommentBoardTitle             string   `json:"comment_board_title,omitempty"`
	CommentBoardPublic            bool     `json:"comment_board_public,omitempty"`
	CommentBoardReadingUsers      []string `json:"comment_board_reading_users,omitempty"`
	CommentBoardEditingUsers      []string `json:"comment_board_editing_users,omitempty"`
	CommentBoardCommentingUsers   []string `json:"comment_board_commenting_users,omitempty"`
	CommentBoardCreatedByUserUuid string   `json:"comment_board_created_by_user_id,omitempty"`
	CommentCreatedAt              int64    `json:"created_date,omitempty"`
	CommentUpdatedAt              int64    `json:"updated_date,omitempty"`
	CommentDeletedAt              *int64   `json:"deleted_date"`
}

type OpenSearchAttachment struct {
	Uuid                           string                        `json:"attachment_id,omitempty"`
	AttachmentByUserUuid           string                        `json:"attachment_by_user_id,omitempty"`
	AttachmentTaskUuid             string                        `json:"attachment_task_id,omitempty"`
	AttachmentProjectUuid          string                        `json:"attachment_project_id,omitempty"`
	AttachmentFileName             string                        `json:"attachment_file_name,omitempty"`
	AttachmentObjKey               string                        `json:"attachment_object_key,omitempty"`
	AttachmentByUserFullName       string                        `json:"attachment_by_user_full_name,omitempty"`
	AttachmentByProfile            *string                       `json:"attachment_by_profile,omitempty"`
	AttachmentCommentUuid          string                        `json:"attachment_comment_id,omitempty"`
	AttachmentChannelUuid          string                        `json:"attachment_channel_id,omitempty"`
	AttachmentChannelName          string                        `json:"attachment_channel_name,omitempty"`
	AttachmentProjectName          string                        `json:"attachment_project_name,omitempty"`
	AttachmentChatGrpId            string                        `json:"attachment_chat_grp_id,omitempty"`
	AttachmentPostUuid             string                        `json:"attachment_post_id,omitempty"`
	AttachmentChatUuid             string                        `json:"attachment_chat_id,omitempty"`
	AttachmentDocUuid              string                        `json:"attachment_doc_id,omitempty"`
	AttachmentDocTitle             string                        `json:"attachment_doc_title,omitempty"`
	AttachmentChatParticipants     []*OpenSearchChatParticipants `json:"attachment_chat_participants,omitempty"`
	AttachmentChatFromUserUuid     string                        `json:"attachment_chat_from_user_id,omitempty"`
	AttachmentChatFromUserFullName string                        `json:"attachment_chat_from_user_name,omitempty"`
	AttachmentDocPrivate           bool                          `json:"attachment_doc_private,omitempty"`
	AttachmentDocReadingUsers      []string                      `json:"attachment_doc_reading_users,omitempty"`
	AttachmentDocEditingUsers      []string                      `json:"attachment_doc_editing_users,omitempty"`
	AttachmentDocCommentingUsers   []string                      `json:"attachment_doc_commenting_users,omitempty"`
	AttachmentDocCreatedByUserUuid string                        `json:"attachment_doc_created_by_user_id,omitempty"`
	AttachmentCreatedAt            int64                         `json:"created_date,omitempty"`
	AttachmentDeletedAt            *int64                        `json:"deleted_date"`
}

type OpenSearchTask struct {
	Uuid                    string  `json:"task_id,omitempty"`
	TaskName                string  `json:"task_name,omitempty"`
	TaskDescription         *string `json:"task_desc,omitempty"`
	TaskAssigneeUuid        *string `json:"task_assignee_user_id,omitempty"`
	TaskAssigneeFullName    string  `json:"task_assignee_user_full_name,omitempty"`
	TaskAssigneeUserProfile *string `json:"task_assignee_profile,omitempty"`
	TaskCreatedAt           int64   `json:"created_date,omitempty"`
	TaskUpdatedAt           int64   `json:"updated_date,omitempty"`
	TaskDeletedAt           *int64  `json:"deleted_date"`
	TaskProjectUuid         string  `json:"task_project_id,omitempty"`
	TaskProjectName         string  `json:"task_project_name,omitempty"`
	TaskStatus              string  `json:"task_status,omitempty"`
	TaskPriority            string  `json:"task_priority,omitempty"`
	TaskLabel               *string `json:"task_label,omitempty"`
	TaskCreatedByUserUuid   string  `json:"task_created_by_user_id,omitempty"`
}

type OpenSearchDoc struct {
	Uuid                 string   `json:"doc_uuid,omitempty"`
	DocTitle             string   `json:"doc_title,omitempty"`
	DocBody              string   `json:"doc_body,omitempty"`
	DocSnippet           string   `json:"doc_snippet,omitempty"`
	DocCreatedByUserUuid string   `json:"doc_created_by_user_id,omitempty"`
	DocCreatedByFullName string   `json:"doc_created_by_user_full_name,omitempty"`
	DocCreatedByProfile  *string  `json:"doc_created_by_profile,omitempty"`
	DocPrivate           bool     `json:"doc_private,omitempty"`
	DocReadingUsers      []string `json:"doc_reading_users,omitempty"`
	DocEditingUsers      []string `json:"doc_editing_users,omitempty"`
	DocCommentingUsers   []string `json:"doc_commenting_users,omitempty"`
	DocCreatedAt         int64    `json:"created_date,omitempty"`
	DocUpdatedAt         int64    `json:"updated_date,omitempty"`
	DocDeletedAt         *int64   `json:"deleted_date"`
}

type OpenSearchBoard struct {
	Uuid                   string   `json:"board_uuid,omitempty"`
	BoardTitle             string   `json:"board_title,omitempty"`
	BoardSnippet           string   `json:"board_snippet,omitempty"`
	BoardCreatedByUserUuid string   `json:"board_created_by_user_id,omitempty"`
	BoardCreatedByFullName string   `json:"board_created_by_user_full_name,omitempty"`
	BoardCreatedByProfile  *string  `json:"board_created_by_profile,omitempty"`
	BoardPrivate           bool     `json:"board_private"`
	BoardReadingUsers      []string `json:"board_reading_users,omitempty"`
	BoardEditingUsers      []string `json:"board_editing_users,omitempty"`
	BoardCommentingUsers   []string `json:"board_commenting_users,omitempty"`
	BoardCreatedAt         int64    `json:"created_date,omitempty"`
	BoardUpdatedAt         int64    `json:"updated_date,omitempty"`
	BoardDeletedAt         *int64   `json:"deleted_date"`
}

type OpenSearchProject struct {
	Uuid             string `json:"project_id,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	ProjectTeamUuid  string `json:"project_team_id,omitempty"`
	ProjectTeamName  string `json:"project_team_name,omitempty"`
	ProjectCreatedAt int64  `json:"created_date,omitempty"`
	ProjectUpdatedAt int64  `json:"updated_date,omitempty"`
	ProjectDeletedAt *int64 `json:"deleted_date"`
}

type OpenSearchChannel struct {
	Uuid             string `json:"ch_id,omitempty"`
	ChannelUUID      string `json:"-"`
	ChannelName      string `json:"ch_name,omitempty"`
	ChannelCreatedAt int64  `json:"created_date,omitempty"`
	ChannelUpdatedAt int64  `json:"updated_date,omitempty"`
	ChannelDeletedAt *int64 `json:"deleted_date"`
}

type OpenSearchUser struct {
	Uuid           string  `json:"user_id,omitempty"`
	UserName       string  `json:"user_name,omitempty"`
	UserEmail      string  `json:"user_email,omitempty"`
	UserCreatedAt  int64   `json:"created_date,omitempty"`
	UserUpdatedAt  int64   `json:"updated_date,omitempty"`
	UserDeletedAt  *int64  `json:"deleted_date"`
	UserFullName   string  `json:"user_full_name,omitempty"`
	UserProfileKey *string `json:"user_profile_object_key,omitempty"`
}

type OpenSearchTeam struct {
	Uuid          string `json:"team_id,omitempty"`
	TeamName      string `json:"team_name,omitempty"`
	TeamCreatedAt int64  `json:"created_date,omitempty"`
	TeamUpdatedAt int64  `json:"updated_date,omitempty"`
	TeamDeletedAt *int64 `json:"deleted_date"`
}

type DocInfoAttachment struct {
	IndexName string `json:"_index,omitempty"`
	DocId     string `json:"_id,omitempty"`
}

type BulkActions struct {
	Create *DocInfoAttachment `json:"create,omitempty"`
	Index  *DocInfoAttachment `json:"index,omitempty"`
	Update *DocInfoAttachment `json:"update,omitempty"`
	Delete *DocInfoAttachment `json:"delete,omitempty"`
}

type BulkUpdate struct {
	Doc         interface{} `json:"doc,omitempty"`
	DocAsUpsert bool        `json:"doc_as_upsert,omitempty"`
}

type GlobalSearchOpenSearchResp struct {
	Type       string                `json:"type,omitempty"`
	Comment    *OpenSearchComment    `json:"comment,omitempty"`
	Chat       *OpenSearchChat       `json:"chat,omitempty"`
	Post       *OpenSearchPost       `json:"post,omitempty"`
	Attachment *OpenSearchAttachment `json:"attachment,omitempty"`
	Task       *OpenSearchTask       `json:"task,omitempty"`
	Doc        *OpenSearchDoc        `json:"doc,omitempty"`
	Board      *OpenSearchBoard      `json:"board,omitempty"`
	User       *OpenSearchUser       `json:"user,omitempty"`
	Project    *OpenSearchProject    `json:"project,omitempty"`
	Team       *OpenSearchTeam       `json:"team,omitempty"`
	Channel    *OpenSearchChannel    `json:"channel,omitempty"`
	Highlight  map[string][]string   `json:"highlight,omitempty"`
}

const (
	TASK_INDEX       = "tasks"
	TASK_TYPE        = "task"
	PROJECT_INDEX    = "projects"
	PROJECT_TYPE     = "project"
	TEAM_INDEX       = "teams"
	TEAM_TYPE        = "team"
	USER_INDEX       = "users"
	USER_TYPE        = "user"
	COMMENT_INDEX    = "comments"
	COMMENT_TYPE     = "comment"
	ATTACHMENT_INDEX = "attachments"
	ATTACHMENT_TYPE  = "attachment"
	DOC_INDEX        = "docs"
	DOC_TYPE         = "doc"
	BOARD_INDEX      = "boards"
	BOARD_TYPE       = "board"
	CHAT_INDEX       = "chats"
	CHAT_TYPE        = "chat"
	POST_INDEX       = "posts"
	POST_TYPE        = "post"
	CHANNEL_INDEX    = "channels"
	CHANNEL_TYPE     = "channel"
)

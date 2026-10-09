package adapter

import (
	models "github.com/akashc777/OneCamp/models/dgraph"
)

type InputEditUserProfile struct {
	UserFullName string `json:"user_full_name,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	// Handle is sent only when the person changes it; nil keeps it.
	Handle        *string `json:"user_handle,omitempty"`
	Title         string  `json:"user_job_title,omitempty"`
	Hobbies       string  `json:"user_hobbies,omitempty"`
	ProfilePicKey string  `json:"user_profile_object_key,omitempty"`
	AppLang       string  `json:"user_app_lang,omitempty"`
	Status        string  `jso:"user_status,omitempty"`
}

type InputUserNameValid struct {
	Uname string `json:"user_name,omitempty"`
}

type InputUserStatus struct {
	Status string `json:"user_status,omitempty"`
}

type InputUserTheme struct {
	ThemeColor string `json:"user_theme_color,omitempty"`
	ThemeMode  string `json:"user_theme_mode,omitempty"`
}

type InputUserFCMToken struct {
	FCMToken string `json:"fcm_token,omitempty"`
}

type InputUserChatNotification struct {
	NotificationType string `json:"notification_type,omitempty"`
	ToUserUuid       string `json:"to_user_id,omitempty"`
}

type InputUserGroupChatNotification struct {
	NotificationType string `json:"notification_type,omitempty"`
	GrpId            string `json:"grp_id,omitempty"`
}

type InputUserChannelNotification struct {
	NotificationType string `json:"notification_type,omitempty"`
	ChannelUuid      string `json:"channel_id,omitempty"`
}

type InputUserProjectNotification struct {
	NotificationType string `json:"notification_type,omitempty"`
	ProjectUuid      string `json:"project_id,omitempty"`
}

type InputUserUUID struct {
	UserUuid string `json:"user_uuid,omitempty"`
}

type AddOrRemoveAttachmentInput struct {
	ObjUuid      string   `json:"obj_uuid,omitempty"`
	SrcKey       string   `json:"src_key,omitempty"`
	SrcValue     string   `json:"src_value,omitempty"`
	ChannelUuids []string `json:"channel_uuids,omitempty"`
	ChatUuids    []string `json:"chat_uuids,omitempty"`
}

type UserAndChannelFwdMessage struct {
	Type             string               `json:"type,omitempty"`
	UserUuid         string               `json:"user_uuid,omitempty"`
	UserDgraphUid    string               `json:"user_dgraph_uid,omitempty"`
	UserName         string               `json:"user_name,omitempty"`
	UserEmail        string               `json:"user_email_id,omitempty"`
	UserProfileKey   string               `json:"user_profile_object_key,omitempty"`
	ChannelUuid      string               `json:"channel_uuid,omitempty"`
	ChannelDgraphUid string               `json:"channel_dgraph_uid,omitempty"`
	ChannelName      string               `json:"channel_name,omitempty"`
	ChatParticipants *[]models.DgraphUser `json:"chat_participants,omitempty"`
	// Group chat destination fields
	GrpId        string `json:"grp_id,omitempty"`
	GrpDgraphUid string `json:"grp_dgraph_uid,omitempty"`
	GrpName      string `json:"grp_name,omitempty"`
}

type SearchInputFwdMsgText struct {
	SearchText string `json:"search_text,omitempty"`
}

type UserFwdMsgInput struct {
	HtmlText    string                     `json:"fwd_text,omitempty"`
	FwdTo       []UserAndChannelFwdMessage `json:"fwd_list,omitempty"`
	ChatUuid    string                     `json:"fwd_chat_uuid,omitempty"`
	PostUuid    string                     `json:"fwd_post_uuid,omitempty"`
	ChannelUuid string                     `json:"fwd_channel_uuid,omitempty"`
	Attachments []*models.DgraphAttachment `json:"fwd_attachments,omitempty"`
}

type UserEmojiStatusInput struct {
	EmojiUuid    string `json:"emoji_id"`
	StatusDesc   string `json:"emoji_status_desc"`
	ExpiryTimeAt string `json:"emoji_expiry_time_at,omitempty"`
	ExpiryTimeIn string `json:"emoji_expiry_time_in"`
	TimeZone     string `json:"emoji_timezone"`
}

type FilterParam struct {
	Id    string      `json:"id"`
	Value interface{} `json:"value"`
}

type SortingParam struct {
	Id   string      `json:"id"`
	Desc interface{} `json:"desc"`
}

type TaskSearchString struct {
	SearchString string `json:"taskSearchString"`
}

type UserTokenOutput struct {
	TokenString string `json:"token"`
}

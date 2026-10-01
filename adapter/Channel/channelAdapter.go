package adapter

type InputChannelMemberInfo struct {
	ChannelUuid string `json:"channel_id,omitempty"`
	UserUuid    string `json:"user_id,omitempty"`
}

type InputJoinChannel struct {
	ChannelUuid string `json:"channel_uuid,omitempty"`
}

type OutputCreateChannel struct {
	ChannelUuid string `json:"channel_uuid,omitempty"`
}

type UpdateChannelInfo struct {
	ChannelUuid       string `json:"channel_uuid,omitempty"`
	ChannelName       string `json:"channel_name,omitempty"`
	ChannelAbout      string `json:"channel_about,omitempty"`
	ChannelPrivate    bool   `json:"channel_private,omitempty"`
	ChannelArchived   bool   `json:"channel_archived,omitempty"`
	ChannelProfileKey string `json:"channel_profile_key"`
	// PostPolicy ("everyone"|"admins_only") lets the edit dialog save the
	// announcement-channel setting in the same request as name/privacy/archive.
	// Empty means "leave the existing policy untouched" (backward compatible
	// with callers that don't send it).
	PostPolicy string `json:"post_policy,omitempty"`
}

type InputCreateChannel struct {
	ChannelName       string `json:"channel_name,omitempty"`
	ChannelPrivate    bool   `json:"channel_private,omitempty"`
	ChannelProfileKey string `json:"channel_profile_key"`
}

type InputCheckChannelName struct {
	ChannelName string `json:"ch_name,omitempty"`
}

type InputSearchChannelName struct {
	SearchText string `json:"search_text,omitempty"`
	PageIndex  int    `json:"page_index,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
}

type InputPublishTypingInChannel struct {
	ChannelUuid string `json:"channel_id,omitempty"`
}

type UserTokenOutput struct {
	TokenString    string `json:"token"`
	AlreadyExisted bool   `json:"already_existed"`
}

type InputMakeVideoChannelCall struct {
	ChannelUuid  string `json:"channel_uuid,omitempty"`
	AudioEnabled bool   `json:"audio_enabled,omitempty"`
	VideoEnabled bool   `json:"video_enabled,omitempty"`
}

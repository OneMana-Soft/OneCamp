package adapter

type CreateOrUpdateTeamInput struct {
	Name string `json:"team_name,omitempty"`
	Uuid string `json:"team_uuid,omitempty"`
}

type AddOrRemoveTeamMemberInput struct {
	UserUuid string `json:"member_uuid,omitempty"`
	TeamUUID string `json:"team_uuid,omitempty"`
}

package adapter

import dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

type CreateOrUpdateProjectInput struct {
	Name        string                           `json:"project_name,omitempty"`
	TeamUuid    string                           `json:"project_team_uuid,omitempty"`
	Uuid        string                           `json:"project_uuid,omitempty"`
	Attachments []*dgraphStruct.DgraphAttachment `json:"project_attachments,omitempty"`

	// Starting from a template (business/ProjectTemplate): which one, the day
	// the project starts (YYYY-MM-DD in TZ; none is today) and whether its
	// dates skip weekends.
	TemplateID   string `json:"template_id,omitempty"`
	StartDate    string `json:"start_date,omitempty"`
	TZ           string `json:"tz,omitempty"`
	SkipWeekends bool   `json:"skip_weekends,omitempty"`
}

type AddOrRemoveProjectMemberInput struct {
	UserUuid    string `json:"user_uuid,omitempty"`
	ProjectUuid string `json:"project_uuid,omitempty"`
}

type RemoveProjectAttachmentInput struct {
	AttachmentObjKey string `json:"attachment_obj_key,omitempty"`
}

type AddOrRemoveProjectInput struct {
	ProjectUuid string `json:"project_uuid,omitempty"`
}

type FilterParam struct {
	Id    string      `json:"id"`
	Value interface{} `json:"value"`
}

type SortingParam struct {
	Id   string      `json:"id"`
	Desc interface{} `json:"desc"`
}

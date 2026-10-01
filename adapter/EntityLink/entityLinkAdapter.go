package adapter

// InputEntityLink is the add/remove request for linking a doc or board to a
// task or project. SourceType is "task" or "project"; RefType is "doc" or
// "board".
type InputEntityLink struct {
	SourceType string `json:"source_type"`
	SourceUUID string `json:"source_uuid"`
	RefType    string `json:"ref_type"`
	RefUUID    string `json:"ref_uuid"`
}

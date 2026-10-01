package adapter

// Board adapter inputs (REST + collaboration-service persistence), mirroring the
// Doc adapter so boards inherit the same conventions.

// InputCreateBoard is the create-board request body.
type InputCreateBoard struct {
	BoardTitle   string `json:"board_title"`
	BoardPrivate bool   `json:"board_private"`
}

// InputUpdateBoard updates board metadata (title / privacy). The board canvas
// state itself is persisted via the collaboration service, not here.
type InputUpdateBoard struct {
	BoardId      string  `json:"board_uuid,omitempty"`
	Title        *string `json:"board_title,omitempty"`
	IsPrivate    *bool   `json:"board_private,omitempty"`
	ThumbnailKey *string `json:"board_thumbnail_key,omitempty"`
}

// InputBoardCollabUpdate is the payload the Hocuspocus board namespace POSTs to
// persist the serialized Yjs board state (mirrors InputDocCollabUpdate).
type InputBoardCollabUpdate struct {
	BoardUuid string `json:"documentId"`
	State     string `json:"state"`
	Snippet   string `json:"snippet"`
	// Contributors is the set of editor user uuids who changed the board since
	// the last persist, used to attribute the version snapshot. May be empty.
	Contributors []string `json:"contributors,omitempty"`
}

// InputUpdateBoardPermissions mirrors the doc permission editor (add/remove
// editors/viewers/commenters by user UUID).
type InputUpdateBoardPermissions struct {
	BoardId          string   `json:"board_uuid,omitempty"`
	AddEditors       []string `json:"add_editors,omitempty"`
	RemoveEditors    []string `json:"remove_editors,omitempty"`
	AddViewers       []string `json:"add_viewers,omitempty"`
	RemoveViewers    []string `json:"remove_viewers,omitempty"`
	AddCommenters    []string `json:"add_commenters,omitempty"`
	RemoveCommenters []string `json:"remove_commenters,omitempty"`
}

// InputGenerateBoardDiagram is the AI diagram-generation request. The diagram
// is generated server-side and streamed onto the canvas by the client. When
// Detailed is true, generation runs a plan->expand pipeline for deeper output.
type InputGenerateBoardDiagram struct {
	BoardId  string `json:"board_uuid"`
	Prompt   string `json:"prompt"`
	Type     string `json:"type"`
	Detailed bool   `json:"detailed"`
}

// BoardDiagramNode / BoardDiagramEdge mirror the minimal graph the refine flow
// round-trips: the client sends the current diagram, the server returns the
// updated one.
type BoardDiagramNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Shape string `json:"shape"`
}

type BoardDiagramEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
}

// InputRefineBoardDiagram applies a natural-language change to an existing
// generated diagram and returns the full updated, laid-out graph.
type InputRefineBoardDiagram struct {
	BoardId     string             `json:"board_uuid"`
	Instruction string             `json:"instruction"`
	Type        string             `json:"type"`
	Title       string             `json:"title"`
	Nodes       []BoardDiagramNode `json:"nodes"`
	Edges       []BoardDiagramEdge `json:"edges"`
}

// BoardClusterItem is one text item pulled from the canvas (a sticky note,
// text element, or labeled shape) to be grouped into a theme.
type BoardClusterItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// InputClusterBoard asks the AI to group existing canvas text into themes and
// synthesize them ("cluster the stickies"). The canvas is NOT modified; the
// response carries the themes plus a laid-out summary mind map the client can
// insert beside the originals.
type InputClusterBoard struct {
	BoardId string             `json:"board_uuid"`
	Items   []BoardClusterItem `json:"items"`
}

// InputGenerateBoardUI is the AI high-fidelity UI design request. The model
// returns a Tailwind-styled HTML screen the client renders in a sandboxed
// design preview (exportable to PNG/SVG/HTML, Figma and Canva).
type InputGenerateBoardUI struct {
	BoardId string `json:"board_uuid"`
	Prompt  string `json:"prompt"`
	Device  string `json:"device"` // "mobile" | "desktop"
}

// InputRefineBoardUI refines an existing generated screen: applies a described
// change or runs an autonomous design-QA pass (empty Instruction) to fix visual
// bugs. Returns corrected HTML the client renders in the same sandboxed preview.
type InputRefineBoardUI struct {
	BoardId     string `json:"board_uuid"`
	Html        string `json:"html"`
	Instruction string `json:"instruction"`
	Device      string `json:"device"` // "mobile" | "desktop"
}

// InputSearchBoardUser is the member-search request used by the share dialog.
type InputSearchBoardUser struct {
	SearchText string `json:"searchText"`
}

// InputBoardCommentMention is posted when a canvas comment @mentions users, so
// the mention/comment is mirrored into the activity subsystem (notifications +
// feed). The thread itself is held in Yjs; this only carries the text and the
// mentioned user UUIDs. CommentID is the stable Yjs comment id, used to key the
// server-side mirror so create/update/delete all target the same row.
type InputBoardCommentMention struct {
	BoardId            string   `json:"board_uuid"`
	CommentID          string   `json:"board_comment_id"`
	CommentText        string   `json:"comment_text"`
	MentionedUserUUIDs []string `json:"mentioned_user_uuids"`
}

// InputBoardCommentDelete removes a board comment's server-side mirror (search
// index + AI embedding + activity record) when it is deleted in the canvas.
type InputBoardCommentDelete struct {
	BoardId   string `json:"board_uuid"`
	CommentID string `json:"board_comment_id"`
}

// InputRestoreBoardSnapshot restores a board to a chosen snapshot from its
// version history.
type InputRestoreBoardSnapshot struct {
	BoardId    string `json:"board_uuid"`
	SnapshotId string `json:"snapshot_id"`
}

// InputRecordBoardView records that the caller opened the board ("Viewed by").
type InputRecordBoardView struct {
	BoardId string `json:"board_uuid"`
}

package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestACommentReachesTheAssigneeOrElseTheCreator(t *testing.T) {
	creator := &dgraphStruct.DgraphUser{Uid: "0x1a", Uuid: "creator-uuid"}
	assignee := &dgraphStruct.DgraphUser{Uid: "0x2b", Uuid: "assignee-uuid"}
	for name, c := range map[string]struct {
		task      *dgraphStruct.DgraphTask
		commenter string
		want      string
	}{
		"assigned":                         {&dgraphStruct.DgraphTask{CreatedBy: creator, Assignee: assignee}, "someone", "assignee-uuid"},
		"unassigned: the creator, by uuid": {&dgraphStruct.DgraphTask{CreatedBy: creator}, "someone", "creator-uuid"},
		"the assignee's own comment":       {&dgraphStruct.DgraphTask{CreatedBy: creator, Assignee: assignee}, "assignee-uuid", ""},
		"the creator's own comment":        {&dgraphStruct.DgraphTask{CreatedBy: creator}, "creator-uuid", ""},
		"no creator, nobody assigned":      {&dgraphStruct.DgraphTask{}, "someone", ""},
		"no task":                          {nil, "someone", ""},
	} {
		if got := commentRecipient(c.task, c.commenter); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

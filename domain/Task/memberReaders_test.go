package Domain

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A task's assignee taken off its project still reads it, but is no reader
// as one of its project's people; nor is its creator outside it. A task in
// no project has no project's people.
func TestOnlyMembersKeepsTheProjectsPeople(t *testing.T) {
	user := func(id string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{Uuid: id} }
	task := &dgraphStruct.DgraphTask{
		Assignee:  user("lee"),
		CreatedBy: user("cat"),
		Project:   &dgraphStruct.DgraphProject{Members: []*dgraphStruct.DgraphUser{user("BEN")}, Admins: []*dgraphStruct.DgraphUser{user("maya")}},
	}
	got := onlyMembers(task, map[string]bool{"ben": true, "maya": true, "lee": true, "cat": true})
	if len(got) != 2 || !got["ben"] || !got["maya"] {
		t.Errorf("got %v, want ben and maya", got)
	}
	if got := onlyMembers(&dgraphStruct.DgraphTask{Assignee: user("lee")}, map[string]bool{"lee": true}); len(got) != 0 {
		t.Errorf("a task in no project: %v", got)
	}
}

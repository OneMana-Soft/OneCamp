package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// An assignment no one can be named for (by nil: an issue given on GitHub by
// a sender who could be neither mapped to a person here nor provisioned as
// one) is written as no one's: its history row names no one, and nothing
// panics. The task is still given, as a person's assignment gives it, and a
// person's is theirs.
func TestAnAssignmentNoOneCanBeNamedForIsNoOnes(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	ben := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "5b1f6c2e-0000-4000-8000-000000000002", UserFullName: "Ben"}
	for name, c := range map[string]struct {
		by   *dgraphStruct.DgraphUser
		want string // the uid the history row names; "" for no one
	}{
		"no one (nil)":         {nil, ""},
		"someone with no node": {&dgraphStruct.DgraphUser{}, ""},
		"Kay, a person here":   {&dgraphStruct.DgraphUser{Uid: "0x3", UserFullName: "Kay"}, "0x3"},
	} {
		t.Run(name, func(t *testing.T) {
			write, search := assigneeWrite("8c0d7e4a-0000-4000-8000-000000000001", nil, ben, c.by, at, time.Time{})
			if write == nil || len(write.Activity) != 1 || search == nil {
				t.Fatalf("assigneeWrite wrote %+v and %+v; want the task with one activity, and its search entry", write, search)
			}
			got := ""
			if by := write.Activity[0].CreatedBy; by != nil {
				got = by.Uid
			}
			if got != c.want {
				t.Errorf("the history row of an assignment by %s names %q; want %q", name, got, c.want)
			}
			if write.Assignee == nil || write.Assignee.Uid != ben.Uid || search.TaskAssigneeUuid == nil || *search.TaskAssigneeUuid != ben.Uuid {
				t.Errorf("an assignment by %s didn't give the task to Ben: the graph's assignee %+v, search's %v", name, write.Assignee, search.TaskAssigneeUuid)
			}
			if next := write.Activity[0].NextState; next != "Ben" {
				t.Errorf("the history row of an assignment by %s says it went to %q; want Ben", name, next)
			}
		})
	}
}

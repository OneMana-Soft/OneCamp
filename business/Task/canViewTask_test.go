package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const viewer = "0x2a"

func task(mut func(*dgraphStruct.DgraphTask)) *dgraphStruct.DgraphTask {
	t := &dgraphStruct.DgraphTask{Project: &dgraphStruct.DgraphProject{}}
	if mut != nil {
		mut(t)
	}
	return t
}

// GET /task/info/{uuid} had its access check commented out and the Dgraph query
// has no membership filter, so any authenticated user could read any task in the
// workspace by uuid: name, description, comments, attachments, project. The MCP
// reach tool, agent delegation and the agent work entity read through the same
// function and were equally open.
func TestCanViewTask(t *testing.T) {
	cases := []struct {
		name string
		task *dgraphStruct.DgraphTask
		want bool
	}{
		{"project member", task(func(x *dgraphStruct.DgraphTask) { x.Project.IsProjectMember = 1 }), true},
		// Not implied by membership: an admin need not sit in project_members.
		{"project admin only", task(func(x *dgraphStruct.DgraphTask) { x.Project.IsProjectAdmin = 1 }), true},
		// The case that made the entity-link refusal so confusing: a task can be
		// assigned to somebody outside its project, and they must keep their work.
		{"assignee outside the project", task(func(x *dgraphStruct.DgraphTask) {
			x.Assignee = &dgraphStruct.DgraphUser{Uid: viewer}
		}), true},
		{"creator outside the project", task(func(x *dgraphStruct.DgraphTask) {
			x.CreatedBy = &dgraphStruct.DgraphUser{Uid: viewer}
		}), true},
		// The hole this closes.
		{"stranger", task(nil), false},
		{"assigned to somebody else", task(func(x *dgraphStruct.DgraphTask) {
			x.Assignee = &dgraphStruct.DgraphUser{Uid: "0x99"}
		}), false},
		{"created by somebody else", task(func(x *dgraphStruct.DgraphTask) {
			x.CreatedBy = &dgraphStruct.DgraphUser{Uid: "0x99"}
		}), false},
		// An unparented task belongs to whoever made it, not to everyone.
		{"no project, no claim", &dgraphStruct.DgraphTask{}, false},
		{"no project, is creator", &dgraphStruct.DgraphTask{
			CreatedBy: &dgraphStruct.DgraphUser{Uid: viewer},
		}, true},
		{"nil task", nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CanViewTask(c.task, viewer); got != c.want {
				t.Errorf("CanViewTask = %v, want %v", got, c.want)
			}
		})
	}
}

// An empty viewer is the placeholder background work passes. It must never be
// treated as a person who happens to match nothing: with a nil Assignee and a
// nil CreatedBy an empty-string comparison could otherwise slip through.
func TestAnEmptyViewerSeesNothing(t *testing.T) {
	full := task(func(x *dgraphStruct.DgraphTask) {
		x.Assignee = &dgraphStruct.DgraphUser{}
		x.CreatedBy = &dgraphStruct.DgraphUser{}
	})
	if CanViewTask(full, "") {
		t.Error("an empty viewer uid was granted access; background reads must use WithSystemRead instead")
	}
}

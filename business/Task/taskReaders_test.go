package business

import (
	"testing"

	domain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who a task's notices may reach (domain/Task ReadersAmong) is who may read it
// (CanViewTask), way in by way in, so an email never reaches someone the app
// would refuse the task to, and never misses someone it would show it to.
func TestTaskReadersAreTheTasksViewers(t *testing.T) {
	const uid, id = "0x9", "AAAAAAAA-0000-4000-8000-000000000009"
	me := &dgraphStruct.DgraphUser{Uid: uid, Uuid: id}
	other := &dgraphStruct.DgraphUser{Uid: "0x8", Uuid: "bbbbbbbb-0000-4000-8000-000000000008"}
	for _, c := range []struct {
		name                             string
		member, admin, assignee, creator bool
		projectless                      bool
		want                             bool
	}{
		{name: "nobody to the task"},
		{name: "a member", member: true, want: true},
		{name: "an admin", admin: true, want: true},
		{name: "the assignee, outside the project", assignee: true, want: true},
		// Taken off the project: their task went with it.
		{name: "the creator, outside the project", creator: true},
		{name: "the creator, in the project", creator: true, member: true, want: true},
		{name: "the creator of a task in no project", creator: true, projectless: true, want: true},
		{name: "the assignee of a task in no project", assignee: true, projectless: true, want: true},
		{name: "someone, for a task in no project", projectless: true},
	} {
		pick := func(b bool) *dgraphStruct.DgraphUser {
			if b {
				return me
			}
			return other
		}
		count := func(b bool) uint8 {
			if b {
				return 1
			}
			return 0
		}
		asRead := &dgraphStruct.DgraphTask{Assignee: pick(c.assignee), CreatedBy: pick(c.creator)}
		withPeople := &dgraphStruct.DgraphTask{Assignee: pick(c.assignee), CreatedBy: pick(c.creator)}
		if !c.projectless {
			asRead.Project = &dgraphStruct.DgraphProject{IsProjectMember: count(c.member), IsProjectAdmin: count(c.admin)}
			withPeople.Project = &dgraphStruct.DgraphProject{Members: []*dgraphStruct.DgraphUser{other}, Admins: []*dgraphStruct.DgraphUser{other}}
			if c.member {
				withPeople.Project.Members = append(withPeople.Project.Members, me)
			}
			if c.admin {
				withPeople.Project.Admins = append(withPeople.Project.Admins, me)
			}
		}
		views := CanViewTask(asRead, uid)
		reaches := len(domain.ReadersAmong(withPeople, []string{id})) == 1
		if views != c.want || reaches != c.want {
			t.Errorf("%s: may read it %v, its notices reach them %v, want %v", c.name, views, reaches, c.want)
		}
	}
}

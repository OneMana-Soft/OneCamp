package controllers

import (
	"net/http"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who may take Maya off the project: its admins and its team's admins, and
// Maya herself. Someone in the project who is neither is refused, and someone
// who can't see the project, or a project or member that isn't there, answers
// as not found, never by reaching for a member who isn't there.
func TestOnlyAdminsTakePeopleOffAProject(t *testing.T) {
	const caller, maya = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	project := func(member, admin, teamAdmin bool, in ...*dgraphStruct.DgraphUser) *dgraphStruct.DgraphProject {
		count := func(b bool) uint8 {
			if b {
				return 1
			}
			return 0
		}
		return &dgraphStruct.DgraphProject{Uuid: "33333333-3333-4333-8333-333333333333",
			IsProjectMember: count(member), IsProjectAdmin: count(admin),
			Team:      &dgraphStruct.DgraphTeam{IsAdmin: count(teamAdmin)},
			CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x1"}, Members: in}
	}
	mayaIn := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: maya}
	creator := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: maya}
	for _, c := range []struct {
		name   string
		p      *dgraphStruct.DgraphProject
		caller string
		want   int
	}{
		{"someone outside the project", project(false, false, false, mayaIn), caller, http.StatusNotFound},
		{"a member who isn't an admin", project(true, false, false, mayaIn), caller, http.StatusForbidden},
		{"one of the project's admins", project(true, true, false, mayaIn), caller, 0},
		{"one of the team's admins, outside the project", project(false, false, true, mayaIn), caller, 0},
		{"Maya leaving", project(true, false, false, mayaIn), maya, 0},
		{"an admin, for someone not in the project", project(true, true, false), caller, http.StatusNotFound},
		{"a team admin, for someone not in the project", project(false, false, true), caller, http.StatusNotFound},
		{"an admin, for the project's creator", project(true, true, false, creator), caller, http.StatusForbidden},
		{"a project that isn't there", nil, caller, http.StatusNotFound},
		{"a project read empty", &dgraphStruct.DgraphProject{}, caller, http.StatusNotFound},
	} {
		if got, msg := memberRemovalRefusal(c.p, c.caller, maya); got != c.want {
			t.Errorf("%s: %d %q, want %d", c.name, got, msg, c.want)
		}
	}
}

// One rule says who manages a project's people, for adding and removing
// members and admins alike: its admins and its team's admins. A project read
// without its team is never taken as its team's admin.
func TestManagesMembersIsTheProjectsOrItsTeamsAdmins(t *testing.T) {
	for _, c := range []struct {
		name string
		p    *dgraphStruct.DgraphProject
		want bool
	}{
		{"one of its admins", &dgraphStruct.DgraphProject{IsProjectAdmin: 1}, true},
		{"one of its team's admins", &dgraphStruct.DgraphProject{Team: &dgraphStruct.DgraphTeam{IsAdmin: 1}}, true},
		{"a member", &dgraphStruct.DgraphProject{IsProjectMember: 1, Team: &dgraphStruct.DgraphTeam{}}, false},
		{"a project read without its team", &dgraphStruct.DgraphProject{IsProjectMember: 1}, false},
		{"no project", nil, false},
	} {
		if got := managesMembers(c.p); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

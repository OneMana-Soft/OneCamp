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
		if got, msg := memberChangeRefusal(c.p, manages(c.p), takeOff, c.caller, maya); got != c.want {
			t.Errorf("%s: %d %q, want %d", c.name, got, msg, c.want)
		}
	}
}

// Adding people and making or unmaking admins ask the same rule as taking
// people off: a member who doesn't manage the project is told who can, in
// words of the change; someone who can't see the project, and a person who
// isn't there, answer as not found. They answered "Not Authorised" to all.
func TestEveryMemberChangeSaysWhoMayMakeIt(t *testing.T) {
	const caller, maya = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	mayaIn := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: maya}
	creator := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: maya}
	guest := &dgraphStruct.DgraphUser{Uid: "0x3", Uuid: maya, IsExternal: true}
	project := func(member, admin bool, in []*dgraphStruct.DgraphUser, team []*dgraphStruct.DgraphUser) *dgraphStruct.DgraphProject {
		count := func(b bool) uint8 {
			if b {
				return 1
			}
			return 0
		}
		return &dgraphStruct.DgraphProject{Uuid: "33333333-3333-4333-8333-333333333333",
			IsProjectMember: count(member), IsProjectAdmin: count(admin),
			Team:      &dgraphStruct.DgraphTeam{Members: team},
			CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x1"}, Members: in}
	}
	none := []*dgraphStruct.DgraphUser(nil)
	for _, c := range []struct {
		name   string
		p      *dgraphStruct.DgraphProject
		change memberChange
		want   int
		says   string
	}{
		{"a member adding someone", project(true, false, none, []*dgraphStruct.DgraphUser{mayaIn}), addMember, http.StatusForbidden, "Only the project's admins can add people to it."},
		{"a member making an admin", project(true, false, []*dgraphStruct.DgraphUser{mayaIn}, none), makeAdmin, http.StatusForbidden, "Only the project's admins can make someone an admin."},
		{"a member unmaking an admin", project(true, false, []*dgraphStruct.DgraphUser{mayaIn}, none), removeAdmin, http.StatusForbidden, "Only the project's admins can change who its admins are."},
		{"someone outside, adding", project(false, false, none, []*dgraphStruct.DgraphUser{mayaIn}), addMember, http.StatusNotFound, "Project not found"},
		{"an admin adding someone in the team", project(true, true, none, []*dgraphStruct.DgraphUser{mayaIn}), addMember, 0, ""},
		{"an admin adding someone outside the team", project(true, true, none, none), addMember, http.StatusNotFound, "They aren't in the project's team."},
		{"an admin adding a guest", project(true, true, none, []*dgraphStruct.DgraphUser{guest}), addMember, http.StatusBadRequest, "A guest can't join a project. Share it with them instead."},
		{"an admin making a member an admin", project(true, true, []*dgraphStruct.DgraphUser{mayaIn}, none), makeAdmin, 0, ""},
		{"an admin making someone not in it an admin", project(true, true, none, none), makeAdmin, http.StatusNotFound, "They aren't in this project."},
		{"an admin unmaking an admin", project(true, true, []*dgraphStruct.DgraphUser{mayaIn}, none), removeAdmin, 0, ""},
		{"an admin unmaking the creator", project(true, true, []*dgraphStruct.DgraphUser{creator}, none), removeAdmin, http.StatusForbidden, "The project's creator stays one of its admins."},
		{"a member leaving is no change to admins", project(true, false, []*dgraphStruct.DgraphUser{mayaIn}, none), removeAdmin, http.StatusForbidden, "Only the project's admins can change who its admins are."},
		{"a project that isn't there", nil, makeAdmin, http.StatusNotFound, "Project not found"},
	} {
		caller := caller
		if c.name == "a member leaving is no change to admins" {
			caller = maya
		}
		if got, msg := memberChangeRefusal(c.p, manages(c.p), c.change, caller, maya); got != c.want || msg != c.says {
			t.Errorf("%s: %d %q, want %d %q", c.name, got, msg, c.want, c.says)
		}
	}
	for _, change := range []memberChange{addMember, takeOff, makeAdmin, removeAdmin, changeRole} {
		if refusedWords[change] == "" || doneWords[change] == "" {
			t.Errorf("change %d has no words", change)
		}
	}
}

// manages is what Work Access answers for a project read with no role rows:
// its admins and its team's admins manage its people (business/WorkAccess).
func manages(p *dgraphStruct.DgraphProject) bool {
	return p != nil && (p.IsProjectAdmin > 0 || (p.Team != nil && p.Team.IsAdmin > 0))
}

// Changing what someone may do is for those who manage the project's people,
// and never lowers its creator.
func TestARoleChangeIsTheirsWhoManageThePeople(t *testing.T) {
	const caller, maya = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	mayaIn := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: maya}
	creator := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: maya}
	in := func(admin bool, who *dgraphStruct.DgraphUser) *dgraphStruct.DgraphProject {
		p := &dgraphStruct.DgraphProject{Uuid: "33333333-3333-4333-8333-333333333333", IsProjectMember: 1,
			CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x1"}, Members: []*dgraphStruct.DgraphUser{who}}
		if admin {
			p.IsProjectAdmin = 1
		}
		return p
	}
	for _, c := range []struct {
		name string
		p    *dgraphStruct.DgraphProject
		want int
		says string
	}{
		{"a member", in(false, mayaIn), http.StatusForbidden, "Only the project's admins can change what people can do in it."},
		{"an admin", in(true, mayaIn), 0, ""},
		{"an admin, for the creator", in(true, creator), http.StatusForbidden, "The project's creator stays one of its admins."},
	} {
		if got, msg := memberChangeRefusal(c.p, manages(c.p), changeRole, caller, maya); got != c.want || msg != c.says {
			t.Errorf("%s: %d %q, want %d %q", c.name, got, msg, c.want, c.says)
		}
	}
}

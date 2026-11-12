package business

import (
	"strings"
	"testing"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// UX-PROJECTS-H2-BE (PROJECTS-20): New task offers the projects its reader
// may add tasks to, by Work Access, and none else: a member's as well as an
// admin's (it offered the projects they admin only), never a viewer's or a
// commenter's, nor one whose tasks only its admins change, nor an archived
// one; by name, whatever its case.
func TestNewTaskOffersTheProjectsYouMayAddTasksToByName(t *testing.T) {
	p := func(id, name string) *dgraphStruct.DgraphProject {
		return &dgraphStruct.DgraphProject{Uuid: id, Name: name}
	}
	ps := []*dgraphStruct.DgraphProject{p("A", "website"), p("b", "Billing"), p("c", "Hiring"), p("d", "Legal"), p("e", "Ops"), p("f", "Archive"), p("g", "Assigned work")}
	standings := map[string]workAccess.Standing{
		"a": {ProjectUUID: "a", Role: workAccess.RoleMember, EditWho: workAccess.EditMembers},
		"b": {ProjectUUID: "b", Role: workAccess.RoleAdmin, EditWho: workAccess.EditAdmins},
		"c": {ProjectUUID: "c", Role: workAccess.RoleViewer, EditWho: workAccess.EditMembers},
		"d": {ProjectUUID: "d", Role: workAccess.RoleCommenter, EditWho: workAccess.EditMembers},
		"e": {ProjectUUID: "e", Role: workAccess.RoleMember, EditWho: workAccess.EditAdmins},
		"f": {ProjectUUID: "f", Role: workAccess.RoleAdmin, Archived: true},
		"g": {ProjectUUID: "g", Role: workAccess.RoleMember, EditWho: workAccess.EditAssigned},
	}
	var got []string
	for _, q := range TasksGoIn(ps, standings) {
		got = append(got, q.Name)
	}
	if strings.Join(got, ", ") != "Assigned work, Billing, website" {
		t.Fatalf("offered %v", got)
	}
	if TasksGoIn(nil, nil) == nil {
		t.Fatal("none is an empty list, not null")
	}
}

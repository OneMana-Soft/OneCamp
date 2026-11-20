package domain

import (
	"encoding/json"
	"slices"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who is in a project or a team is its members and its admins, each once
// whatever the case of their uuid, in one order.
func TestPeopleOfIsEachPersonOnce(t *testing.T) {
	members := []*dgraphStruct.DgraphUser{{Uuid: "B0B"}, nil, {Uuid: ""}, {Uuid: "a11ce"}}
	admins := []*dgraphStruct.DgraphUser{{Uuid: "b0b"}, {Uuid: "c0"}}
	if got := PeopleOf(members, admins); !slices.Equal(got, []string{"a11ce", "b0b", "c0"}) {
		t.Errorf("members and admins: %v", got)
	}
	if got := PeopleOf(); len(got) != 0 {
		t.Errorf("nobody: %v", got)
	}
}

// One read of a space serves a doc's own space (GetDocACL, beside the doc)
// and a space named by its uuid (GetSpace): the answer gives the space of the
// kind asked for, its name and who is in it, and nothing for another kind or
// one that isn't there.
func TestOneReadOfASpace(t *testing.T) {
	answer := []byte(`{
		"docInfo": [{"doc_uuid": "d"}],
		"project": [{"project_name": "Launch", "project_members": [{"user_uuid": "B0B"}], "project_admins": [{"user_uuid": "dee"}, {"user_uuid": "b0b"}]}],
		"team": []
	}`)
	var acl struct {
		DocInfo []*dgraphStruct.DgraphDoc `json:"docInfo"`
		spaceRead
	}
	if err := json.Unmarshal(answer, &acl); err != nil {
		t.Fatal(err)
	}
	name, people, ok := acl.of("project")
	if !ok || name != "Launch" || !slices.Equal(people, []string{"b0b", "dee"}) || len(acl.DocInfo) != 1 {
		t.Errorf("the doc's project: %q %v %v", name, people, ok)
	}
	if _, _, ok := acl.of("team"); ok {
		t.Error("a team that isn't there was found")
	}
	if _, _, ok := acl.of("channel"); ok {
		t.Error("a kind that isn't a space was found")
	}

	// A read of many docs' spaces (GetDocACLs) gives each doc its own, by uuid.
	var many spaceRead
	if err := json.Unmarshal([]byte(`{
		"project": [{"project_uuid": "p1", "project_name": "Launch", "project_members": [{"user_uuid": "ann"}]},
			{"project_uuid": "P2", "project_name": "Docs", "project_members": [{"user_uuid": "bo"}]}],
		"team": [{"team_uuid": "t1", "team_name": "Design", "team_admins": [{"user_uuid": "cy"}]}]
	}`), &many); err != nil {
		t.Fatal(err)
	}
	if name, people, ok := many.in("project", "p2"); !ok || name != "Docs" || !slices.Equal(people, []string{"bo"}) {
		t.Errorf("project p2: %q %v %v", name, people, ok)
	}
	if name, people, ok := many.in("team", "t1"); !ok || name != "Design" || !slices.Equal(people, []string{"cy"}) {
		t.Errorf("team t1: %q %v %v", name, people, ok)
	}
	if _, _, ok := many.in("project", "p3"); ok {
		t.Error("a project that wasn't read was found")
	}
}

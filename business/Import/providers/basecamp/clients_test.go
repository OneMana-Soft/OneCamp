package basecamp

import (
	"strings"
	"testing"
)

// A Basecamp client sees only what's shared with clients there, so a
// project's clients don't join it here, where they'd see everything in it;
// the report counts them, without names, for the admin to add on purpose.
func TestABasecampProjectsClientsDontJoinIt(t *testing.T) {
	members, clients := projectPeople([]bcPerson{{ID: 1, Name: "Ana"}, {ID: 2, Name: "Cleo", Client: true}, {ID: 3, Name: "Bo"}, {ID: 4, Name: "Cal", Client: true}})
	if strings.Join(members, ",") != "1,3" || clients != 2 {
		t.Fatalf("members %v, clients %d", members, clients)
	}
	n := clientsNote(42, clients)
	if n.SourceID != "42" || n.Code != "PROJECT_CLIENTS_LEFT_OUT" || !strings.Contains(n.Message, "Left out: 2.") || strings.Contains(n.Message, "Cleo") {
		t.Errorf("the report's line: %+v", n)
	}
	if members, clients := projectPeople([]bcPerson{{ID: 5}}); len(members) != 1 || clients != 0 {
		t.Errorf("a project without clients: %v %d", members, clients)
	}
}

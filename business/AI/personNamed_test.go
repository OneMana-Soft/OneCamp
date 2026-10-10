package business

import (
	"fmt"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A name the model gives for a direct message's recipient or a task's
// assignee is used only when it plainly means one person: the only one the
// search found, else the one whose handle it is, else the only one with that
// display name. The search finds people by full name and handle too, so the
// first one found was too often someone else.
func TestANameIsUsedOnlyWhenItMeansOnePerson(t *testing.T) {
	sam := &dgraphStruct.DgraphUser{Uuid: "1", UserName: "Sam", UserFullName: "Samuel Rivera", Handle: "sam.r"}
	ana := &dgraphStruct.DgraphUser{Uuid: "2", UserName: "Ana", UserFullName: "Ana Rivera", Handle: "ana.rivera"}
	otherSam := &dgraphStruct.DgraphUser{Uuid: "3", UserName: "sam", Handle: "sam-2"}
	for _, c := range []struct {
		name  string
		found []*dgraphStruct.DgraphUser
		want  *dgraphStruct.DgraphUser
	}{
		{"rivera", []*dgraphStruct.DgraphUser{sam}, sam},          // the only one found
		{"rivera", []*dgraphStruct.DgraphUser{sam, ana}, nil},     // either of them
		{"ANA.RIVERA", []*dgraphStruct.DgraphUser{sam, ana}, ana}, // her handle
		{"Sam", []*dgraphStruct.DgraphUser{ana, sam}, sam},        // the only one called that
		{"Sam", []*dgraphStruct.DgraphUser{sam, otherSam}, nil},   // two called that
		{"sam-2", []*dgraphStruct.DgraphUser{sam, otherSam}, otherSam},
	} {
		if got := personNamed(c.found, c.name); got != c.want {
			t.Errorf("%q among %d: got %+v, want %+v", c.name, len(c.found), got, c.want)
		}
	}
}

// When a name could be more than one person, the model is told who, each by
// name and @handle and never by address, which it would repeat.
func TestAnAmbiguousNameListsPeopleWithoutTheirAddresses(t *testing.T) {
	people := []*dgraphStruct.DgraphUser{
		{UserName: "Sam", UserFullName: "Samuel Rivera", Handle: "sam.r", EmailID: "sam@example.test"},
		{UserFullName: "Priya Raman", Handle: "priya.raman", EmailID: "priya@example.test"},
		{Handle: "jo.chen", EmailID: "jo.chen@example.test"},
		{EmailID: "nameless@example.test"},
		nil,
	}
	if got, want := peopleByNameAndHandle(people), "Sam (@sam.r), Priya Raman (@priya.raman), @jo.chen"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	many := make([]*dgraphStruct.DgraphUser, ambiguousPeopleShown+3)
	for i := range many {
		many[i] = &dgraphStruct.DgraphUser{UserName: fmt.Sprintf("Sam %d", i), Handle: fmt.Sprintf("sam-%d", i)}
	}
	got := peopleByNameAndHandle(many)
	if !strings.HasSuffix(got, ", and 3 more") || strings.Count(got, "@") != ambiguousPeopleShown {
		t.Fatalf("a long list should name %d and count the rest: %q", ambiguousPeopleShown, got)
	}
}

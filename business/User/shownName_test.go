package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The name copied over an author's entries in search is the one people see:
// a new display name reaches them, and a new full name doesn't replace the
// display name there.
func TestTheNameSearchCopiesIsTheShownName(t *testing.T) {
	saved := &dgraphStruct.DgraphUser{UserName: "Sam", UserFullName: "Samuel Rivera", EmailID: "sam@example.com"}
	cases := []struct {
		name   string
		update dgraphStruct.DgraphUser
		want   string
	}{
		{"display name changed", dgraphStruct.DgraphUser{UserName: "Sammy", UserFullName: "Samuel Rivera"}, "Sammy"},
		{"full name changed", dgraphStruct.DgraphUser{UserName: "Sam", UserFullName: "Samuel R."}, "Sam"},
		{"nothing sent keeps the saved names", dgraphStruct.DgraphUser{}, "Sam"},
	}
	for _, c := range cases {
		update := c.update
		if got := shownNameAfter(saved, &update); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := shownNameAfter(nil, &dgraphStruct.DgraphUser{UserFullName: "Samuel Rivera"}); got != "Samuel Rivera" {
		t.Errorf("nothing saved: got %q", got)
	}
}

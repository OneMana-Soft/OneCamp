package models

import (
	"strings"
	"testing"
)

func TestDgraphUserDisplayName(t *testing.T) {
	var nobody *DgraphUser
	if got := nobody.DisplayName(); got != "" {
		t.Fatalf("a nil user is called %q", got)
	}
	u := &DgraphUser{UserFullName: "Samuel Rivera", EmailID: "sam@example.com"}
	if got := u.DisplayName(); got != "Samuel Rivera" {
		t.Fatalf("no display name: got %q, want the full name", got)
	}
	u.UserName = "Sam"
	if got := u.DisplayName(); got != "Sam" {
		t.Fatalf("got %q, want the display name", got)
	}
}

// Byte order, as Dgraph's orderasc on user_name gives (checked against a live
// Dgraph on 14 Oct 2026), with nobody nameless or missing in front.
func TestSortUsersByNameMatchesDgraph(t *testing.T) {
	var users []*DgraphUser
	for _, n := range []string{"zed", "", "Bob", "alice", "Émile", "bob", "_x", "Zoë", "ann-marie", "ann marie", "10x", "Ålborg"} {
		users = append(users, &DgraphUser{UserName: n})
	}
	users = append(users, nil)
	SortUsersByName(users)
	var got []string
	for _, u := range users {
		if u == nil {
			got = append(got, "<nil>")
			continue
		}
		got = append(got, u.UserName)
	}
	want := []string{"10x", "Bob", "Zoë", "_x", "alice", "ann marie", "ann-marie", "bob", "zed", "Ålborg", "Émile", "", "<nil>"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order\n got %q\nwant %q", got, want)
	}
}

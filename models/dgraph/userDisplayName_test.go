package models

import "testing"

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

package business

import "testing"

// A comment is removed by its author, who can still read what it's on, or by
// a moderator: the resource's owner, someone with full access to it, or a
// workspace admin; never once the resource is in the trash.
func TestWhoMayRemoveAComment(t *testing.T) {
	for _, c := range []struct {
		name            string
		author, canRead bool
		m               Moderator
		want            bool
	}{
		{"its author", true, true, Moderator{}, true},
		{"its author, who can't read the doc any more", true, false, Moderator{}, false},
		{"someone else who can read it", false, true, Moderator{}, false},
		{"the doc's owner", false, true, Moderator{Owner: true}, true},
		{"someone with full access", false, true, Moderator{FullAccess: true}, true},
		{"a workspace admin", false, false, Moderator{Admin: true}, true},
		{"the owner, on a doc in the trash", false, true, Moderator{Owner: true, Deleted: true}, false},
		{"its author, on a doc in the trash", true, true, Moderator{Deleted: true}, false},
	} {
		if got := CanRemoveComment(c.author, c.canRead, c.m); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

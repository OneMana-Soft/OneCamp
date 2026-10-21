package todoist

import "testing"

// I1: a Todoist task's notes are its comments; the import counts them so the
// comments stage asks for them (it never did).
func TestATaskCountsItsNotes(t *testing.T) {
	snap := &todoistSnapshot{Notes: []todoistNote{{ID: "n1", ItemID: "i1"}, {ID: "n2", ItemID: "i1"}, {ID: "n3", ItemID: "i2"}, {ID: "n4", ItemID: "i1", IsDeleted: true}}}
	if got := buildSourceTask(snap, &todoistItem{ID: "i1", ProjectID: "p"}, nil).CommentCount; got != 2 {
		t.Fatalf("comment count %d, want 2", got)
	}
}

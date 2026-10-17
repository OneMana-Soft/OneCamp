package business

import (
	"reflect"
	"testing"
)

// The archive takes whole subtrees: never a page under a page still in use,
// never a page with a subpage still in use.
func TestTheArchiveTakesWholeSubtreesOnly(t *testing.T) {
	// Engineering (live, used) > RFCs (inactive) > {A (inactive), B (used)}
	// Old (inactive) > {Notes (inactive) > Draft (inactive)}
	// Lone (inactive, no parent)
	// Orphan (inactive) whose parent is already gone (no live parent)
	links := docLinks{
		Parent: map[string]string{
			"rfcs": "eng", "a": "rfcs", "b": "rfcs",
			"old": "", "notes": "old", "draft": "notes",
			"lone": "", "orphan": "",
		},
		Kids: map[string][]string{
			"eng": {"rfcs"}, "rfcs": {"a", "b"},
			"old": {"notes"}, "notes": {"draft"},
		},
	}
	due := []string{"rfcs", "a", "old", "notes", "draft", "lone", "orphan"}
	got := archivableDocs(due, links)
	want := []string{"old", "notes", "draft", "lone", "orphan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("archived %v, want %v: RFCs and A stay (Engineering above is live, B below is in use)", got, want)
	}
}

// A page whose parent is in use stays, even when everything under it is idle.
func TestTheArchiveSkipsDocsWhoseParentIsLive(t *testing.T) {
	links := docLinks{Parent: map[string]string{"child": "parent", "grandchild": "child"}, Kids: map[string][]string{"child": {"grandchild"}}}
	if got := archivableDocs([]string{"child", "grandchild"}, links); len(got) != 0 {
		t.Fatalf("archived %v under a live parent", got)
	}
	// And with the parent idle too, all three go together.
	links.Parent["parent"] = ""
	links.Kids["parent"] = []string{"child"}
	if got := archivableDocs([]string{"parent", "child", "grandchild"}, links); len(got) != 3 {
		t.Fatalf("archived %v, want the whole subtree", got)
	}
}

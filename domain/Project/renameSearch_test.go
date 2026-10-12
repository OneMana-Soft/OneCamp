package Domain

import "testing"

// A rename wrote the project's whole search document, deleted_date null
// included, so renaming an archived project (as the demo's refresh does to
// retire one) put it back in search.
func TestARenameLeavesTheArchivedStateInSearchAlone(t *testing.T) {
	f := projectRenameFields("Q4 launch-retired-1791614951", 1791614951)
	if _, ok := f["deleted_date"]; ok {
		t.Fatalf("a rename touches deleted_date: %v", f)
	}
	if f["project_name"] != "Q4 launch-retired-1791614951" || f["updated_date"] != int64(1791614951) {
		t.Errorf("rename fields: %v", f)
	}
}

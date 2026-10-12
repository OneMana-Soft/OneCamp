package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The restore list named every doc "Document · <time> (<id>)", and listed the
// docs owners deleted among the ones the retention archived. Each now reads as
// its title, and one its owner deleted says who deleted it: there is no Trash
// in the app yet, so admins restore those here too, knowingly. A private doc
// the admin may not read is "Private doc" and whose it is, never its title.
func TestTheRestoreListSaysWhoseChoiceADocsDeletionWas(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	owner := &dgraphStruct.DgraphUser{Uuid: "o", UserName: "Dana Whitfield"}
	for _, c := range []struct {
		name     string
		doc      *dgraphStruct.DgraphDoc
		readable bool
		want     string
	}{
		{"archived", &dgraphStruct.DgraphDoc{Title: "Q3 plan", CreatedBy: owner, DeletedAt: &at, ArchivedAt: &at}, true, "Q3 plan"},
		{"deleted by its owner", &dgraphStruct.DgraphDoc{Title: "Q3 plan", CreatedBy: owner, DeletedAt: &at}, true, "Q3 plan · deleted by Dana Whitfield"},
		{"untitled, owner unknown", &dgraphStruct.DgraphDoc{DeletedAt: &at}, true, "Untitled document · deleted by its owner"},
		{"archived, not theirs to read", &dgraphStruct.DgraphDoc{Title: "Salaries", CreatedBy: owner, DeletedAt: &at, ArchivedAt: &at}, false, "Private doc · owned by Dana Whitfield"},
		{"deleted by its owner, not theirs to read", &dgraphStruct.DgraphDoc{Title: "Salaries", CreatedBy: owner, DeletedAt: &at}, false, "Private doc · deleted by Dana Whitfield"},
		{"archived, not theirs to read, owner unknown", &dgraphStruct.DgraphDoc{Title: "Salaries", DeletedAt: &at, ArchivedAt: &at}, false, "Private doc"},
		{"deleted, not theirs to read, owner unknown", &dgraphStruct.DgraphDoc{Title: "Salaries", DeletedAt: &at}, false, "Private doc · deleted by its owner"},
	} {
		if got := docRestoreName(c.doc, c.readable); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

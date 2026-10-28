package business

import (
	"slices"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestADocsKeepersAreItsOwnerFullAccessAndEditors(t *testing.T) {
	olu, ful, ed, rea := &dgraphStruct.DgraphUser{Uuid: "olu"}, &dgraphStruct.DgraphUser{Uuid: "ful"}, &dgraphStruct.DgraphUser{Uuid: "ed"}, &dgraphStruct.DgraphUser{Uuid: "rea"}
	got := docKeepers(&dgraphStruct.DgraphDoc{
		CreatedBy:       olu,
		FullAccessUsers: []*dgraphStruct.DgraphUser{ful},
		EditingUser:     []*dgraphStruct.DgraphUser{ed, olu, {}},
		ReadingUser:     []*dgraphStruct.DgraphUser{rea},
		CommentingUser:  []*dgraphStruct.DgraphUser{rea},
	})
	if !slices.Equal(got, []string{"olu", "ful", "ed"}) {
		t.Errorf("keepers %v, want the owner, the full-access person and the editor once each, not readers or commenters", got)
	}
	if docKeepers(nil) != nil || len(docKeepers(&dgraphStruct.DgraphDoc{})) != 0 {
		t.Error("a doc with nobody has no keepers")
	}
}

// After a hand-over the doc's owner is the one it was handed to: its maker is
// a keeper only while it is still shared with them.
func TestAHandedOnDocsKeeperIsItsNewOwner(t *testing.T) {
	maker, bea := &dgraphStruct.DgraphUser{Uuid: "olu"}, &dgraphStruct.DgraphUser{Uuid: "bea"}
	got := docKeepers(&dgraphStruct.DgraphDoc{CreatedBy: maker, Owner: bea})
	if !slices.Equal(got, []string{"bea"}) {
		t.Errorf("keepers %v, want the new owner alone: the maker was taken off it", got)
	}
}

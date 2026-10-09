package business

import (
	"slices"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestADocsKeepersAreItsMakerAndEditors(t *testing.T) {
	olu, ed, rea := &dgraphStruct.DgraphUser{Uuid: "olu"}, &dgraphStruct.DgraphUser{Uuid: "ed"}, &dgraphStruct.DgraphUser{Uuid: "rea"}
	got := docKeepers(&dgraphStruct.DgraphDoc{
		CreatedBy:      olu,
		EditingUser:    []*dgraphStruct.DgraphUser{ed, olu, {}},
		ReadingUser:    []*dgraphStruct.DgraphUser{rea},
		CommentingUser: []*dgraphStruct.DgraphUser{rea},
	})
	if !slices.Equal(got, []string{"olu", "ed"}) {
		t.Errorf("keepers %v, want the maker and the editor once each, not readers or commenters", got)
	}
	if docKeepers(nil) != nil || len(docKeepers(&dgraphStruct.DgraphDoc{})) != 0 {
		t.Error("a doc with nobody has no keepers")
	}
}

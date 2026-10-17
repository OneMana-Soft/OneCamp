package businness

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A comment on a doc whose creator was removed dereferenced a nil creator and
// answered 500; it records no creator instead.
func TestACommentOnADocWithoutItsCreatorRecordsNone(t *testing.T) {
	for _, doc := range []*dgraphStruct.DgraphDoc{nil, {}, {CreatedBy: &dgraphStruct.DgraphUser{}}} {
		if got := contentAddedBy(doc); got != nil {
			t.Errorf("%+v: got %+v, want none", doc, got)
		}
	}
	if got := contentAddedBy(&dgraphStruct.DgraphDoc{CreatedBy: &dgraphStruct.DgraphUser{Uid: "0x1"}}); got == nil || got.Uid != "0x1" {
		t.Errorf("got %+v, want 0x1", got)
	}
}

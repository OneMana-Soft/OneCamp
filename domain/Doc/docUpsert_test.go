package domain

import (
	"context"
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Deleting a doc answered 200 for weeks and deleted nothing: the update carried
// no uuid, so its query matched no doc. An update that names no doc must fail,
// before it reaches Dgraph, rather than succeed at nothing.
func TestAnUpdateThatNamesNoDocFails(t *testing.T) {
	_, err := CreateOrUpdateDgraphDoc(context.Background(), &dgraphStruct.DgraphDoc{Uid: "uid(doc)"})
	if !errors.Is(err, ErrNoDocToUpdate) {
		t.Fatalf("got %v, want ErrNoDocToUpdate", err)
	}
}

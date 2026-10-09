package models

import (
	"context"
	"errors"
	"testing"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
)

// A sharing change writes the doc's and each person's id into its query, so
// anything that isn't a uuid is refused before a query is built (and before
// the graph client is touched: there is none in this test).
func TestASharingChangeNamesOnlyUuids(t *testing.T) {
	doc, person := "3f1c2a9e-8b7d-4c6e-9a5f-1d2e3c4b5a69", "7a8b9c0d-1e2f-4a3b-8c5d-6e7f8a9b0c1d"
	for name, in := range map[string]*adapter.InputUpdateDocPermissions{
		"a doc id with a query in it":    {DocId: doc + `") { x as var(func: has(user_uuid)) } #`},
		"a person's id with a query":     {DocId: doc, AddViewers: []string{person, `") } { y(func: has(doc_uuid)) { doc_body } } #`}},
		"an upper-case uuid, not as one": {DocId: "3F1C2A9E-8B7D-4C6E-9A5F-1D2E3C4B5A69"},
		"no doc at all":                  {AddEditors: []string{person}},
	} {
		if err := UpdateDocPermissions(context.Background(), in); !errors.Is(err, ErrSharingIDs) {
			t.Errorf("%s: %v, want ErrSharingIDs", name, err)
		}
	}
	if !sharingIDsValid(&adapter.InputUpdateDocPermissions{DocId: doc, AddEditors: []string{person}, RemoveViewers: []string{""}}) {
		t.Error("a doc and a person by their uuids are a valid change")
	}
}

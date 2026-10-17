//go:build integration
// +build integration

package integration

import (
	"context"
	"testing"
	"time"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/google/uuid"
)

// QA DSA-08 (14 Oct 2026): a doc's "edited" time disagreed in three places, and
// opening a seeded doc moved its list time with nobody editing. The doc's page
// read no edited time at all (getDocInfo left it out, so the page showed the
// created time), and the collaboration service's conversion as a doc loads
// (block ids given, old content moved over) was stored as an edit. Now the
// doc's details carry the edited time, a conversion is stored without moving
// it, and an edit still moves it.
func TestOpeningADocIsNotEditingIt(t *testing.T) {
	ctx := context.Background()
	dg := SetupDgraph(t)
	doc, sam := uuid.NewString(), uuid.NewString()
	earlier := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam, "user_name": "sam"},
		{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": doc, "doc_title": "Q4 launch plan", "doc_body": "<p>Ship</p>", "doc_private": false,
			"doc_created_at": earlier.Add(-time.Hour), "doc_updated_at": earlier, "doc_deleted_at": "0001-01-01T00:00:00Z",
			"doc_created_by": map[string]any{"uid": "_:sam"}},
	})
	editedAt := func() time.Time {
		t.Helper()
		d, err := docDomain.GetDgraphDocByUUID(ctx, doc, uids["sam"])
		if err != nil || d == nil || d.UpdatedAt == nil {
			t.Fatalf("the doc's details carry no edited time: %+v %v", d, err)
		}
		return d.UpdatedAt.UTC()
	}
	if got := editedAt(); !got.Equal(earlier) {
		t.Fatalf("the doc's details say edited %v, want %v", got, earlier)
	}

	// The service gives the blocks ids as the doc opens: stored, not an edit.
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: doc, Body: `<p data-block-id="b1">Ship</p>`, Snippet: "Ship", State: "eA==", Hash: "h1", Lineage: "L1", Conversion: true}); err != nil {
		t.Fatal(err)
	}
	if got := readDoc(t, doc); got["doc_body"] != `<p data-block-id="b1">Ship</p>` {
		t.Fatalf("the conversion was not stored: %v", got)
	}
	if got := editedAt(); !got.Equal(earlier) {
		t.Fatalf("opening the doc moved its edited time to %v", got)
	}

	// Sam edits it: the edited time moves.
	if _, err := docDomain.WriteCollabSave(ctx, docDomain.CollabWrite{DocUUID: doc, Body: `<p data-block-id="b1">Ship on Friday</p>`, Snippet: "Ship on Friday", State: "eQ==", Hash: "h2", Lineage: "L1", EditorUUID: sam}); err != nil {
		t.Fatal(err)
	}
	if got := editedAt(); !got.After(earlier) {
		t.Fatalf("an edit left the edited time at %v", got)
	}
}

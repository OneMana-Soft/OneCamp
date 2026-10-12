//go:build integration

package domain

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// The search repair names a doc too large to read by reading only its uuid,
// at the offset where the entry page failed (business/Doc
// repairDocSearchPage), and takes no doc there to mean the docs ran out. So
// at every offset that read names the doc the entry page reads, deleted docs
// included, and past the last doc it names none.
//
// Run: go test -tags=integration ./domain/Doc/ -run '^TestTheUUIDPageNamesTheDocsTheEntryPageReads$' -v
func TestTheUUIDPageNamesTheDocsTheEntryPageReads(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)
	seeded := map[string]bool{}
	var docs []map[string]any
	for i := 0; i < 4; i++ {
		id := uuid.NewString()
		seeded[id] = true
		d := map[string]any{"uid": fmt.Sprintf("_:d%d", i), "dgraph.type": "Doc", "doc_uuid": id, "doc_title": "Plan", "doc_body": "<p>Ship on Thursday.</p>"}
		if i == 2 {
			d["doc_deleted_at"] = time.Now().UTC().Format(time.RFC3339)
		}
		docs = append(docs, d)
	}
	dg.Mutate(t, docs)

	entries, err := GetDocSearchEntryPage(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := GetDocUUIDPage(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(entries) {
		t.Fatalf("the uuid page names %d docs, the entry page reads %d", len(ids), len(entries))
	}
	found := 0
	for i, e := range entries {
		if ids[i] != e.Uuid {
			t.Errorf("at %d the uuid page names %q, the entry page reads %q", i, ids[i], e.Uuid)
		}
		one, err := GetDocUUIDPage(ctx, i, 1)
		if err != nil || len(one) != 1 || one[0] != e.Uuid {
			t.Errorf("the uuid of the doc at %d alone: %v %v, want [%s]", i, one, err, e.Uuid)
		}
		if seeded[e.Uuid] {
			found++
		}
	}
	if found != len(seeded) {
		t.Errorf("%d of the %d docs made were read", found, len(seeded))
	}
	if past, err := GetDocUUIDPage(ctx, len(entries), 1); err != nil || len(past) != 0 {
		t.Errorf("past the last doc: %v %v, want none", past, err)
	}
}

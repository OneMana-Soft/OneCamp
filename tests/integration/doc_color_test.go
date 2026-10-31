//go:build integration
// +build integration

package integration_test

// Nodes coloured in bulk (docs F1, POST /doc/color's write), on a real graph
// with the production schema: one write colours every live doc named, a doc
// in the trash is left as it is, and no colour removes the predicate.
//
// Run: integ_rel.sh <wt> <log> '^TestNodesAreColouredInOneWrite$'

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	domainDoc "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestNodesAreColouredInOneWrite(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	live, gone := time.Time{}, time.Now().Add(-time.Hour)
	node := func(kind string, deleted *time.Time) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{Uid: "_:" + uuid.NewString()[:8], DType: []string{"Doc"}, Uuid: uuid.NewString(), Title: "n", NodeKind: kind, DeletedAt: deleted}
	}
	folder, page, trashed := node(domainDoc.NodeFolder, &live), node("", &live), node(domainDoc.NodeFolder, &gone)
	if _, err := dgexec.Set(ctx, []*dgraphStruct.DgraphDoc{folder, page, trashed}); err != nil {
		t.Fatal(err)
	}
	colourOf := func(id string) string {
		t.Helper()
		got, err := domainDoc.GetDocRowsByUUID(ctx, []string{id}, "0x1")
		if err != nil || len(got) != 1 {
			t.Fatalf("reading %s: %+v %v", id, got, err)
		}
		return got[0].Color
	}

	changed, err := domainDoc.WriteDocColors(ctx, []string{folder.Uuid, page.Uuid, trashed.Uuid}, "dusk")
	sort.Strings(changed)
	want := []string{folder.Uuid, page.Uuid}
	sort.Strings(want)
	if err != nil || len(changed) != 2 || changed[0] != want[0] || changed[1] != want[1] {
		t.Fatalf("colouring three, one in the trash: %v %v", changed, err)
	}
	if colourOf(folder.Uuid) != "dusk" || colourOf(page.Uuid) != "dusk" || colourOf(trashed.Uuid) != "" {
		t.Errorf("colours: %q %q %q", colourOf(folder.Uuid), colourOf(page.Uuid), colourOf(trashed.Uuid))
	}
	if changed, err := domainDoc.WriteDocColors(ctx, []string{folder.Uuid}, ""); err != nil || len(changed) != 1 {
		t.Fatalf("clearing: %v %v", changed, err)
	}
	if colourOf(folder.Uuid) != "" || colourOf(page.Uuid) != "dusk" {
		t.Errorf("after clearing the folder: %q %q", colourOf(folder.Uuid), colourOf(page.Uuid))
	}
}

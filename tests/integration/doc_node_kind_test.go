//go:build integration
// +build integration

package integration_test

// The three predicates Docs & Files adds (F1) on a real graph with the
// production schema: doc_node_kind's exact index finds a folder by its kind and
// a page by having none, all three are in type Doc, so a node's delete
// (S * *) takes them with the rest of the doc, and the chip read carries a
// link's address.
//
// Run: integ_rel.sh <wt> <log> '^TestDocNodePredicatesOnTheGraph$'

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	entityLinkDomain "github.com/akashc777/OneCamp/domain/EntityLink"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDocNodePredicatesOnTheGraph(t *testing.T) {
	ctx := context.Background()
	integration.SetupDgraph(t)
	if dgraphInit.DgraphClient == nil {
		t.Fatal("no graph")
	}
	live := time.Time{}
	node := func(kind, color, link string) *dgraphStruct.DgraphDoc {
		return &dgraphStruct.DgraphDoc{Uid: "_:" + uuid.NewString()[:8], DType: []string{"Doc"}, Uuid: uuid.NewString(),
			Title: "n", NodeKind: kind, Color: color, LinkURL: link, DeletedAt: &live}
	}
	page, folder := node("", "", ""), node("folder", "moss", "")
	file, link := node("file", "", ""), node("link", "sky", "https://example.com/plan")
	nodes := []*dgraphStruct.DgraphDoc{page, folder, file, link}
	uids, err := dgexec.Set(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.Uuid)
	}
	type row struct {
		UID   string `json:"uid"`
		UUID  string `json:"doc_uuid"`
		Kind  string `json:"doc_node_kind"`
		Color string `json:"doc_color"`
		Link  string `json:"doc_link_url"`
	}
	mine := fmt.Sprintf(`mine as var(func: eq(doc_uuid, [%q, %q, %q, %q]))`, ids[0], ids[1], ids[2], ids[3])
	folders, err := dgexec.List[row](ctx, `{
		`+mine+`
		got(func: eq(doc_node_kind, "folder")) @filter(uid(mine)) { uid doc_uuid doc_node_kind doc_color }
	}`, nil, "got")
	if err != nil || len(folders) != 1 || folders[0].UUID != folder.Uuid || folders[0].Color != "moss" {
		t.Fatalf("by the kind's index: %+v %v; want the folder alone", folders, err)
	}
	pages, err := dgexec.List[row](ctx, `{
		`+mine+`
		got(func: uid(mine)) @filter(NOT has(doc_node_kind)) { doc_uuid }
	}`, nil, "got")
	if err != nil || len(pages) != 1 || pages[0].UUID != page.Uuid {
		t.Fatalf("pages only: %+v %v; want the page alone", pages, err)
	}
	// The chip read carries a link's address, which a chip turns into the
	// link's family and never shows (docs F1b).
	chips, err := entityLinkDomain.DocChips(ctx, []string{link.Uuid})
	if err != nil || chips[link.Uuid] == nil || chips[link.Uuid].LinkURL != "https://example.com/plan" {
		t.Fatalf("the chip read of a link: %+v %v; want its address", chips[link.Uuid], err)
	}

	// A node's delete names no predicate: type Doc must hold all three.
	linkUID := uids[link.Uid[2:]]
	if linkUID == "" {
		t.Fatalf("no uid for the link in %v", uids)
	}
	if _, err := dgexec.Mutate(ctx, "", nil, dgexec.Mutation{DelNQuads: "<" + linkUID + "> * * ."}); err != nil {
		t.Fatal(err)
	}
	left, err := dgexec.List[row](ctx, `{
		got(func: uid(`+linkUID+`)) { doc_node_kind doc_color doc_link_url }
	}`, nil, "got")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range left {
		if r.Kind != "" || r.Color != "" || r.Link != "" {
			t.Errorf("a deleted link still holds %+v: a predicate isn't in type Doc", r)
		}
	}
}

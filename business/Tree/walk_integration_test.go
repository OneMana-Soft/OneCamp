//go:build integration

package business

// A walk over a subtree, against a real graph: the server's work over every
// page under one (a sharing change applied to its subpages, a page that takes
// its new parent's sharing, a copy) takes the whole subtree or refuses it.
// Until 16 Oct 2026 a walk stopped at the cap without a word, so a sharing
// change to the pages under one with more than 5,000 under it left the rest
// with the sharing they had, while a delete of the same page was refused.
//
// Run: go test -tags=integration ./business/Tree/ -run TestAWalkTakesTheWholeSubtreeOrRefusesIt -v

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAWalkTakesTheWholeSubtreeOrRefusesIt(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	live := "0001-01-01T00:00:00Z"
	made := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	root, section := uuid.NewString(), uuid.NewString()
	doc := func(key, id string, parent string) map[string]any {
		m := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id, "doc_title": key,
			"doc_deleted_at": live, "doc_created_at": made, "doc_updated_at": made,
			"doc_created_by": map[string]any{"uid": "_:owner"}, "doc_owner": map[string]any{"uid": "_:owner"}}
		if parent != "" {
			m["doc_parent"] = map[string]any{"uid": "_:" + parent}
		}
		return m
	}
	// A wiki with one section holding the rest: one page past the cap under
	// the wiki, over two levels.
	nodes := []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "owner"},
		doc("wiki", root, ""),
		doc("section", section, "wiki"),
	}
	for i := 0; i < subtreeCap; i++ {
		nodes = append(nodes, doc(fmt.Sprintf("p%d", i), uuid.NewString(), "section"))
	}
	uids := dg.Mutate(t, nodes)

	walked := 0
	err := Hierarchy.Walk(ctx, accessBusiness.KindDoc, root, func(Node) error { walked++; return nil })
	if !errors.Is(err, ErrTooMany) || walked != 0 {
		t.Fatalf("a walk over a page with %d under it (the cap is %d) visited %d and answered %v, want ErrTooMany before any",
			subtreeCap+1, subtreeCap, walked, err)
	}
	// What a sharing change applied to the subpages, and a page taking its new
	// parent's sharing, read (business/Access fullUnder).
	if got, err := accessBusiness.Subtree(ctx, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: root}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("the Access Resolver's subtree has %d pages and answered %v, want ErrTooMany", len(got), err)
	}

	// One of them in the trash leaves the cap exactly: walked whole, the page
	// first, and every page after the one it is under.
	dg.Mutate(t, map[string]any{"uid": uids["p0"], "doc_deleted_at": made})
	var seen []Node
	if err := Hierarchy.Walk(ctx, accessBusiness.KindDoc, root, func(n Node) error { seen = append(seen, n); return nil }); err != nil {
		t.Fatalf("a walk at the cap: %v", err)
	}
	if len(seen) != subtreeCap+1 || seen[0].UUID != root || seen[1].UUID != section || seen[1].ParentUUID != root {
		t.Fatalf("a walk at the cap visited %d pages, starting %+v", len(seen), seen[:min(len(seen), 2)])
	}
	for _, n := range seen[2:] {
		if n.ParentUUID != section {
			t.Fatalf("a page under the section was walked as under %q", n.ParentUUID)
		}
	}
	if got, err := accessBusiness.Subtree(ctx, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: root}); err != nil || len(got) != subtreeCap+1 {
		t.Fatalf("the Access Resolver's subtree at the cap has %d pages: %v", len(got), err)
	}
}

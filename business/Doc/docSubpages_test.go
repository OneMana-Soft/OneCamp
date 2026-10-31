package businness

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
)

const (
	pgMoved  = "88888888-8888-4888-8888-888888888888"
	pgFrom   = "99999999-9999-4999-8999-999999999999"
	pgTo     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	pgFolder = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	pgBinned = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	pgLater  = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
)

// blockEdits stands in for the collaboration service's sub-page ops, noting
// each as "remove|place <doc> <page> <by>".
type blockEdits struct {
	mu  sync.Mutex
	got []string
}

func (b *blockEdits) note(op, doc, page string, o collab.Options) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, op+" "+doc+" "+page+" "+o.By.UserUUID)
}

// edits waits for the background work, and says what it wrote since last asked.
func (b *blockEdits) edits(t *testing.T) []string {
	t.Helper()
	if left := helpers.SettleBackground(5 * time.Second); len(left) != 0 {
		t.Fatalf("still running: %v", left)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	got := b.got
	b.got = nil
	return got
}

// recordBlockEdits stands in for the sub-page ops, and for the read of the
// pages they are about: the rows as they are when it's made.
func recordBlockEdits(t *testing.T, rows map[string]*dgraphStruct.DgraphDoc) *blockEdits {
	t.Helper()
	b := &blockEdits{}
	remove, place, read := collab.RemoveSubpage, collab.PlaceSubpage, pageRows
	collab.RemoveSubpage = func(_ context.Context, doc, page string, o collab.Options) (collab.Result, error) {
		b.note("remove", doc, page, o)
		return collab.Result{Removed: 1}, nil
	}
	collab.PlaceSubpage = func(_ context.Context, doc, page string, o collab.Options) (collab.Result, error) {
		b.note("place", doc, page, o)
		return collab.Result{Blocks: 1}, nil
	}
	pageRows = func(_ context.Context, uuids []string, _ string) ([]*dgraphStruct.DgraphDoc, error) {
		var out []*dgraphStruct.DgraphDoc
		for _, id := range uuids {
			if r := rows[strings.ToLower(id)]; r != nil {
				out = append(out, r)
			}
		}
		return out, nil
	}
	t.Cleanup(func() { collab.RemoveSubpage, collab.PlaceSubpage, pageRows = remove, place, read })
	return b
}

// pages are the pages the cases move: each says where the moved one is when
// its follow-up reads it (under).
func pages(t *testing.T) (*blockEdits, func(page, parent string)) {
	gone := time.Now()
	rows := map[string]*dgraphStruct.DgraphDoc{}
	for _, d := range []*dgraphStruct.DgraphDoc{{Uuid: pgMoved}, {Uuid: pgFrom}, {Uuid: pgTo}, {Uuid: pgLater}, {Uuid: pgFolder, NodeKind: NodeFolder}, {Uuid: pgBinned, DeletedAt: &gone}} {
		rows[d.Uuid] = d
	}
	under := func(page, parent string) {
		rows[page].Parent = nil
		if parent != "" {
			rows[page].Parent = &dgraphStruct.DgraphDoc{Uuid: parent}
		}
	}
	return recordBlockEdits(t, rows), under
}

// mover is sam, who may edit every body but the ones named.
func mover(cannot ...string) Mover {
	return Mover{By: collab.By{UserUUID: sam}, UID: "0x5a", MayEdit: func(_ context.Context, doc string) bool { return !slices.Contains(cannot, doc) }}
}

// A page made under a page from the sidebar or Docs & Files takes its place at
// the end of its parent's body as a sub-page block, in its maker's name, as a
// page made inside a Notion page does (docs C7a). The editor's "/page" places
// its own block, so it takes none from here; nor does a folder or a link, nor
// a page under a folder, which has no body.
func TestAPageMadeUnderAPageTakesItsPlaceInIt(t *testing.T) {
	b := recordBlockEdits(t, nil)
	page := func(id string) *dgraphStruct.DgraphDoc { return &dgraphStruct.DgraphDoc{Uuid: id} }
	folder := &dgraphStruct.DgraphDoc{Uuid: d3, NodeKind: NodeFolder}
	by := collab.By{UserUUID: sam}

	PlaceInParent(context.Background(), NewDoc{}, page(d2), page(pgMoved), by)
	PlaceInParent(context.Background(), NewDoc{Placed: true}, page(d2), page(pgMoved), by)
	PlaceInParent(context.Background(), NewDoc{}, folder, page(pgMoved), by)
	PlaceInParent(context.Background(), NewDoc{}, page(d2), &dgraphStruct.DgraphDoc{Uuid: pgMoved, NodeKind: NodeFolder}, by)
	PlaceInParent(context.Background(), NewDoc{}, nil, page(pgMoved), by)

	if got, want := b.edits(t), []string{"place " + d2 + " " + pgMoved + " " + sam}; !slices.Equal(got, want) {
		t.Errorf("wrote %q; want one block, in the page's parent", got)
	}
}

// A page moved in the tree takes its block along (docs C7a): out of the body
// of the page it left, and onto the end of the one it went to, in the mover's
// name, as a page moved in Notion does. A body the mover may not edit keeps
// the block, which reads as a link to a page that lives elsewhere. Each body
// follows where the page is by then: a second move may have landed first.
func TestAMovedPageTakesItsBlockAlong(t *testing.T) {
	b, under := pages(t)
	remove := func(doc string) string { return "remove " + doc + " " + pgMoved + " " + sam }
	place := func(doc string) string { return "place " + doc + " " + pgMoved + " " + sam }
	for _, c := range []struct {
		name           string
		page, from, to string
		m              Mover
		now            string // where the page is when the follow-up reads it, when not to
		want           []string
	}{
		{"from one page to another", pgMoved, pgFrom, pgTo, mover(), "", []string{remove(pgFrom), place(pgTo)}},
		{"out of a page whose body the mover may not edit", pgMoved, pgFrom, pgTo, mover(pgFrom), "", []string{place(pgTo)}},
		{"from the top", pgMoved, "", pgTo, mover(), "", []string{place(pgTo)}},
		{"to the top", pgMoved, pgFrom, "", mover(), "", []string{remove(pgFrom)}},
		{"out of a folder, which has no body", pgMoved, pgFolder, pgTo, mover(), "", []string{place(pgTo)}},
		{"into a folder", pgMoved, pgFrom, pgFolder, mover(), "", []string{remove(pgFrom)}},
		{"out of a page in the trash", pgMoved, pgBinned, pgTo, mover(), "", []string{place(pgTo)}},
		{"a folder moved, which takes no block", pgFolder, pgFrom, pgTo, mover(), "", nil},
		{"among the same page's children", pgMoved, pgFrom, strings.ToUpper(pgFrom), mover(), "", nil},
		{"moved on again before this ran", pgMoved, pgFrom, pgTo, mover(), pgLater, []string{remove(pgFrom)}},
		{"moved back again before this ran", pgMoved, pgFrom, pgTo, mover(), pgFrom, nil},
	} {
		now := c.to
		if c.now != "" {
			now = c.now
		}
		under(c.page, now)
		FollowMove(context.Background(), c.page, c.from, c.to, c.m)
		if got := b.edits(t); !slices.Equal(got, c.want) {
			t.Errorf("%s: wrote %q, want %q", c.name, got, c.want)
		}
	}
}

// A page brought back from the trash shows in its parent's body again (docs
// C7a): its block at the end, unless the body still holds one (a page trashed
// from the tree keeps its block, reading "Deleted page"), which the
// collaboration service checks as it writes. A parent whose body the person
// may not edit lists it under "Pages inside" instead.
func TestARestoredPageShowsInItsParentAgain(t *testing.T) {
	b, under := pages(t)
	for _, c := range []struct {
		name string
		res  Restored
		m    Mover
		now  string // where the page is when the follow-up reads it, when not res.ParentUUID
		want []string
	}{
		{"under a page", Restored{Top: pgMoved, ParentUUID: pgTo}, mover(), "", []string{"place " + pgTo + " " + pgMoved + " " + sam}},
		{"under a page whose body they may not edit", Restored{Top: pgMoved, ParentUUID: pgTo}, mover(pgTo), "", nil},
		{"at the top, its parent gone", Restored{Top: pgMoved}, mover(), "", nil},
		{"under a folder", Restored{Top: pgMoved, ParentUUID: pgFolder}, mover(), "", nil},
		{"a folder", Restored{Top: pgFolder, ParentUUID: pgTo}, mover(), "", nil},
		{"moved on before this ran", Restored{Top: pgMoved, ParentUUID: pgTo}, mover(), pgLater, nil},
	} {
		now := c.res.ParentUUID
		if c.now != "" {
			now = c.now
		}
		under(c.res.Top, now)
		FollowRestore(context.Background(), &c.res, c.m)
		if got := b.edits(t); !slices.Equal(got, c.want) {
			t.Errorf("%s: wrote %q, want %q", c.name, got, c.want)
		}
	}
}

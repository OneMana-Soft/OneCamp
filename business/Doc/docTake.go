package businness

// What a person takes with a subtree: the owner's rule, "protect what they
// can't see" (15 Oct 2026).
//
// Deleting a page used to take every page under it, the ones the person
// couldn't open included: a private page someone kept under a shared one went
// to the trash, and then for good, by the hand of someone who never knew it
// was there. Now one function decides, for a delete and for a delete for good
// (takeFor): from the page acted on down, a page the person may act on goes
// with it, and a page they may not stays, with everything under it, moving up
// to the parent of the page acted on, its own sharing kept. What "may" means
// is the action's, by the person's decision on each page (the one rule,
// DecideDocs, ended grants included): opening it now for a delete
// (takesOnDelete), and having had full access to it for a delete for good
// (takesOnPurge). An admin's say lets them act on the page they chose; it
// never takes a page under it for them.
//
// A restore takes no decision of its own: it brings back exactly what one
// delete took (the batch), which this function decided when it was made.
// Pages that stayed were never deleted, so restoring their old parent leaves
// them where they moved: they may have been reorganised since, and pulling
// them back would undo that without a word. Undo straight after a delete
// leaves them there too, right after the restored page.

import (
	"context"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	"github.com/akashc777/OneCamp/business/Tree/position"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// treePage is a page of a subtree as the walk knows it.
type treePage struct {
	UID, UUID string
	// ParentUUID is the page it sits under ("" for the root).
	ParentUUID string
	// Position and CreatedAt order it among its siblings (position.Order),
	// which keeps the order of the pages that move up.
	Position  string
	CreatedAt time.Time
}

// subtreeTake is what one person's action takes of a subtree.
type subtreeTake struct {
	// Takes are the pages that go, the root first, every page after the page
	// it sits under.
	Takes []treePage
	// Stays are the tops of what stays: pages the person may not act on whose
	// parent goes. Each moves up to the root's parent with everything under
	// it. They are in the order the tree shows them, top to bottom.
	Stays []treePage
	// With is, for each top of what stays (by uuid), the pages that stay with
	// it, the top included, every page after the page it sits under.
	With map[string][]treePage
}

// Which pages under the page acted on an action takes, by the person's
// decision on each.
var (
	// takesOnDelete: a page they can open now.
	takesOnDelete = func(d DocDecision) bool { return d.Level >= level.View }
	// takesOnPurge: a page they had full access to before it was deleted.
	takesOnPurge = func(d DocDecision) bool { return d.Was >= PurgeNeeds }
)

// takeFor is what the person takes of a subtree: their decision on every page
// under root, read once by the one rule (DecideDocs), put to the walk
// (takeSubtree). A page with no decision (gone meanwhile) stays.
func takeFor(ctx context.Context, v Viewer, root treePage, under []treePage, may func(DocDecision) bool) (subtreeTake, error) {
	decided, err := DecideDocs(ctx, v, uuidsOf(under))
	if err != nil {
		return subtreeTake{}, err
	}
	return takeSubtree(root, under, func(p treePage) bool {
		return may(decided[strings.ToLower(p.UUID)])
	}), nil
}

// takeSubtree walks a subtree from its root, top to bottom as the tree shows
// it: the root goes (the caller has decided the person may act on it), and
// below it each page goes when may says the person may act on it, and stays
// with everything under it when not. under is every page below the root; a
// page whose parent isn't the root or in under is ignored. Pure.
func takeSubtree(root treePage, under []treePage, may func(page treePage) bool) subtreeTake {
	kids := map[string][]treePage{}
	for _, p := range under {
		if p.UUID != "" && p.UUID != root.UUID {
			kids[p.ParentUUID] = append(kids[p.ParentUUID], p)
		}
	}
	for _, sibs := range kids {
		inShownOrder(sibs)
	}
	all := func(top treePage) []treePage {
		out := []treePage{top}
		for i := 0; i < len(out); i++ {
			out = append(out, kids[out[i].UUID]...)
		}
		return out
	}
	t := subtreeTake{With: map[string][]treePage{}}
	var walk func(p treePage)
	walk = func(p treePage) {
		t.Takes = append(t.Takes, p)
		for _, k := range kids[p.UUID] {
			if may(k) {
				walk(k)
				continue
			}
			t.Stays = append(t.Stays, k)
			t.With[k.UUID] = all(k)
		}
	}
	walk(root)
	return t
}

// inShownOrder sorts one level's pages as the tree shows them
// (position.Order).
func inShownOrder(pages []treePage) {
	sibs := make([]position.Sibling, len(pages))
	byID := make(map[string]treePage, len(pages))
	for i, p := range pages {
		sibs[i] = position.Sibling{ID: p.UUID, Position: p.Position, CreatedAt: p.CreatedAt}
		byID[p.UUID] = p
	}
	position.Order(sibs)
	for i, s := range sibs {
		pages[i] = byID[s.ID]
	}
}

// pageOf is a doc as the walk knows it.
func pageOf(d *dgraphStruct.DgraphDoc) treePage {
	p := treePage{UID: d.Uid, UUID: d.Uuid, Position: d.Position}
	if d.Parent != nil {
		p.ParentUUID = d.Parent.Uuid
	}
	if d.CreatedAt != nil {
		p.CreatedAt = *d.CreatedAt
	}
	return p
}

// uidsOf is the pages' graph uids.
func uidsOf(pages []treePage) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.UID)
	}
	return out
}

// uuidsOf is the pages' ids.
func uuidsOf(pages []treePage) []string {
	out := make([]string, 0, len(pages))
	for _, p := range pages {
		out = append(out, p.UUID)
	}
	return out
}

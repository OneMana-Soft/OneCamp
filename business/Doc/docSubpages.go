package businness

// Sub-pages in step with the tree (docs C7a): a page made, moved or brought
// back under a page shows in that page's body as its sub-page block, as a
// Notion page does. The blocks are written through the Live Doc Writer's
// sub-page ops (collab.PlaceSubpage, RemoveSubpage), in the name of the person
// who changed the tree, in the background: the tree change stands whatever
// becomes of them, and a block left behind reads as a link to a page that
// lives elsewhere, as every block did before C7a. Only pages: a folder has no
// body, and a folder, a link or a file takes no block.

import (
	"context"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
)

// Mover is who changed the tree, for the body edits that follow: the name
// they are written in, the person's graph uid to read the pages with, and
// whether they may edit a page's body, as the caller's Access Resolver says.
type Mover struct {
	By      collab.By
	UID     string
	MayEdit func(ctx context.Context, docUUID string) bool
}

// Seam for tests: the pages a tree change's body edits are about.
var pageRows = DocRows

// PlaceInParent puts a page made under a page at the end of its parent's
// body, as the sub-page block the editor's "/page" places: a page made from
// the sidebar or Docs & Files appears in its parent, as one made inside a
// Notion page does. A page under a folder takes none, nor does one whose
// maker places its block itself (in.Placed). The maker may edit the parent:
// a page is made under one only then.
func PlaceInParent(ctx context.Context, in NewDoc, parent, made *dgraphStruct.DgraphDoc, by collab.By) {
	if in.Placed || !IsPage(parent) || !IsPage(made) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("doc.PlaceInParent", func() { placeBlock(ctx, parent.Uuid, made.Uuid, by) })
}

// FollowMove keeps a moved page's block with it: out of the body of the page
// it left, when the mover may edit that body, and at the end of the one it
// went to, which the move needed them to edit. A move among the same page's
// children changes no body. Each body follows where the page is when this
// runs, so a second move landing first (A to B, then B to C) leaves nothing
// in B.
func FollowMove(ctx context.Context, page, from, to string, m Mover) {
	if strings.EqualFold(from, to) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("doc.FollowMove", func() {
		rows, ok := pagesOf(ctx, m, page, from, to)
		if !ok || !livePage(rows, page) {
			return
		}
		now := parentUUIDOf(rows[strings.ToLower(page)])
		if livePage(rows, from) && !strings.EqualFold(now, from) && m.MayEdit(ctx, from) {
			removeBlock(ctx, from, page, m.By)
		}
		if livePage(rows, to) && strings.EqualFold(now, to) {
			placeBlock(ctx, to, page, m.By)
		}
	})
}

// FollowRestore shows a page brought back from the trash in its parent's body
// again, when the person may edit that body: a page put in the trash from the
// tree kept its block, which read "Deleted page", and one whose block was
// deleted gets one at the end. A parent whose body they may not edit lists it
// under "Pages inside" instead.
func FollowRestore(ctx context.Context, res *Restored, m Mover) {
	if res == nil || res.Top == "" || res.ParentUUID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("doc.FollowRestore", func() {
		rows, ok := pagesOf(ctx, m, res.Top, res.ParentUUID)
		if ok && livePage(rows, res.Top) && livePage(rows, res.ParentUUID) &&
			strings.EqualFold(parentUUIDOf(rows[strings.ToLower(res.Top)]), res.ParentUUID) && m.MayEdit(ctx, res.ParentUUID) {
			placeBlock(ctx, res.ParentUUID, res.Top, m.By)
		}
	})
}

// pagesOf reads the docs named ("" names none), by lower-case uuid.
func pagesOf(ctx context.Context, m Mover, uuids ...string) (map[string]*dgraphStruct.DgraphDoc, bool) {
	var named []string
	for _, id := range uuids {
		if id != "" {
			named = append(named, id)
		}
	}
	rows, err := pageRows(ctx, named, m.UID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/pagesOf %v: %v", named, err)
		return nil, false
	}
	byID := make(map[string]*dgraphStruct.DgraphDoc, len(rows))
	for _, r := range rows {
		byID[strings.ToLower(r.Uuid)] = r
	}
	return byID, true
}

// livePage is whether the doc read is a page out of the trash: one whose body
// shows its sub-pages.
func livePage(rows map[string]*dgraphStruct.DgraphDoc, uuid string) bool {
	d := rows[strings.ToLower(uuid)]
	return IsPage(d) && !IsDeleted(d)
}

// placeBlock puts the page's block at the end of the parent's body, unless
// the body holds one already.
func placeBlock(ctx context.Context, parent, page string, by collab.By) {
	if _, err := collab.PlaceSubpage(ctx, parent, page, collab.Options{By: by}); err != nil && !errors.Is(err, collab.ErrHasContent) {
		helpers.LogErrorWithContext(ctx, "business/placeBlock: the block of %s in %s: %v", page, parent, err)
	}
}

// removeBlock takes the page's blocks out of the body of a page it left.
func removeBlock(ctx context.Context, doc, page string, by collab.By) {
	if _, err := collab.RemoveSubpage(ctx, doc, page, collab.Options{By: by}); err != nil && !errors.Is(err, collab.ErrNoSuchBlock) {
		helpers.LogErrorWithContext(ctx, "business/removeBlock: the blocks of %s in %s: %v", page, doc, err)
	}
}

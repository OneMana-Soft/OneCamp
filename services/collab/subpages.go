package collab

import "context"

// A page's sub-page blocks, kept in step with the tree (docs C7a; the
// collaboration service's live/ops/subpages.js): every block of the page, at
// any depth, so a block in a column or a callout follows the page too.
// Variables, so a test can stand in for the collaboration service.

// RemoveSubpage takes every block of the page out of the doc: the page left
// it. ErrNoSuchBlock when the doc holds none.
var RemoveSubpage = func(ctx context.Context, docUUID, pageUUID string, o Options) (Result, error) {
	return write(ctx, docUUID, "removeSubpage", wire{DocID: pageUUID}, o)
}

// PlaceSubpage puts the page's block at the end of the doc: the page is in it
// now. ErrHasContent when the doc holds a block of the page already, a link
// placed earlier or this block, so it never takes a second.
var PlaceSubpage = func(ctx context.Context, docUUID, pageUUID string, o Options) (Result, error) {
	return write(ctx, docUUID, "placeSubpage", wire{DocID: pageUUID}, o)
}

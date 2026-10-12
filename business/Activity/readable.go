package business

// Feed items about a doc or a board are kept only if the reader can read it
// now.
//
// A comment's mentions are stored for everyone it names, whether or not they
// may read the doc or board it is on, and the feed's queries hold such a
// comment to its doc's or board's deletion alone (posts, chats and tasks they
// hold to membership). So someone named in a comment on a private doc they
// couldn't read, or taken off the doc later, found the comment's text and the
// doc's current title in their feed: the live event was held back
// (business/Doc PublishDocCommentActivity), the next load wasn't. The doc and
// the board decide, as they do in global search: business/Doc ReadableDocsFor
// and business/Board ReadableBoardsFor, each asked once a page. Both packages
// import this one, so they hand their checks over when they start
// (RegisterDocReadCheck, RegisterBoardReadCheck). Until they have, and on a
// page the graph can't answer for, no item about a doc or a board is kept:
// refusing costs a feed item, keeping a wrong one costs a private doc.

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// ReadCheck answers, of the named docs or boards, which the person (by uuid)
// may read now, keyed by lower-case uuid. An error says nothing about any.
type ReadCheck func(ctx context.Context, ids []string, userUUID string) (map[string]bool, error)

// readableDocs and readableBoards are the doc's and the board's read checks.
var readableDocs, readableBoards ReadCheck

// RegisterDocReadCheck hands the feed the doc read check; business/Doc does
// when it starts.
func RegisterDocReadCheck(check ReadCheck) { readableDocs = check }

// RegisterBoardReadCheck hands the feed the board read check; business/Board
// does when it starts.
func RegisterBoardReadCheck(check ReadCheck) { readableBoards = check }

// about is what a feed item is about that has a read rule of its own: the doc
// or the board a comment is on.
type about struct{ doc, board string }

func commentAbout(c *dgraphStruct.DgraphComment) about {
	var a about
	if c == nil {
		return a
	}
	if c.Doc != nil {
		a.doc = c.Doc.Uuid
	}
	if c.Board != nil {
		a.board = c.Board.Uuid
	}
	return a
}

func mentionAbout(m *dgraphStruct.DgraphMentions) about {
	if m == nil {
		return about{}
	}
	return commentAbout(m.Comment)
}

func reactionAbout(r *dgraphModels.ReactionsActivity) about {
	if r == nil {
		return about{}
	}
	return commentAbout(r.Comment)
}

func itemAbout(it dgraphModels.UnifiedActivityItem) about {
	switch {
	case it.Mention != nil:
		return mentionAbout(it.Mention)
	case it.Comment != nil:
		return commentAbout(it.Comment)
	case it.Reaction != nil:
		return reactionAbout(it.Reaction)
	}
	return about{}
}

// keepReadable returns the items without those about a doc or a board the
// reader (by uuid) can't read now, asking about all the docs once and all the
// boards once. Items about anything else are kept as they are.
func keepReadable[T any](ctx context.Context, readerUUID string, items []T, of func(T) about) []T {
	var docs, boards []string
	seenDoc, seenBoard := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		a := of(it)
		if d := strings.ToLower(a.doc); d != "" && !seenDoc[d] {
			seenDoc[d] = true
			docs = append(docs, d)
		}
		if b := strings.ToLower(a.board); b != "" && !seenBoard[b] {
			seenBoard[b] = true
			boards = append(boards, b)
		}
	}
	if len(docs) == 0 && len(boards) == 0 {
		return items
	}
	docOK := ask(ctx, "docs", readableDocs, docs, readerUUID)
	boardOK := ask(ctx, "boards", readableBoards, boards, readerUUID)
	kept := make([]T, 0, len(items))
	dropped := 0
	for _, it := range items {
		a := of(it)
		if d := strings.ToLower(a.doc); d != "" && !docOK[d] {
			dropped++
			continue
		}
		if b := strings.ToLower(a.board); b != "" && !boardOK[b] {
			dropped++
			continue
		}
		kept = append(kept, it)
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "activity: left out %d items on docs or boards the reader can't read", dropped)
	}
	return kept
}

// ask runs a read check over ids; none is readable when there is no check or
// it fails.
func ask(ctx context.Context, what string, check ReadCheck, ids []string, readerUUID string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	if check == nil {
		helpers.LogErrorWithContext(ctx, "activity: no read check for %s, leaving the items on %d of them out", what, len(ids))
		return nil
	}
	ok, err := check(ctx, ids, readerUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "activity: could not check %d %s against the graph, leaving their items out: %v", len(ids), what, err)
		return nil
	}
	return ok
}

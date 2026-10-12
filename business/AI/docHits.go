package business

// Search hits on a doc are kept only if the person can read the doc now.
//
// The AI search index carries each doc's privacy and sharing so it can filter
// hits (services/AI buildPermissionFilter), but that copy is written when the
// doc changes, after it changes, and for a long time a save from the editor
// wrote every doc as public, and an edited comment its doc as public. So the
// doc decides: a hit on a doc, or on a comment on one, is kept only if the
// doc's own read rule (docBusiness.CanRead) says the person may read it. One
// that can't be read back is dropped: refusing costs a search result, keeping
// a wrong one costs a private doc.
//
// The two searches across the workspace go through here; the ones narrowed to
// a channel or a conversation (ai.SearchRecent) can't return a doc.
// TestWorkspaceSearchesGoThroughTheDocCheck keeps it that way.
//
// A search an AI agent makes for someone other than its sponsor runs as the
// sponsor, with the asker's own permission filter added in the index (services/AI
// runRequester.go). That half rests on the same index copy, so the doc decides
// for the asker too: a hit is kept only if the doc, read for each of them, says
// both may read it.

import (
	"context"
	"fmt"
	"strings"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// searchSimilar is ai.SearchSimilar for the person, without the doc hits they
// can't read.
func searchSimilar(ctx context.Context, userInfo *userModels.UserInfo, query string, channels, projects, grpIDs []string, limit int) ([]ai.SimilarResult, error) {
	res, err := ai.SearchSimilar(ctx, query, userInfo.UserDgraphInfo.Uuid, channels, projects, grpIDs, limit)
	if err != nil {
		return nil, err
	}
	return keepReadableDocs(ctx, userInfo, res), nil
}

// searchRecentGlobal is ai.SearchRecentGlobal for the person, without the doc
// hits they can't read.
func searchRecentGlobal(ctx context.Context, userInfo *userModels.UserInfo, channels, projects, grpIDs []string, limit int) ([]ai.SimilarResult, error) {
	res, err := ai.SearchRecentGlobal(ctx, userInfo.UserDgraphInfo.Uuid, channels, projects, grpIDs, limit)
	if err != nil {
		return nil, err
	}
	return keepReadableDocs(ctx, userInfo, res), nil
}

// fetchUnreadAcrossScopes is ai.FetchUnreadAcrossScopes (the workspace-wide
// catch-up window) for the person, without the docs, comments on docs and
// comments on boards they can't read.
func fetchUnreadAcrossScopes(ctx context.Context, userInfo *userModels.UserInfo, channels, projects, grpIDs []string, sinceUnix int64, limit int) ([]ai.ScopedContent, error) {
	items, err := ai.FetchUnreadAcrossScopes(ctx, userInfo.UserDgraphInfo.Uuid, channels, projects, grpIDs, sinceUnix, limit)
	if err != nil {
		return nil, err
	}
	return keepReadable(ctx, userInfo, items, func(c ai.ScopedContent) readRef {
		return refOf(c.ContentType, c.ContentUUID, c.DocUUID, c.BoardUUID)
	}), nil
}

// readDocFor reads a doc as the person sees it. A seam for tests.
var readDocFor = func(ctx context.Context, docUUID, userUID string) (*dgraphStruct.DgraphDoc, error) {
	return docBusiness.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
}

// readableBoards answers which of the boards a person (by uuid) may read now,
// a deleted board being readable by nobody: boardBusiness.ReadableBoardsFor
// reads each board's deletion, which its basic read (GetBasicBoardByUUID, used
// here before) leaves out. A seam for tests.
var readableBoards = boardBusiness.ReadableBoardsFor

// keepReadableDocs returns the results without the hits on a doc, or on a
// comment on one, that the person can't read now, nor, for an agent run acting
// for someone else, that person (docReaders). Each doc is read once per reader.
//
// A comment on a board is held to the board the same way: its entry carries a
// copy of the board's privacy and sharing from when the comment was written,
// and making the board private or no longer sharing it doesn't reach it, nor
// does deleting it when the delete's cascade misses the entry.
func keepReadableDocs(ctx context.Context, userInfo *userModels.UserInfo, results []ai.SimilarResult) []ai.SimilarResult {
	return keepReadable(ctx, userInfo, results, func(r ai.SimilarResult) readRef {
		return refOf(r.ContentType, r.ContentUUID, r.DocUUID, r.BoardUUID)
	})
}

// readRef is what an item is about, when that has a reader rule of its own:
// the doc (its own, or the one a comment is on) and the board a comment is on.
type readRef struct{ doc, board string }

// refOf reads an item's readRef from its index fields. Only doc comments carry
// doc_uuid and only board comments board_uuid.
func refOf(contentType, contentUUID, docUUID, boardUUID string) readRef {
	switch contentType {
	case "doc":
		return readRef{doc: contentUUID}
	case "comment":
		return readRef{doc: docUUID, board: boardUUID}
	}
	return readRef{}
}

// keepReadable drops the items about a doc or a board that a reader can't read
// now, asking each doc and board once.
func keepReadable[T any](ctx context.Context, userInfo *userModels.UserInfo, items []T, ref func(T) readRef) []T {
	if len(items) == 0 {
		return items
	}
	readers, known := docReaders(ctx, userInfo)
	readable := map[string]bool{}
	allowed := func(key string, check func() bool) bool {
		ok, asked := readable[key]
		if !asked {
			ok = known && check()
			readable[key] = ok
		}
		return ok
	}
	kept := make([]T, 0, len(items))
	for _, it := range items {
		r := ref(it)
		if r.doc != "" && !allowed("doc:"+r.doc, func() bool { return readableByAll(ctx, r.doc, readers) }) {
			continue
		}
		if r.board != "" && !allowed("board:"+r.board, func() bool { return boardReadableByAll(ctx, r.board, readers) }) {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}

// boardReadableByAll reports whether every reader may read the board now, read
// for each with its deletion: a deleted board's comments, which the delete's
// cascade missed, were still quoted, since the board read as live.
func boardReadableByAll(ctx context.Context, boardUUID string, readers []docReader) bool {
	for _, p := range readers {
		readable, err := readableBoards(ctx, []string{boardUUID}, p.uuid)
		if err != nil || !readable[strings.ToLower(boardUUID)] {
			return false
		}
	}
	return len(readers) > 0
}

// docReader is a person a doc hit must be readable by: their graph uid, which a
// doc is read for (its sharing counts are that reader's), and their uuid.
type docReader struct{ uid, uuid string }

// docReaders is who a doc hit must be readable by: the person searching and,
// when an agent run acts for someone other than its sponsor, that person too.
// known is false for a run acting for someone who can't be resolved, and then
// no doc hit is kept.
func docReaders(ctx context.Context, userInfo *userModels.UserInfo) (readers []docReader, known bool) {
	readers = []docReader{{uid: userInfo.UserDgraphInfo.Uid, uuid: userInfo.UserDgraphInfo.Uuid}}
	requester, _, forOther := ai.RunRequester(ctx)
	if !forOther {
		return readers, true
	}
	if requester = strings.TrimSpace(requester); requester == "" {
		return nil, false
	}
	asker, err := ai.RunMemo(ctx, "person:"+requester, func() (*dgraphStruct.DgraphUser, error) {
		asker, err := lookupPerson(ctx, requester)
		if err != nil || asker == nil || asker.Uid == "" {
			return nil, fmt.Errorf("the person who asked could not be resolved")
		}
		return asker, nil
	})
	if err != nil {
		return nil, false
	}
	return append(readers, docReader{uid: asker.Uid, uuid: requester}), true
}

// readableByAll reports whether every reader may read the doc, read for each,
// and the doc isn't deleted. Vector searches drop deleted docs again on their
// own (ai.KeepLive); the catch-up window doesn't, and an entry whose doc was
// deleted without the index hearing of it would otherwise still be quoted.
func readableByAll(ctx context.Context, docUUID string, readers []docReader) bool {
	for _, p := range readers {
		doc, err := readDocFor(ctx, docUUID, p.uid)
		if err != nil || !docBusiness.CanRead(doc, p.uuid) || helpers.IsSoftDeleted(doc.DeletedAt) {
			return false
		}
	}
	return len(readers) > 0
}

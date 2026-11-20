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

// readableDocs answers which of the docs a person (by uuid) may read now, a
// deleted doc being readable by nobody: docBusiness.ReadableDocsFor reads
// them all at once. A seam for tests.
var readableDocs = docBusiness.ReadableDocsFor

// readableBoards answers which of the boards a person (by uuid) may read now,
// a deleted board being readable by nobody: boardBusiness.ReadableBoardsFor
// reads them all, deletion included, at once. A seam for tests.
var readableBoards = boardBusiness.ReadableBoardsFor

// keepReadableDocs returns the results without the hits on a doc, or on a
// comment on one, that the person can't read now, nor, for an agent run acting
// for someone else, that person (docReaders). Each reader is asked once about
// every doc, and once about every board.
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
// doc_uuid; board comments carry board_uuid, and a board's own entry (what it
// says) is the board.
func refOf(contentType, contentUUID, docUUID, boardUUID string) readRef {
	switch contentType {
	case "doc":
		return readRef{doc: contentUUID}
	case ai.BoardContentType:
		return readRef{board: contentUUID}
	case "comment":
		return readRef{doc: docUUID, board: boardUUID}
	}
	return readRef{}
}

// keepReadable drops the items about a doc or a board that a reader can't read
// now: each reader is asked once about all the docs and once about all the
// boards, so N hits cost one doc call and one board call per reader.
func keepReadable[T any](ctx context.Context, userInfo *userModels.UserInfo, items []T, ref func(T) readRef) []T {
	if len(items) == 0 {
		return items
	}
	var docs, boards []string
	named := map[string]bool{}
	for _, it := range items {
		r := ref(it)
		if r.doc != "" && !named["doc:"+strings.ToLower(r.doc)] {
			named["doc:"+strings.ToLower(r.doc)] = true
			docs = append(docs, r.doc)
		}
		if r.board != "" && !named["board:"+strings.ToLower(r.board)] {
			named["board:"+strings.ToLower(r.board)] = true
			boards = append(boards, r.board)
		}
	}
	var docOK, boardOK map[string]bool
	if len(docs)+len(boards) > 0 {
		readers, known := docReaders(ctx, userInfo)
		if !known {
			readers = nil
		}
		docOK = readableByAll(ctx, docs, readers, readableDocs)
		boardOK = readableByAll(ctx, boards, readers, readableBoards)
	}
	kept := make([]T, 0, len(items))
	for _, it := range items {
		r := ref(it)
		if r.doc != "" && !docOK[strings.ToLower(r.doc)] {
			continue
		}
		if r.board != "" && !boardOK[strings.ToLower(r.board)] {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}

// readableByAll answers, of the ids (docs, or boards), which every reader may
// read now, keyed by lower-case uuid, asking read once per reader for the lot:
// docBusiness.ReadableDocsFor or boardBusiness.ReadableBoardsFor, each of which
// holds a deleted one readable by nobody. A reader whose call fails reads none
// of them, so an error drops every hit of that kind, and with no readers (one
// who can't be resolved) none is readable.
func readableByAll(ctx context.Context, ids []string, readers []docReader, read func(context.Context, []string, string) (map[string]bool, error)) map[string]bool {
	ok := map[string]bool{}
	if len(ids) == 0 || len(readers) == 0 {
		return ok
	}
	for _, id := range ids {
		ok[strings.ToLower(id)] = true
	}
	for _, p := range readers {
		readable, err := read(ctx, ids, p.uuid)
		if err != nil {
			return map[string]bool{}
		}
		for id := range ok {
			if !readable[id] {
				delete(ok, id)
			}
		}
	}
	return ok
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

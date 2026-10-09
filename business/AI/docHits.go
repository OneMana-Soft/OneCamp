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

// readDocFor reads a doc as the person sees it. A seam for tests.
var readDocFor = func(ctx context.Context, docUUID, userUID string) (*dgraphStruct.DgraphDoc, error) {
	return docBusiness.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
}

// keepReadableDocs returns the results without the hits on a doc, or on a
// comment on one, that the person can't read now, nor, for an agent run acting
// for someone else, that person (docReaders). Each doc is read once per reader.
func keepReadableDocs(ctx context.Context, userInfo *userModels.UserInfo, results []ai.SimilarResult) []ai.SimilarResult {
	if len(results) == 0 {
		return results
	}
	readers, known := docReaders(ctx, userInfo)
	readable := map[string]bool{}
	kept := make([]ai.SimilarResult, 0, len(results))
	for _, r := range results {
		if docUUID := docOfHit(r); docUUID != "" {
			ok, asked := readable[docUUID]
			if !asked {
				ok = known && readableByAll(ctx, docUUID, readers)
				readable[docUUID] = ok
			}
			if !ok {
				continue
			}
		}
		kept = append(kept, r)
	}
	return kept
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

// readableByAll reports whether every reader may read the doc, read for each.
func readableByAll(ctx context.Context, docUUID string, readers []docReader) bool {
	for _, p := range readers {
		doc, err := readDocFor(ctx, docUUID, p.uid)
		if err != nil || !docBusiness.CanRead(doc, p.uuid) {
			return false
		}
	}
	return len(readers) > 0
}

// docOfHit is the doc a hit is about: the doc itself, or the doc a comment is
// on (only doc comments carry doc_uuid); "" for anything else.
func docOfHit(r ai.SimilarResult) string {
	if r.ContentType == "doc" {
		return r.ContentUUID
	}
	return r.DocUUID
}

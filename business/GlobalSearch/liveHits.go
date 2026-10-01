package business

import (
	"context"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

// Global search reads the indices, which can hold content the databases no
// longer have: a restore to an earlier point brings the databases back and
// leaves the indices as they were, and a delete can fail half way. A result
// like that opened nothing. Each page is now checked where the content lives
// (domain/Liveness, as AI answers are): content that is gone is dropped and
// its entry forgotten, soft-deleted content is dropped, and a check that
// cannot run keeps its results.

// hitIndex is the index each checked type is stored in, for forgetting it.
var hitIndex = map[string]string{
	openSearchStruct.CHAT_TYPE:    openSearchStruct.CHAT_INDEX,
	openSearchStruct.POST_TYPE:    openSearchStruct.POST_INDEX,
	openSearchStruct.COMMENT_TYPE: openSearchStruct.COMMENT_INDEX,
	openSearchStruct.TASK_TYPE:    openSearchStruct.TASK_INDEX,
	openSearchStruct.DOC_TYPE:     openSearchStruct.DOC_INDEX,
}

// hitKey is a hit's type and the id of what it points at.
func hitKey(h *openSearchStruct.GlobalSearchOpenSearchResp) (string, string) {
	if h == nil {
		return "", ""
	}
	switch {
	case h.Chat != nil:
		return h.Type, h.Chat.Uuid
	case h.Post != nil:
		return h.Type, h.Post.Uuid
	case h.Comment != nil:
		return h.Type, h.Comment.Uuid
	case h.Task != nil:
		return h.Type, h.Task.Uuid
	case h.Doc != nil:
		return h.Type, h.Doc.Uuid
	}
	return h.Type, ""
}

// liveCheckers and forgetFn are seams for tests.
var liveCheckers = liveness.Checkers

var forgetFn = func(gone []*openSearchStruct.GlobalSearchOpenSearchResp) {
	helpers.GoSafeNamed("search.forget-stale-entries", func() {
		ctx := context.Background()
		for _, h := range gone {
			t, id := hitKey(h)
			if index, ok := hitIndex[t]; ok {
				if err := opensearchModels.ForgetEntry(ctx, index, id); err != nil {
					helpers.MessageLogs.ErrorLog.Printf("search: could not forget the stale %s entry %s: %v", t, id, err)
				}
			}
		}
	})
}

// keepLiveHits returns the hits whose content still exists and is not
// deleted, and has the indices forget the ones whose content is gone.
func keepLiveHits(ctx context.Context, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	if len(hits) == 0 {
		return hits
	}
	states := liveness.States(ctx, liveCheckers, hits, hitKey)
	kept, gone := liveness.Partition(hits, hitKey, states)
	if len(gone) > 0 {
		helpers.LogInfoWithContext(ctx, "search: dropped %d results whose content no longer exists", len(gone))
		forgetFn(gone)
	}
	return kept
}

// page cuts hits to a page and checks it. HasMore is decided on what the
// index returned, before anything is dropped, so losing a stale hit never
// makes a page claim to be the last.
func page(ctx context.Context, hits []*openSearchStruct.GlobalSearchOpenSearchResp, size int) GlobalSearchPagination {
	var p GlobalSearchPagination
	p.HasMore = len(hits) > size
	if p.HasMore {
		hits = hits[:size]
	}
	p.Page = keepLiveHits(ctx, hits)
	return p
}

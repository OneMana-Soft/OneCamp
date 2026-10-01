package ai

// Search results whose content no longer exists.
//
// The demo showed why: asked what the team discussed, the assistant cited the
// handbook four times, and three of the four links opened nothing. Every
// vector search result is checked where its content lives before anyone sees
// it (domain/Liveness, which global search uses too). Content that is gone is
// dropped and its embedding deleted, so the next search does not meet it;
// soft-deleted content is dropped but its entry kept; a check that cannot run
// keeps the results.

import (
	"context"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/helpers"
)

// liveCheckers is the set of checks KeepLive runs. A seam for tests.
var liveCheckers = liveness.Checkers

// forgetStaleFn deletes the index entries of content that no longer exists.
// A seam for tests.
var forgetStaleFn = func(stale []SimilarResult) {
	helpers.GoSafeNamed("ai.forget-stale-embeddings", func() {
		ctx := context.Background()
		for _, r := range stale {
			if err := DeleteEmbedding(ctx, r.ContentType, r.ContentUUID); err != nil {
				helpers.MessageLogs.ErrorLog.Printf("AI: could not delete the stale embedding %s:%s: %v", r.ContentType, r.ContentUUID, err)
			}
		}
	})
}

func similarKey(r SimilarResult) (string, string) { return r.ContentType, r.ContentUUID }

// KeepLive returns the results whose content still exists and is not deleted,
// and has the index forget the ones whose content is gone.
func KeepLive(ctx context.Context, results []SimilarResult) []SimilarResult {
	if len(results) == 0 {
		return results
	}
	states := liveness.States(ctx, liveCheckers, results, similarKey)
	kept, stale := liveness.Partition(results, similarKey, states)
	if len(stale) > 0 {
		helpers.LogInfoWithContext(ctx, "AI: dropped %d search results whose content no longer exists", len(stale))
		forgetStaleFn(stale)
	}
	return kept
}

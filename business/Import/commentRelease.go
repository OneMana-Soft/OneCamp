package business

import (
	"context"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
)

// The comments the search repair held back (commentSearchRepair.go), let in
// once their import's re-check has settled: every review re-checked, or kept
// as it is by an admin (recheck.go). The repair ran to the end once and never
// comes back to them, so each settled import's comments go through its page
// again (repairTaskCommentPage), which now finds the import safe. A comment
// the re-check hid stays hidden, one deleted stays deleted, and the entries a
// person's edit already rewrote are left as they are.

// The imports and records the release reads and writes. Seams for tests.
var (
	unreleasedImports = reviewModel.UnreleasedImports
	importCommentIDs  = importModels.CommentIDs
	markReleased      = reviewModel.MarkReleased
)

// releaseInBackground lets in, after the request ends, the held-back comments
// of the imports an admin's keep, re-check or undo has just settled, rather
// than at the next start's pass (StartRechecks).
func releaseInBackground(ctx context.Context) {
	helpers.GoSafeNamed("import.release", func() {
		if err := releaseSettled(context.WithoutCancel(ctx)); err != nil {
			helpers.LogErrorWithContext(ctx, "Letting settled imports' held-back comments into search: %v", err)
		}
	})
}

// releaseSettled lets into search the held-back comments of each import
// that has settled since its comments were last let in, and records it.
func releaseSettled(ctx context.Context) error {
	pending, err := unreleasedImports(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	fixed, err := fixMoments()
	if err != nil {
		return err
	}
	settled, err := settledImports(ctx)
	if err != nil {
		return err
	}
	ctx = recheckContext(ctx)
	for _, id := range pending {
		var rep TaskCommentSearchRepair
		for after := uuid.Nil; ; {
			ids, err := importCommentIDs(ctx, id, after, taskCommentSearchRepairPage)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				break
			}
			page := make([]string, len(ids))
			for i, c := range ids {
				page[i] = c.String()
			}
			if err := repairTaskCommentPage(ctx, &rep, page, fixed, settled); err != nil {
				return err
			}
			after = ids[len(ids)-1]
		}
		if err := markReleased(ctx, id); err != nil {
			return err
		}
		helpers.LogInfoWithContext(ctx, "Import %s settled its re-check: %s", id, rep)
	}
	return nil
}

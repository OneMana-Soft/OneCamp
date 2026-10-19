package business

// Comment counts on a member's rows (tables A8): each row of a page says how
// many comments its open threads hold, read in one query for the page
// (models/postgres/Thread OpenCommentCounts), so the grid marks the rows
// people are talking about. Only the member's bundle and pages carry them
// (controllers/DataTable): the API, AI tools and guests read rows without, and
// a guest can't open a row's threads.

import (
	"context"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	threadModels "github.com/akashc777/OneCamp/models/postgres/Thread"
)

// rowCommentCounts is a seam for tests: where the counts are read.
var rowCommentCounts = func(ctx context.Context, ids []string) (map[string]int, error) {
	return threadModels.OpenCommentCounts(ctx, tableAccess.KindTableRow, ids)
}

// WithCommentCounts sets each row's Comments. Counts that can't be read leave
// the rows without one, so the grid shows no mark: the rows themselves are
// what the reader asked for, and they are never held back for a count.
func WithCommentCounts(ctx context.Context, rows []*model.Row) {
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Id.String())
	}
	counts, err := rowCommentCounts(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable WithCommentCounts: the rows go out without their comment counts: %+v", err)
		return
	}
	for _, r := range rows {
		r.Comments = counts[r.Id.String()]
	}
}

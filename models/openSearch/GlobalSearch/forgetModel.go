package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ForgetEntry removes one entry from a search index. For content its database
// says no longer exists (see domain/Liveness), never for content that is only
// soft-deleted, whose entry must survive a restore from trash.
func ForgetEntry(ctx context.Context, index, id string) error {
	return opensearchInit.DeleteDocument(ctx, index, id)
}

// deleteByQuery is search's delete-by-query, a seam for tests.
var deleteByQuery = func(ctx context.Context, req opensearchapi.DocumentDeleteByQueryReq) (*opensearchapi.DocumentDeleteByQueryResp, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("search is not connected")
	}
	return opensearchInit.OpenSearchClient.Document.DeleteByQuery(ctx, req)
}

// refreshPresent makes every write so far to those of some indices that exist
// visible to the next search, a seam for tests.
var refreshPresent = opensearchInit.RefreshPresentIndices

// ForgetWhere removes, for good, every entry of indices one of whose fields
// holds one of values ("_id" for the entries' own ids), in one delete, and
// answers how many. It is ForgetEntry for content gone for good that has many
// entries, or entries a caller can't name one by one: a board purged past the
// trash takes its own entry (_id), its comments' (comment_board_id) and its AI
// text's (board_uuid). Values are ids, so a field of one index never names
// another index's entry by chance.
//
// Like updateByQuerySettled, it waits for the delete, and runs again while
// entries it matched were being written by something else at the same moment,
// which conflicts "proceed" skips; and each run refreshes its indices first.
// A delete-by-query sees only what the last refresh showed, and an index
// nothing searched for 30 s goes search-idle and skips its scheduled
// refreshes: a purged board kept its comment's entry, findable after the
// purge (b204). An index that doesn't exist (the AI index where AI is off) is
// skipped, not an error.
func ForgetWhere(ctx context.Context, indices, fields, values []string) (deleted int, err error) {
	if len(values) == 0 || len(indices) == 0 || len(fields) == 0 {
		return 0, nil
	}
	// Each field as itself, and as its keyword sub-field: a field an index
	// mapped by itself as text never matches a whole id (exactClauses).
	var clauses []string
	for _, f := range fields {
		clauses = append(clauses, exactClauses(f, values)...)
	}
	body := `{ "query": { "bool": { "should": [` + strings.Join(clauses, ",") + `], "minimum_should_match": 1 } } }`
	yes := true
	for attempt := 1; ; attempt++ {
		if err := refreshPresent(ctx, indices...); err != nil {
			return deleted, err
		}
		resp, err := deleteByQuery(ctx, opensearchapi.DocumentDeleteByQueryReq{
			Indices: indices,
			Body:    strings.NewReader(body),
			Params: opensearchapi.DocumentDeleteByQueryParams{
				Refresh:           &yes,
				WaitForCompletion: &yes,
				Conflicts:         "proceed",
				IgnoreUnavailable: &yes,
				AllowNoIndices:    &yes,
			},
		})
		if err != nil {
			return deleted, err
		}
		deleted += resp.Deleted
		if len(resp.Failures) > 0 {
			return deleted, fmt.Errorf("%d entries not removed: %s", len(resp.Failures), resp.Failures[0])
		}
		if resp.VersionConflicts == 0 {
			return deleted, nil
		}
		if attempt == conflictAttempts {
			return deleted, fmt.Errorf("%d entries were being written by something else and were still not removed after %d runs", resp.VersionConflicts, attempt)
		}
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(conflictBackoff(attempt)):
		}
	}
}

package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// conflictAttempts is how many times an update-by-query runs while entries it
// matched were being written by something else at the same moment.
const conflictAttempts = 5

// conflictBackoff is how long a run waits before the next one, a seam for
// tests.
var conflictBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * 200 * time.Millisecond }

// updateByQuery is search's update-by-query, a seam for tests.
var updateByQuery = func(ctx context.Context, req opensearchapi.UpdateByQueryReq) (*opensearchapi.UpdateByQueryResp, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("search is not connected")
	}
	return opensearchInit.OpenSearchClient.UpdateByQuery(ctx, req)
}

// updateByQuerySettled runs an update-by-query (refreshed, waited for) whose
// script leaves an entry already right as it is, and runs it again while some
// of the entries it matched were being written by something else at the same
// moment. With conflicts "proceed" those entries are skipped: not written and
// not counted. So the startup repair recorded itself done with a comment's
// sharing still unwritten (its doc's delete cascade was writing the comment
// as the repair ran), and nothing wrote it again; a cascade skipped the same
// way would have left an entry of a deleted doc live. It answers how many
// entries it changed over every run, and an error when any failed, or when
// some were still in conflict after the last run.
func updateByQuerySettled(ctx context.Context, indices []string, body string) (updated int, err error) {
	yes := true
	for attempt := 1; ; attempt++ {
		resp, err := updateByQuery(ctx, opensearchapi.UpdateByQueryReq{
			Indices: indices,
			Body:    strings.NewReader(body),
			Params: opensearchapi.UpdateByQueryParams{
				Refresh:           &yes,
				WaitForCompletion: &yes,
				Conflicts:         "proceed",
			},
		})
		if err != nil {
			return updated, err
		}
		updated += resp.Updated
		if len(resp.Failures) > 0 {
			return updated, fmt.Errorf("%d entries not written: %s", len(resp.Failures), resp.Failures[0])
		}
		if resp.VersionConflicts == 0 {
			return updated, nil
		}
		if attempt == conflictAttempts {
			return updated, fmt.Errorf("%d entries were being written by something else and were still not rewritten after %d runs", resp.VersionConflicts, attempt)
		}
		select {
		case <-ctx.Done():
			return updated, ctx.Err()
		case <-time.After(conflictBackoff(attempt)):
		}
	}
}

package business

// The one-time pass that gives entries written before the searchable copies
// existed their copies (initializers/opensearchInit/queryFields.go): each
// entry missing a copy is rewritten as it is, which the index fills the copy
// in for. It's resumable: an entry rewritten already has its copy and isn't
// matched again, so a pass that stops part way picks up where it was. Done
// once per install, and recorded.

import (
	"context"
	"fmt"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
)

// queryCopiesKey records the pass done on this install.
const queryCopiesKey = "search.query_copies_backfilled"

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "search.query_copies", Every: 6 * time.Hour, RunAtStart: true, Run: BackfillQueryCopies})
}

// The pass's reads and writes, seams for tests.
var (
	copiesDone     = configModel.IsDone
	markCopiesDone = func() error { return configModel.UpsertConfig(queryCopiesKey, "done") }
	rewriteMissing = opensearchModels.UpdateByQuerySettled
)

// BackfillQueryCopies is the pass: every entry missing a searchable copy is
// rewritten, index by index, and the pass is recorded once every index took
// its copies. Run at start and every six hours until it's recorded.
func BackfillQueryCopies(ctx context.Context) error {
	if done, err := copiesDone(queryCopiesKey); err != nil || done {
		return err
	}
	complete := true
	for index, fields := range opensearchInit.QueryCopies {
		for field := range fields {
			if !opensearchInit.QueryFieldReady(index, field+opensearchInit.QueryCopy) {
				complete = false // the index didn't take the copy; the next pass tries again
				continue
			}
			body := fmt.Sprintf(`{ "query": { "bool": { "filter": [ { "exists": { "field": %q } } ], "must_not": [ { "exists": { "field": %q } } ] } } }`,
				field, field+opensearchInit.QueryCopy)
			n, err := rewriteMissing(ctx, []string{index}, body)
			if err != nil {
				return fmt.Errorf("search: giving %s's %s its searchable copy: %w", index, field, err)
			}
			if n > 0 {
				helpers.LogInfoWithContext(ctx, "search: gave %d %s entries a searchable copy of %s", n, index, field)
			}
		}
	}
	if !complete {
		return nil
	}
	return markCopiesDone()
}

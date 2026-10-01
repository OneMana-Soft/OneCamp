package opensearchInit

// Will search find anything?
//
// HOW THIS FAILS QUIETLY. A missing index is not an error to a search: it is
// zero results. Someone types a word they know is in a message, gets nothing
// back, and concludes the product's search is bad rather than absent. Boot only
// proved the cluster answered; index creation happens right after, and an
// OpenSearch that is up but out of disk, or read-only because a watermark
// tripped, accepts the connection and refuses the create.
//
// The index list is the one this package actually creates, not a copy: a copy
// would keep passing after a new index was added here, which is precisely the
// drift worth catching.
//
// Reachability is asked first and separately, because otherwise a cluster that
// is merely down reports as every index being absent, and the operator goes off
// to recreate indices that were never the problem.
//
// WHAT IT DOES NOT PROVE. That documents are being INDEXED. An index can exist
// and be empty because the writer is failing, and only a workspace with known
// content could tell the difference.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "search",
		Kind: helpers.CheckKindDependency,
		Describe: "OpenSearch is reachable and every index this build needs exists. It does not prove " +
			"documents are reaching those indices, only that a search has somewhere to look.",
		Probe: func(ctx context.Context) error {
			if OpenSearchClient == nil {
				return fmt.Errorf("no OpenSearch client: search was never initialised, so every search returns nothing")
			}
			if len(indicesToCreate) == 0 {
				return fmt.Errorf("search never finished starting up: the indices were never declared, " +
					"so the connection was made and the setup after it did not run")
			}

			// Reachability FIRST, and separately. Without this, a cluster that is
			// simply down makes every Indices.Exists call fail and the check
			// reports "all indices missing" -- sending an operator to recreate
			// indices that are fine. A diagnostic that misnames the fault is
			// worse than one that stays quiet.
			if _, err := OpenSearchClient.Ping(ctx, nil); err != nil {
				return fmt.Errorf("OpenSearch is not answering, so every search returns nothing: %w", err)
			}

			var missing []string
			for index := range indicesToCreate {
				resp, err := OpenSearchClient.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
					Indices: []string{index},
				})
				if err != nil {
					// The cluster answered a ping a moment ago, so a failure
					// here is about this index rather than the connection.
					// Exists reports a missing index as an error on some client
					// versions, which is why this is not treated as fatal.
					missing = append(missing, index)
					continue
				}
				if resp.StatusCode != http.StatusOK {
					missing = append(missing, index)
				}
			}

			if len(missing) > 0 {
				sort.Strings(missing)
				return fmt.Errorf("%d of %d search indices are missing (%s); searches against them return "+
					"no results rather than an error, which reads to a user as nothing being found",
					len(missing), len(indicesToCreate), strings.Join(missing, ", "))
			}
			return nil
		},
	})
}

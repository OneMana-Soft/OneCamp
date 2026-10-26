package opensearchInit

import (
	"context"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// RefreshIndices makes every write to indices so far visible to the next
// search. For a change that must see what was just written
// (models/openSearch/GlobalSearch updateByQuerySettled, which refreshes before
// it matches). Never per write: a refresh is the whole index's.
func RefreshIndices(ctx context.Context, indices ...string) error {
	if OpenSearchClient == nil {
		return fmt.Errorf("search is not connected")
	}
	_, err := OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: indices})
	return err
}

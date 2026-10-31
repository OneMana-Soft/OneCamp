package opensearchInit

import (
	"context"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// RefreshIndices makes every write to indices so far visible to the next
// search. For a change that takes access away (business/Doc
// SettleAccessIndexes, business/Board SettleBoardSearch): it returns only once
// search no longer grants what was taken. Never per write: a refresh is the
// whole index's.
func RefreshIndices(ctx context.Context, indices ...string) error {
	if OpenSearchClient == nil {
		return fmt.Errorf("search is not connected")
	}
	_, err := OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: indices})
	return err
}

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
	return refreshIndices(ctx, indices, opensearchapi.IndicesRefreshParams{})
}

// RefreshPresentIndices is RefreshIndices for indices some installs don't
// have (the AI index where AI is off): one that doesn't exist is skipped, not
// an error. For a purge's delete (models/openSearch/GlobalSearch ForgetWhere),
// which skips them too.
func RefreshPresentIndices(ctx context.Context, indices ...string) error {
	yes := true
	return refreshIndices(ctx, indices, opensearchapi.IndicesRefreshParams{IgnoreUnavailable: &yes, AllowNoIndices: &yes})
}

func refreshIndices(ctx context.Context, indices []string, params opensearchapi.IndicesRefreshParams) error {
	if OpenSearchClient == nil {
		return fmt.Errorf("search is not connected")
	}
	_, err := OpenSearchClient.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: indices, Params: params})
	return err
}

package models

import (
	"context"
	"errors"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// ForgetEntry removes one entry from a search index. For content its database
// says no longer exists (see domain/Liveness), never for content that is only
// soft-deleted, whose entry must survive a restore from trash.
func ForgetEntry(ctx context.Context, index, id string) error {
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}
	_, err := opensearchInit.OpenSearchClient.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{Index: index, DocumentID: id})
	return err
}

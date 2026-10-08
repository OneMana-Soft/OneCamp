package opensearchInit

import (
	"context"
	"errors"
	"net/http"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DeleteDocument removes one document from an index. A document that isn't
// there (nor its index) is already gone, so that's no error: deleting a post
// that was never embedded, or forgetting an entry twice, succeeds quietly.
func DeleteDocument(ctx context.Context, index, id string) error {
	return deleteDocument(ctx, OpenSearchClient, index, id)
}

func deleteDocument(ctx context.Context, c *opensearchapi.Client, index, id string) error {
	if c == nil {
		return errors.New("search is not connected")
	}
	resp, err := c.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{Index: index, DocumentID: id})
	if err != nil && resp != nil && resp.Inspect().Response != nil && resp.Inspect().Response.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

package opensearchInit

import (
	"context"
	"errors"
	"io"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// updateRetries is how many times OpenSearch tries a partial update again
// when another write to the same document lands in between.
var updateRetries = 5

// UpdateDocument applies a partial update (a body of {"doc": …}) to one
// document. OpenSearch reads the document, merges, and writes it back; when
// another write lands in between, the update fails with a version conflict
// and its change would be lost (a task renamed and moved at once, a seed
// writing a doc twice). It's tried again instead, from the document as it
// then is, so both changes land.
func UpdateDocument(ctx context.Context, index, id string, body io.Reader) error {
	return updateDocument(ctx, OpenSearchClient, index, id, body)
}

func updateDocument(ctx context.Context, c *opensearchapi.Client, index, id string, body io.Reader) error {
	if c == nil {
		return errors.New("search is not connected")
	}
	_, err := c.Update(ctx, opensearchapi.UpdateReq{
		Index:      index,
		DocumentID: id,
		Body:       body,
		Params:     opensearchapi.UpdateParams{RetryOnConflict: &updateRetries},
	})
	return err
}

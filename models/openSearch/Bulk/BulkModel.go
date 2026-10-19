package models

import (
	"context"
	"errors"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// BulkCreateInOpenSearch performs a bulk (NDJSON) write.
//
// The body goes through openSearchStruct.BulkReader, which is the same guarantee
// IndexReader gives single-document writes: no embedded media and no unbounded
// field reaches the cluster. This matters more here than anywhere else, because the
// bulk path carries most of the write volume — every post and comment created with
// attachments, and every cascading rename or delete — and it is assembled by hand
// as NDJSON in the domain layer, so it would otherwise be the one way around the
// guard.
func BulkCreateInOpenSearch(bulkCreateString string) (err error) {

	_, err = opensearchInit.OpenSearchClient.Bulk(
		context.Background(),
		opensearchapi.BulkReq{
			Body: openSearchStruct.BulkReader(bulkCreateString),
		},
	)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"models/BulkCreateInOpenSearch Error performing bulk operation err: %+v",
			err)
		return
	}

	return
}

// BulkIndexChecked performs a bulk (NDJSON) write, through the same BulkReader
// guard, and answers an error when the cluster refused any item, naming the
// first: the outbox's sinks try again on it, where BulkCreateInOpenSearch's
// callers only ever saw a request that failed whole.
func BulkIndexChecked(ctx context.Context, body string) error {
	resp, err := opensearchInit.OpenSearchClient.Bulk(ctx, opensearchapi.BulkReq{Body: openSearchStruct.BulkReader(body)})
	if err != nil {
		return err
	}
	if resp == nil || !resp.Errors {
		return nil
	}
	for _, item := range resp.Items {
		for op, r := range item {
			if r.Error != nil {
				return fmt.Errorf("%s of %s in %s refused (%d): %s: %s", op, r.ID, r.Index, r.Status, r.Error.Type, r.Error.Reason)
			}
		}
	}
	return errors.New("the bulk write said some items failed, and named none")
}

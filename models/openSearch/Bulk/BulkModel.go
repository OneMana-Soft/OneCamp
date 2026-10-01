package models

import (
	"context"

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

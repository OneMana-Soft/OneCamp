package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	memDgraph "github.com/akashc777/OneCamp/models/dgraph/WorkspaceMemory"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DeleteEmbeddings is DeleteEmbedding for several items of one kind in one
// bulk request (backend audit U2b: the memory cascades deleted each alone).
// Those the reindex buffer takes are its to replay; a document already gone
// is no error.
func DeleteEmbeddings(ctx context.Context, contentType string, contentUUIDs []string) error {
	var body strings.Builder
	n := 0
	for _, id := range contentUUIDs {
		if bufferReindexWrite(EmbeddingDoc{ContentType: contentType, ContentUUID: id}, true) {
			continue
		}
		action, _ := json.Marshal(map[string]any{"delete": map[string]string{"_index": AI_EMBEDDINGS_INDEX, "_id": contentType + ":" + id}})
		body.Write(action)
		body.WriteByte('\n')
		n++
	}
	if n == 0 {
		return nil
	}
	if opensearchInit.OpenSearchClient == nil {
		return fmt.Errorf("search is not connected")
	}
	resp, err := opensearchInit.OpenSearchClient.Bulk(ctx, opensearchapi.BulkReq{Body: openSearchStruct.BulkReader(body.String())})
	if err != nil {
		return fmt.Errorf("failed to delete %d embeddings: %w", n, err)
	}
	if resp != nil && resp.Errors {
		for _, item := range resp.Items {
			for _, r := range item {
				if r.Error != nil {
					return fmt.Errorf("failed to delete embedding %s: %s", r.ID, r.Error.Reason)
				}
			}
		}
	}
	return nil
}

// dropMemoryProjectionsMany removes several memory items' search and graph
// projections, in one request to each.
func dropMemoryProjectionsMany(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	errSearch := DeleteEmbeddings(ctx, "memory", ids)
	errGraph := memDgraph.DeleteMemoryItems(ctx, ids)
	if errSearch != nil {
		return errSearch
	}
	return errGraph
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	memDgraph "github.com/akashc777/OneCamp/models/dgraph/WorkspaceMemory"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	bulkModel "github.com/akashc777/OneCamp/models/openSearch/Bulk"
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

// storeEmbeddingsChunk is how many texts StoreEmbeddings asks the model for in
// one call: a local model embeds them one after another, within its call's
// time limit.
const storeEmbeddingsChunk = 16

// upsertEntries writes entries onto the index in one bulk request. A seam for
// tests.
var upsertEntries = bulkModel.BulkUpsert

// StoreEmbeddings is StoreEmbedding for several entries: their vectors asked
// for storeEmbeddingsChunk texts at a time, and the entries written in one
// bulk request (made, or written over, as StoreEmbedding does). Those the
// reindex buffer takes are its to replay; an entry with no text is skipped.
func StoreEmbeddings(ctx context.Context, docs []EmbeddingDoc) error {
	var todo []EmbeddingDoc
	var texts []string
	for _, d := range docs {
		if bufferReindexWrite(d, false) || strings.TrimSpace(d.ContentText) == "" {
			continue
		}
		d.ContentText = entryText(d.ContentText)
		todo, texts = append(todo, d), append(texts, d.ContentText)
	}
	if len(todo) == 0 {
		return nil
	}
	svc := GetService()
	if svc == nil || svc.Embedder == nil {
		return fmt.Errorf("AI embedding service not initialized")
	}
	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += storeEmbeddingsChunk {
		got, err := svc.GenerateEmbeddingsForIndexing(ctx, texts[start:min(start+storeEmbeddingsChunk, len(texts))])
		if err != nil {
			return fmt.Errorf("failed to generate embeddings: %w", err)
		}
		vectors = append(vectors, got...)
	}
	if len(vectors) != len(todo) {
		return fmt.Errorf("embedding returned %d vectors for %d texts", len(vectors), len(todo))
	}
	entries := make([]bulkModel.Upsert, len(todo))
	for i, d := range todo {
		if len(vectors[i]) == 0 {
			return fmt.Errorf("embedding returned an empty vector for %s:%s", d.ContentType, d.ContentUUID)
		}
		raw, err := entryJSON(embeddingDocWithVector{EmbeddingDoc: d, Embedding: vectors[i]})
		if err != nil {
			return fmt.Errorf("failed to marshal embedding doc: %w", err)
		}
		entries[i] = bulkModel.Upsert{ID: d.ContentType + ":" + d.ContentUUID, Doc: json.RawMessage(raw)}
	}
	if err := upsertEntries(ctx, AI_EMBEDDINGS_INDEX, entries); err != nil {
		return fmt.Errorf("failed to write %d embeddings: %w", len(entries), err)
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

package opensearchInit

// Helpers for managing the ai_embeddings k-NN index at a runtime-chosen
// vector dimension. The dimension is pinned at index-create time and
// CANNOT be altered in place — changing the embedding model to one with a
// different dimension requires dropping and recreating this index, then
// re-embedding all content. These helpers back the admin "change
// embedding model" → reindex flow (business/AI + services/AI reindex).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// AIEmbeddingsIndexName is the canonical name (kept in sync with
// services/AI.AI_EMBEDDINGS_INDEX).
const AIEmbeddingsIndexName = "ai_embeddings"

// aiEmbeddingsIndexBody renders the ai_embeddings mapping for a given
// vector dimension. Everything except the knn_vector dimension is fixed.
func aiEmbeddingsIndexBody(dimension int) string {
	return fmt.Sprintf(`{
		"settings": {
			"index": {
				"knn": true,
				"number_of_shards": 1,
				"number_of_replicas": 0
			}
		},
		"mappings": {
			"properties": {
				"embedding": { "type": "knn_vector", "dimension": %d },
				"content_text": { "type": "text", "index": true },
				"content_type": { "type": "keyword", "index": true },
				"content_uuid": { "type": "keyword", "index": true },
				"created_date": { "type": "date", "format": "epoch_second", "index": true },
				"deleted_date": { "type": "date", "format": "epoch_second", "index": true },

				"channel_uuid": { "type": "keyword", "index": true },
				"channel_name": { "type": "text", "index": true },

				"chat_by_user_id": { "type": "keyword", "index": true },
				"chat_to_user_id": { "type": "keyword", "index": true },
				"chat_participant_uuids": { "type": "keyword", "index": true },
				"chat_grp_id": { "type": "keyword", "index": true },

				"doc_private": { "type": "boolean", "index": true },
				"doc_created_by_user_id": { "type": "keyword", "index": true },
				"doc_reading_users": { "type": "keyword", "index": true },
				"doc_editing_users": { "type": "keyword", "index": true },
				"doc_commenting_users": { "type": "keyword", "index": true },

				"project_uuid": { "type": "keyword", "index": true },
				"task_assignee_user_id": { "type": "keyword", "index": true },
				"task_created_by_user_id": { "type": "keyword", "index": true },

				"author_uuid": { "type": "keyword", "index": true },
				"author_name": { "type": "text", "index": true }
			}
		}
	}`, dimension)
}

// RecreateAIEmbeddingsIndex deletes the existing ai_embeddings index (if
// present) and creates a fresh one at the given dimension. This DESTROYS
// all stored vectors — callers must re-embed content afterwards.
func RecreateAIEmbeddingsIndex(ctx context.Context, dimension int) error {
	if dimension <= 0 {
		return fmt.Errorf("invalid embedding dimension %d", dimension)
	}

	// Delete if it exists (ignore 404).
	existsResp, err := OpenSearchClient.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{AIEmbeddingsIndexName},
	})
	if err == nil && existsResp.StatusCode == http.StatusOK {
		if _, derr := OpenSearchClient.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{
			Indices: []string{AIEmbeddingsIndexName},
		}); derr != nil {
			return fmt.Errorf("delete ai_embeddings index: %w", derr)
		}
		helpers.LogInfoWithContext(ctx, "Dropped ai_embeddings index for reindex at dim=%d", dimension)
	}

	if _, err := OpenSearchClient.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: AIEmbeddingsIndexName,
		Body:  strings.NewReader(aiEmbeddingsIndexBody(dimension)),
	}); err != nil {
		return fmt.Errorf("create ai_embeddings index at dim=%d: %w", dimension, err)
	}

	helpers.LogInfoWithContext(ctx, "Created ai_embeddings index at dim=%d", dimension)
	return nil
}

// EnsureAIEmbeddingsIndexDimension reconciles the live ai_embeddings index
// with the configured embedding dimension at startup, NON-DESTRUCTIVELY:
//
//   - index missing                 → create at wantDim
//   - index present, dim matches    → no-op
//   - index present, empty, wrong   → recreate at wantDim (safe: no data)
//   - index present, has data, wrong→ leave intact, log a loud warning so
//     the operator triggers a reindex from the admin panel (we never
//     auto-destroy embedded content on boot)
//
// This closes the gap where an operator boots with an env-configured
// embedding model whose dimension differs from the default 768 baked into
// the initial index creation.
func EnsureAIEmbeddingsIndexDimension(ctx context.Context, wantDim int) error {
	if wantDim <= 0 {
		return nil
	}

	existsResp, err := OpenSearchClient.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{
		Indices: []string{AIEmbeddingsIndexName},
	})
	if err != nil || existsResp.StatusCode != http.StatusOK {
		// Missing → create at the wanted dimension.
		return RecreateAIEmbeddingsIndex(ctx, wantDim)
	}

	curDim, derr := aiEmbeddingsIndexDimension(ctx)
	if derr != nil {
		// Can't read mapping — leave the index alone rather than risk it.
		helpers.LogErrorWithContext(ctx, "EnsureAIEmbeddingsIndexDimension: read mapping failed: %v", derr)
		return nil
	}
	if curDim == wantDim {
		return nil // already correct
	}

	count, cerr := aiEmbeddingsDocCount(ctx)
	if cerr == nil && count == 0 {
		helpers.LogInfoWithContext(ctx, "ai_embeddings empty at dim=%d; recreating at configured dim=%d", curDim, wantDim)
		return RecreateAIEmbeddingsIndex(ctx, wantDim)
	}

	helpers.MessageLogs.ErrorLog.Printf(
		"AI embeddings index dimension mismatch: index=%d configured=%d with %d documents. "+
			"Semantic search will fail until you change the embedding model in the admin panel "+
			"(which rebuilds the index) or align the embedding model with the index dimension.",
		curDim, wantDim, count)
	return nil
}

// aiEmbeddingsIndexDimension reads the knn_vector dimension from the live
// index mapping. Returns 0 if it can't be determined.
func aiEmbeddingsIndexDimension(ctx context.Context) (int, error) {
	resp, err := OpenSearchClient.Indices.Mapping.Get(ctx, &opensearchapi.MappingGetReq{
		Indices: []string{AIEmbeddingsIndexName},
	})
	if err != nil {
		return 0, err
	}
	// resp.Indices is map[indexName] -> { Mappings: <raw> }.
	var mapping struct {
		Properties struct {
			Embedding struct {
				Dimension int `json:"dimension"`
			} `json:"embedding"`
		} `json:"properties"`
	}
	for _, idx := range resp.Indices {
		if err := json.Unmarshal(idx.Mappings, &mapping); err != nil {
			continue
		}
		if mapping.Properties.Embedding.Dimension > 0 {
			return mapping.Properties.Embedding.Dimension, nil
		}
	}
	return 0, fmt.Errorf("dimension not found in mapping")
}

// aiEmbeddingsDocCount returns the number of documents in the index.
func aiEmbeddingsDocCount(ctx context.Context) (int, error) {
	resp, err := OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{
		Indices: []string{AIEmbeddingsIndexName},
	})
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

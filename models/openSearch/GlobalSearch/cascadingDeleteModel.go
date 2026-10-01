package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// SyncCascadingDeletionInOpenSearch updates the deleted_date for all documents in specified indices
// that match any of the provided parent fields with the given parentID.
// deletedBy tags the deletion source: "cascade" for parent archive, "user" for individual deletion.
func SyncCascadingDeletionInOpenSearch(ctx context.Context, parentFields []string, parentID string, deletedAt int64, indices []string, deletedBy string) error {
	if len(parentFields) == 0 || parentID == "" || len(indices) == 0 {
		return nil
	}

	var shouldClauses []string
	for _, field := range parentFields {
		shouldClauses = append(shouldClauses, fmt.Sprintf(`{ "term": { "%s": "%s" } }`, field, parentID))
	}

	body := fmt.Sprintf(`{
		"script": {
			"source": "ctx._source.deleted_date = params.deleted_at; ctx._source.deleted_by = params.deleted_by",
			"lang": "painless",
			"params": {
				"deleted_at": %d,
				"deleted_by": "%s"
			}
		},
		"query": {
			"bool": {
				"should": [
					%s
				],
				"minimum_should_match": 1
			}
		}
	}`, deletedAt, deletedBy, strings.Join(shouldClauses, ","))

	boolTrue := true
	resp, err := opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingDeletionInOpenSearch failed: %v", err)
		return err
	}

	if len(resp.Failures) > 0 {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingDeletionInOpenSearch had %d failures: %+v", len(resp.Failures), resp.Failures)
	}

	return nil
}

// SyncCascadingUnarchiveInOpenSearch removes the deleted_date for all documents in specified indices
// that match any of the provided parent fields with the given parentID.
// Only removes if deleted_by matches (protects individually-deleted items from being resurrected).
func SyncCascadingUnarchiveInOpenSearch(ctx context.Context, parentFields []string, parentID string, indices []string, deletedBy string) error {
	if len(parentFields) == 0 || parentID == "" || len(indices) == 0 {
		return nil
	}

	var shouldClauses []string
	for _, field := range parentFields {
		shouldClauses = append(shouldClauses, fmt.Sprintf(`{ "term": { "%s": "%s" } }`, field, parentID))
	}

	body := fmt.Sprintf(`{
		"script": {
			"source": "if (ctx._source.deleted_by == params.deleted_by) { ctx._source.remove('deleted_date'); ctx._source.remove('deleted_by'); }",
			"lang": "painless",
			"params": {
				"deleted_by": "%s"
			}
		},
		"query": {
			"bool": {
				"should": [
					%s
				],
				"minimum_should_match": 1
			}
		}
	}`, deletedBy, strings.Join(shouldClauses, ","))

	boolTrue := true
	resp, err := opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingUnarchiveInOpenSearch failed: %v", err)
		return err
	}

	if len(resp.Failures) > 0 {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingUnarchiveInOpenSearch had %d failures: %+v", len(resp.Failures), resp.Failures)
	}

	return nil
}

// SyncDocMetadataInOpenSearch updates the document metadata (title, permissions) for all comments and attachments associated with a document.
func SyncDocMetadataInOpenSearch(ctx context.Context, docUUID string, title *string, isPrivate *bool, readingUsers, editingUsers, commentingUsers []string, createdBy string) error {
	if docUUID == "" {
		return nil
	}

	params := map[string]interface{}{
		"readingUsers":    readingUsers,
		"editingUsers":    editingUsers,
		"commentingUsers": commentingUsers,
		"createdBy":       createdBy,
	}

	source := ""
	if isPrivate != nil {
		params["isPrivate"] = *isPrivate
		source += "if (ctx._index == 'comments') { ctx._source.comment_doc_private = params.isPrivate; } else if (ctx._index == 'attachments') { ctx._source.attachment_doc_private = params.isPrivate; } "
	}
	if title != nil {
		params["title"] = *title
		source += "if (ctx._index == 'comments') { ctx._source.comment_doc_title = params.title; } else if (ctx._index == 'attachments') { ctx._source.attachment_doc_title = params.title; } "
	}

	source += "if (ctx._index == 'comments') { ctx._source.comment_doc_reading_users = params.readingUsers; ctx._source.comment_doc_editing_users = params.editingUsers; ctx._source.comment_doc_commenting_users = params.commentingUsers; ctx._source.comment_doc_created_by_user_id = params.createdBy; } else if (ctx._index == 'attachments') { ctx._source.attachment_doc_reading_users = params.readingUsers; ctx._source.attachment_doc_editing_users = params.editingUsers; ctx._source.attachment_doc_commenting_users = params.commentingUsers; ctx._source.attachment_doc_created_by_user_id = params.createdBy; }"

	paramsJSON, _ := json.Marshal(params)
	body := fmt.Sprintf(`{
		"script": {
			"source": "%s",
			"lang": "painless",
			"params": %s
		},
		"query": {
			"bool": {
				"should": [
					{ "term": { "comment_doc_id": "%s" } },
					{ "term": { "attachment_doc_id": "%s" } }
				],
				"minimum_should_match": 1
			}
		}
	}`, source, string(paramsJSON), docUUID, docUUID)

	boolTrue := true
	_, err := opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: []string{"comments", "attachments"},
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncDocMetadataInOpenSearch failed: %v", err)
		return err
	}

	return nil
}

// SyncCascadingDeletionInOpenSearchMulti is like the single-ID version but uses a terms query
// for multiple IDs — one OpenSearch UpdateByQuery call instead of N.
func SyncCascadingDeletionInOpenSearchMulti(ctx context.Context, parentField string, parentIDs []string, deletedAt int64, indices []string, deletedBy string) error {
	if parentField == "" || len(parentIDs) == 0 || len(indices) == 0 {
		return nil
	}

	idsJSON, _ := json.Marshal(parentIDs)
	body := fmt.Sprintf(`{
		"script": {
			"source": "ctx._source.deleted_date = params.deleted_at; ctx._source.deleted_by = params.deleted_by",
			"lang": "painless",
			"params": {
				"deleted_at": %d,
				"deleted_by": "%s"
			}
		},
		"query": {
			"terms": {
				"%s": %s
			}
		}
	}`, deletedAt, deletedBy, parentField, string(idsJSON))

	boolTrue := true
	opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})
	return nil
}

// SyncCascadingUnarchiveInOpenSearchMulti is the unarchive counterpart for multiple IDs.
func SyncCascadingUnarchiveInOpenSearchMulti(ctx context.Context, parentField string, parentIDs []string, indices []string, deletedBy string) error {
	if parentField == "" || len(parentIDs) == 0 || len(indices) == 0 {
		return nil
	}

	idsJSON, _ := json.Marshal(parentIDs)
	body := fmt.Sprintf(`{
		"script": {
			"source": "if (ctx._source.deleted_by == params.deleted_by) { ctx._source.remove('deleted_date'); ctx._source.remove('deleted_by'); }",
			"lang": "painless",
			"params": {
				"deleted_by": "%s"
			}
		},
		"query": {
			"terms": {
				"%s": %s
			}
		}
	}`, deletedBy, parentField, string(idsJSON))

	boolTrue := true
	opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})
	return nil
}

// SyncCascadingDeletionInOpenSearchCombined handles multiple fields × multiple IDs in ONE call.
// fieldIDs maps OpenSearch field names to lists of parent IDs.
func SyncCascadingDeletionInOpenSearchCombined(ctx context.Context, fieldIDs map[string][]string, deletedAt int64, indices []string, deletedBy string) error {
	if len(fieldIDs) == 0 || len(indices) == 0 {
		return nil
	}

	var termsClauses []string
	for field, ids := range fieldIDs {
		idsJSON, _ := json.Marshal(ids)
		termsClauses = append(termsClauses, fmt.Sprintf(`{ "terms": { "%s": %s } }`, field, string(idsJSON)))
	}

	body := fmt.Sprintf(`{
		"script": {
			"source": "ctx._source.deleted_date = params.deleted_at; ctx._source.deleted_by = params.deleted_by",
			"lang": "painless",
			"params": {
				"deleted_at": %d,
				"deleted_by": "%s"
			}
		},
		"query": {
			"bool": {
				"should": [%s],
				"minimum_should_match": 1
			}
		}
	}`, deletedAt, deletedBy, strings.Join(termsClauses, ","))

	boolTrue := true
	opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})
	return nil
}

// SyncCascadingUnarchiveInOpenSearchCombined is the unarchive counterpart for multiple fields × IDs.
func SyncCascadingUnarchiveInOpenSearchCombined(ctx context.Context, fieldIDs map[string][]string, indices []string, deletedBy string) error {
	if len(fieldIDs) == 0 || len(indices) == 0 {
		return nil
	}

	var termsClauses []string
	for field, ids := range fieldIDs {
		idsJSON, _ := json.Marshal(ids)
		termsClauses = append(termsClauses, fmt.Sprintf(`{ "terms": { "%s": %s } }`, field, string(idsJSON)))
	}

	body := fmt.Sprintf(`{
		"script": {
			"source": "if (ctx._source.deleted_by == params.deleted_by) { ctx._source.remove('deleted_date'); ctx._source.remove('deleted_by'); }",
			"lang": "painless",
			"params": {
				"deleted_by": "%s"
			}
		},
		"query": {
			"bool": {
				"should": [%s],
				"minimum_should_match": 1
			}
		}
	}`, deletedBy, strings.Join(termsClauses, ","))

	boolTrue := true
	opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: indices,
		Body:    strings.NewReader(body),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})
	return nil
}

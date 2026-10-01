package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func UpdateChatInOpenSearch(ctx context.Context, openSearchChat *openSearchStruct.OpenSearchChat) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchChat,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchChat.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      "chats",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatInOpenSearch failed to update chat document err: %+v",
			err)
		return
	}
	return
}

func MigrateChatGroupIDInOpensearch(ctx context.Context, oldGrpID, newGrpID string) (err error) {

	var boolTrue = true
	// Build painless script as a single-line string for valid JSON
	painlessScript := `if (ctx._index == 'chats') { ctx._source.chat_grp_id = params.newGrpId; } else if (ctx._index == 'comments') { ctx._source.comment_chat_grp_id = params.newGrpId; } else if (ctx._index == 'attachments') { ctx._source.attachment_chat_grp_id = params.newGrpId; }`

	body := strings.NewReader(fmt.Sprintf(`{
        "script": {
            "source": %q,
            "lang": "painless",
            "params": {
                "newGrpId": "%s"
            }
        },
        "query": {
            "bool": {
                "should": [
                    { "term": { "chat_grp_id": "%s" } },
                    { "term": { "comment_chat_grp_id": "%s" } },
                    { "term": { "attachment_chat_grp_id": "%s" } }
                ],
                "minimum_should_match": 1
            }
        }
    }`, painlessScript, newGrpID, oldGrpID, oldGrpID, oldGrpID))

	resp, err := opensearchInit.OpenSearchClient.UpdateByQuery(context.Background(), opensearchapi.UpdateByQueryReq{
		Indices: []string{"chats", "comments", "attachments"}, // All 3 indices in one go
		Body:    body,
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue, // Make changes visible immediately
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed", // Safe in case of concurrent updates
			MaxDocs:           nil,       // No limit
		},
	})

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MigrateChatGroupIDInOpensearch failed for group migration: %v", err)
		return fmt.Errorf("update_by_query failed: %w", err)
	}

	if len(resp.Failures) > 0 {
		helpers.LogErrorWithContext(ctx, "models/MigrateChatGroupIDInOpensearch had %d failures: %+v", len(resp.Failures), resp.Failures)
		return fmt.Errorf("update_by_query had %d failures", len(resp.Failures))
	}

	return nil
}

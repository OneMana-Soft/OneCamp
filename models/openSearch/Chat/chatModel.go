package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
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

	err = opensearchInit.UpdateDocument(context.Background(), "chats", docId, document)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatInOpenSearch failed to update chat document err: %+v",
			err)
		return
	}
	return
}

// MigrateChatGroupIDInOpensearch moves a conversation's chats, comments and
// files in search from its old grouping id to its new one (someone was added
// to a group). The chats are matched by their whole id (ChatGroupingIDField):
// a term on chat_grp_id itself never matched, so a group's messages stayed
// under the old id, and a search hit on one opened a conversation that was no
// longer there. It runs settled (UpdateByQuerySettled), so an entry another
// write held is moved all the same. Running it again changes nothing.
func MigrateChatGroupIDInOpensearch(ctx context.Context, oldGrpID, newGrpID string) (err error) {
	script, _ := json.Marshal(`if (ctx._index == 'chats') { ctx._source.chat_grp_id = params.newGrpId; } else if (ctx._index == 'comments') { ctx._source.comment_chat_grp_id = params.newGrpId; } else if (ctx._index == 'attachments') { ctx._source.attachment_chat_grp_id = params.newGrpId; }`)
	from, _ := json.Marshal(oldGrpID)
	to, _ := json.Marshal(newGrpID)
	field, _ := json.Marshal(openSearchStruct.ChatGroupingIDField)
	body := fmt.Sprintf(`{
		"script": { "source": %s, "lang": "painless", "params": { "newGrpId": %s } },
		"query": {
			"bool": {
				"should": [
					{ "term": { %s: %s } },
					{ "term": { "comment_chat_grp_id": %s } },
					{ "term": { "attachment_chat_grp_id": %s } }
				],
				"minimum_should_match": 1
			}
		}
	}`, script, to, field, from, from, from)

	if _, err = globalSearchModels.UpdateByQuerySettled(ctx, []string{"chats", "comments", "attachments"}, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/MigrateChatGroupIDInOpensearch couldn't move %s to %s: %v", oldGrpID, newGrpID, err)
		return fmt.Errorf("moving a conversation's search entries: %w", err)
	}
	return nil
}

// GroupingSample is one grouping id that chat entries carry, and one of its
// messages, to ask the graph where that message lives now.
type GroupingSample struct {
	GroupingID string
	ChatUUID   string
}

// ChatGroupingIDs lists the grouping ids chat entries carry, size at a time,
// each with one of its messages. after is the page's cursor, nil for the
// first; next is nil after the last page. Deleted entries count: their ids
// should be right too.
func ChatGroupingIDs(ctx context.Context, after json.RawMessage, size int) (samples []GroupingSample, next json.RawMessage, err error) {
	field, _ := json.Marshal(openSearchStruct.ChatGroupingIDField)
	from := ""
	if len(after) > 0 {
		from = `, "after": ` + string(after)
	}
	body := fmt.Sprintf(`{
		"size": 0,
		"aggs": {
			"ids": {
				"composite": { "size": %d, "sources": [ { "id": { "terms": { "field": %s } } } ]%s },
				"aggs": { "one": { "top_hits": { "size": 1, "_source": ["chat_id"] } } }
			}
		}
	}`, size, field, from)
	if opensearchInit.OpenSearchClient == nil {
		return nil, nil, fmt.Errorf("search is not connected")
	}
	resp, err := opensearchInit.OpenSearchClient.Search(ctx, &opensearchapi.SearchReq{Indices: []string{"chats"}, Body: strings.NewReader(body)})
	if err != nil {
		return nil, nil, err
	}
	var parsed struct {
		IDs struct {
			AfterKey json.RawMessage `json:"after_key"`
			Buckets  []struct {
				Key struct {
					ID string `json:"id"`
				} `json:"key"`
				One struct {
					Hits struct {
						Hits []struct {
							Source struct {
								ChatID string `json:"chat_id"`
							} `json:"_source"`
						} `json:"hits"`
					} `json:"hits"`
				} `json:"one"`
			} `json:"buckets"`
		} `json:"ids"`
	}
	if err := json.Unmarshal(resp.Aggregations, &parsed); err != nil {
		return nil, nil, fmt.Errorf("reading the chats' grouping ids: %w", err)
	}
	for _, b := range parsed.IDs.Buckets {
		s := GroupingSample{GroupingID: b.Key.ID}
		if len(b.One.Hits.Hits) > 0 {
			s.ChatUUID = b.One.Hits.Hits[0].Source.ChatID
		}
		samples = append(samples, s)
	}
	if len(parsed.IDs.Buckets) < size {
		return samples, nil, nil
	}
	return samples, parsed.IDs.AfterKey, nil
}

func CreateChatInOpenSearch(ctx context.Context, openSearchChat *openSearchStruct.OpenSearchChat) (err error) {

	jsonData, err := json.Marshal(openSearchChat)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChatInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchChat.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "chats",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChatInOpenSearch failed to create chat document err: %+v",
			err)
		return
	}
	return
}

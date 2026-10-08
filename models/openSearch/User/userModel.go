package models

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func UpdateUserInOpenSearch(ctx context.Context, openSearchUser *openSearchStruct.OpenSearchUser) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc:         openSearchUser,
		DocAsUpsert: true,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateUserInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchUser.Uuid

	err = opensearchInit.UpdateDocument(context.Background(), "users", docId, document)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateUserInOpenSearch failed to update comment document err: %+v",
			err)
		return
	}
	return
}
func PropagateUserInfoChangeInOpenSearch(ctx context.Context, userUUID string, newFullName string, newProfilePic string) (err error) {
	indices := []string{openSearchStruct.POST_INDEX, openSearchStruct.CHAT_INDEX, openSearchStruct.COMMENT_INDEX, openSearchStruct.ATTACHMENT_INDEX, openSearchStruct.DOC_INDEX, openSearchStruct.TASK_INDEX, openSearchStruct.USER_INDEX}

	for _, index := range indices {
		var script string
		var query string

		switch index {
		case openSearchStruct.POST_INDEX:
			script = `ctx._source.post_by_user_full_name = params.newName; ctx._source.post_by_profile = params.newProfile`
			query = fmt.Sprintf(`{"term": {"post_by_user_id": "%s"}}`, userUUID)

		case openSearchStruct.CHAT_INDEX:
			script = `
				if (ctx._source.chat_by_user_id == params.userId) { 
					ctx._source.chat_by_user_full_name = params.newName; 
					ctx._source.chat_by_profile = params.newProfile; 
				} 
				if (ctx._source.chat_participants != null) { 
					for (int i = 0; i < ctx._source.chat_participants.length; i++) { 
						if (ctx._source.chat_participants[i].user_uuid == params.userId) { 
							ctx._source.chat_participants[i].user_name = params.newName; 
							ctx._source.chat_participants[i].user_profile_object_key = params.newProfile; 
						} 
					} 
				}
			`
			query = fmt.Sprintf(`{
				"bool": {
					"should": [
						{ "term": { "chat_by_user_id": "%s" } },
						{ "nested": { "path": "chat_participants", "query": { "term": { "chat_participants.user_uuid": "%s" } } } }
					]
				}
			}`, userUUID, userUUID)

		case openSearchStruct.COMMENT_INDEX:
			script = `
				if (ctx._source.comment_by_user_id == params.userId) { 
					ctx._source.comment_by_user_full_name = params.newName; 
					ctx._source.comment_by_profile = params.newProfile; 
				}
				if (ctx._source.comment_chat_from_user_id == params.userId) {
					ctx._source.comment_chat_from_user_full_name = params.newName;
				}
				if (ctx._source.comment_chat_participants != null) { 
					for (int i = 0; i < ctx._source.comment_chat_participants.length; i++) { 
						if (ctx._source.comment_chat_participants[i].user_uuid == params.userId) { 
							ctx._source.comment_chat_participants[i].user_name = params.newName; 
							ctx._source.comment_chat_participants[i].user_profile_object_key = params.newProfile; 
						} 
					} 
				}
			`
			query = fmt.Sprintf(`{
				"bool": {
					"should": [
						{ "term": { "comment_by_user_id": "%s" } },
						{ "term": { "comment_chat_from_user_id": "%s" } },
						{ "nested": { "path": "comment_chat_participants", "query": { "term": { "comment_chat_participants.user_uuid": "%s" } } } }
					]
				}
			}`, userUUID, userUUID, userUUID)

		case openSearchStruct.ATTACHMENT_INDEX:
			script = `
				if (ctx._source.attachment_by_user_id == params.userId) { 
					ctx._source.attachment_by_user_full_name = params.newName; 
					ctx._source.attachment_by_profile = params.newProfile; 
				}
				if (ctx._source.attachment_chat_from_user_id == params.userId) {
					ctx._source.attachment_chat_from_user_name = params.newName;
				}
				if (ctx._source.attachment_chat_participants != null) { 
					for (int i = 0; i < ctx._source.attachment_chat_participants.length; i++) { 
						if (ctx._source.attachment_chat_participants[i].user_uuid == params.userId) { 
							ctx._source.attachment_chat_participants[i].user_name = params.newName; 
							ctx._source.attachment_chat_participants[i].user_profile_object_key = params.newProfile; 
						} 
					} 
				}
			`
			query = fmt.Sprintf(`{
				"bool": {
					"should": [
						{ "term": { "attachment_by_user_id": "%s" } },
						{ "term": { "attachment_chat_from_user_id": "%s" } },
						{ "nested": { "path": "attachment_chat_participants", "query": { "term": { "attachment_chat_participants.user_uuid": "%s" } } } }
					]
				}
			}`, userUUID, userUUID, userUUID)

		case openSearchStruct.DOC_INDEX:
			script = `ctx._source.doc_created_by_user_full_name = params.newName; ctx._source.doc_created_by_profile = params.newProfile`
			query = fmt.Sprintf(`{"term": {"doc_created_by_user_id": "%s"}}`, userUUID)

		case openSearchStruct.TASK_INDEX:
			script = `ctx._source.task_assignee_user_full_name = params.newName; ctx._source.task_assignee_profile = params.newProfile`
			query = fmt.Sprintf(`{"term": {"task_assignee_user_id": "%s"}}`, userUUID)
		case openSearchStruct.USER_INDEX:
			script = `ctx._source.user_name = params.newName; ctx._source.user_full_name = params.newName; ctx._source.user_profile_object_key = params.newProfile`
			query = fmt.Sprintf(`{"term": {"uuid": "%s"}}`, userUUID)
		}

		body := map[string]interface{}{
			"script": map[string]interface{}{
				"source": script,
				"lang":   "painless",
				"params": map[string]interface{}{
					"userId":     userUUID,
					"newName":    newFullName,
					"newProfile": newProfilePic,
				},
			},
			"query": json.RawMessage(query),
		}

		jsonData, err := json.Marshal(body)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/PropagateUserInfoChangeInOpenSearch Error marshaling body for index %s err: %+v", index, err)
			continue
		}

		// conflicts=proceed, because a version conflict here is NORMAL, not a fault.
		//
		// This walks live indices while people are posting. Two propagations for the same user (a
		// rename and an avatar change, or a retry overlapping its predecessor) touch the same
		// documents, and without this the whole update_by_query is treated as failed the moment
		// one document moved underneath it. Beta showed exactly that: total 9, updated 0,
		// version_conflicts 9 — every document skipped on one pass, while a concurrent pass
		// reported updated 71 of the same 111. With proceed, conflicted documents are skipped and
		// the rest are written, which is the right semantics for a denormalised copy that the
		// next write to those documents corrects anyway.
		res, uerr := opensearchInit.OpenSearchClient.UpdateByQuery(
			context.Background(),
			opensearchapi.UpdateByQueryReq{
				Indices: []string{index},
				Body:    openSearchStruct.IndexReader(jsonData),
				Params:  opensearchapi.UpdateByQueryParams{Conflicts: "proceed"},
			},
		)
		err = uerr

		// A COUNT, NOT THE RESPONSE BODY.
		//
		// A partial update_by_query returns HTTP 200 with a failures array, so the client could
		// not parse it as an error and %+v rendered the ENTIRE response — every conflicted
		// document with its seqNo and index_uuid, thousands of characters on one line, once per
		// index. Those lines are batched to the OTel collector, which made the least useful log
		// on the box one of the most expensive. These four numbers are what a reader needs, and
		// they are emitted only when something was actually skipped.
		if err == nil && res != nil && res.VersionConflicts > 0 {
			helpers.LogInfoWithContext(ctx,
				"models/PropagateUserInfoChangeInOpenSearch index=%s total=%d updated=%d "+
					"skipped_on_conflict=%d (skipped documents keep the previous name/avatar "+
					"until their next write)",
				index, res.Total, res.Updated, res.VersionConflicts)
		}

		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/PropagateUserInfoChangeInOpenSearch Failed to update index %s err: %+v", index, err)
		}
	}
	return nil
}

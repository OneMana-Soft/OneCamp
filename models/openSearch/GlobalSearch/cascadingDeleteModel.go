package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// exactClauses match a field holding one of ids exactly: as the field, and as
// its keyword sub-field. A field an index mapped by itself as text (the AI
// index's doc_uuid, on a comment's entry) is analyzed, so a term query for a
// whole uuid never matches it, and deleting a doc left its comments findable
// in AI search; its .keyword holds the value whole. Where no such sub-field
// exists, that clause matches nothing.
func exactClauses(field string, ids []string) []string {
	idsJSON, _ := json.Marshal(ids)
	fieldJSON, _ := json.Marshal(field)
	keywordJSON, _ := json.Marshal(field + ".keyword")
	return []string{
		fmt.Sprintf(`{ "terms": { %s: %s } }`, fieldJSON, idsJSON),
		fmt.Sprintf(`{ "terms": { %s: %s } }`, keywordJSON, idsJSON),
	}
}

// EntryDeletedPainless is true, in an update-by-query's script, for an entry
// deleted already. A deletion time below 1 was never a deletion (a doc's new
// comments were once written with -1, and a team's restore writes -1): such
// an entry is live.
const EntryDeletedPainless = `(ctx._source.deleted_date != null && !(ctx._source.deleted_date instanceof Number && ((Number) ctx._source.deleted_date).longValue() < 1))`

// cascadeDeleteScript marks an entry deleted by a cascade from its parent (a
// doc, post, chat, task, channel or project going away), unless the entry is
// deleted already: then its deletion time and marker stay as they are. The
// cascade wrote its own over them, so a comment its author had deleted was
// marked "cascade", and restoring the parent, which clears what the cascade
// marked, brought the comment back to search for everyone who may read the
// parent.
const cascadeDeleteScript = `if ` + EntryDeletedPainless + ` { ctx.op = 'noop'; return; }
ctx._source.deleted_date = params.deleted_at;
ctx._source.deleted_by = params.deleted_by;`

// hideScript marks an entry hidden by its own marker. Like
// cascadeDeleteScript it leaves an entry a person deleted as it is, but it
// takes over a cascade's mark: a comment of a trashed task is marked
// "cascade", which restoring the task clears, and that would bring back what
// was hidden under it. EntryDeletedPainless is one parenthesised expression,
// so the whole condition needs its own parentheses, or the script doesn't
// compile and every hide fails.
const hideScript = `if (` + EntryDeletedPainless + ` && ctx._source.deleted_by != 'cascade') { ctx.op = 'noop'; return; }
ctx._source.deleted_date = params.deleted_at;
ctx._source.deleted_by = params.deleted_by;`

// handOverScript passes an entry marked by params.from to another marker,
// params.deleted_by, at params.deleted_at: what a hide took from under a
// trashed task or an archived project, handed back to that cascade, whose
// restore then brings it back with the rest.
const handOverScript = `if (ctx._source.deleted_by != params.from) { ctx.op = 'noop'; return; }
ctx._source.deleted_date = params.deleted_at;
ctx._source.deleted_by = params.deleted_by;`

// cascadeDeleteBody is the update-by-query body that has cascadeDeleteScript
// mark what query matches as deleted at deletedAt by deletedBy. Pure.
func cascadeDeleteBody(query string, deletedAt int64, deletedBy string) string {
	return markBody(cascadeDeleteScript, query, map[string]any{"deleted_at": deletedAt, "deleted_by": deletedBy})
}

// markBody is the update-by-query body that runs script, with params, over
// what query matches. Pure.
func markBody(script, query string, params map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"source": script, "lang": "painless", "params": params})
	return fmt.Sprintf(`{"script": %s, "query": %s}`, raw, query)
}

// HideInOpenSearch marks hidden, at hiddenAt by hiddenBy, the entries in
// indices that name id in any of fields (hideScript):
// SyncCascadingUnarchiveInOpenSearch with the same marker brings them back,
// or HandOverInOpenSearch hands them to a cascade.
func HideInOpenSearch(ctx context.Context, fields []string, id string, hiddenAt int64, indices []string, hiddenBy string) error {
	if len(fields) == 0 || id == "" || len(indices) == 0 {
		return nil
	}
	_, err := updateByQuerySettled(ctx, indices, markBody(hideScript, naming(fields, id),
		map[string]any{"deleted_at": hiddenAt, "deleted_by": hiddenBy}))
	return err
}

// HandOverInOpenSearch passes the entries in indices that name id in any of
// fields, and that from marked, to the marker to at at (handOverScript), in
// one write: a comment that comes back under a trashed task or an archived
// project stays out of search until that restore.
func HandOverInOpenSearch(ctx context.Context, fields []string, id, from string, at int64, indices []string, to string) error {
	if len(fields) == 0 || id == "" || len(indices) == 0 {
		return nil
	}
	_, err := updateByQuerySettled(ctx, indices, markBody(handOverScript, naming(fields, id),
		map[string]any{"from": from, "deleted_at": at, "deleted_by": to}))
	return err
}

// naming is the query for the entries that name id in any of fields. Pure.
func naming(fields []string, id string) string {
	var should []string
	for _, field := range fields {
		should = append(should, exactClauses(field, []string{id})...)
	}
	return anyOf(should)
}

// anyOf is a query matching what any of the clauses matches. Pure.
func anyOf(clauses []string) string {
	return fmt.Sprintf(`{"bool": {"should": [%s], "minimum_should_match": 1}}`, strings.Join(clauses, ","))
}

// SyncCascadingDeletionInOpenSearch marks deleted, at deletedAt, the entries
// in indices that name parentID in any of parentFields, except those deleted
// already (cascadeDeleteScript). deletedBy tags the deletion: "cascade" for a
// parent going away, which restoring the parent clears; "user" for an item
// deleted by itself, which nothing brings back. Every cascade here runs under
// its caller's ctx, so a settle's time limit holds (business/Board
// settleBoardDelete, business/Doc searchFollowsTrash).
func SyncCascadingDeletionInOpenSearch(ctx context.Context, parentFields []string, parentID string, deletedAt int64, indices []string, deletedBy string) error {
	if len(parentFields) == 0 || parentID == "" || len(indices) == 0 {
		return nil
	}

	var shouldClauses []string
	for _, field := range parentFields {
		shouldClauses = append(shouldClauses, exactClauses(field, []string{parentID})...)
	}

	body := cascadeDeleteBody(anyOf(shouldClauses), deletedAt, deletedBy)

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingDeletionInOpenSearch failed: %v", err)
		return err
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
		shouldClauses = append(shouldClauses, exactClauses(field, []string{parentID})...)
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

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingUnarchiveInOpenSearch failed: %v", err)
		return err
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

	// The caller's ctx, so that its time limit holds (domain/Doc gives each
	// doc search write two minutes).
	if _, err := updateByQuerySettled(ctx, []string{"comments", "attachments"}, body); err != nil {
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
	fieldJSON, _ := json.Marshal(parentField)
	body := cascadeDeleteBody(fmt.Sprintf(`{"terms": {%s: %s}}`, fieldJSON, idsJSON), deletedAt, deletedBy)

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingDeletionInOpenSearchMulti failed: %v", err)
		return err
	}
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

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingUnarchiveInOpenSearchMulti failed: %v", err)
		return err
	}
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
		termsClauses = append(termsClauses, exactClauses(field, ids)...)
	}

	body := cascadeDeleteBody(anyOf(termsClauses), deletedAt, deletedBy)

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingDeletionInOpenSearchCombined failed: %v", err)
		return err
	}
	return nil
}

// SyncCascadingUnarchiveInOpenSearchCombined is the unarchive counterpart for multiple fields × IDs.
func SyncCascadingUnarchiveInOpenSearchCombined(ctx context.Context, fieldIDs map[string][]string, indices []string, deletedBy string) error {
	if len(fieldIDs) == 0 || len(indices) == 0 {
		return nil
	}

	var termsClauses []string
	for field, ids := range fieldIDs {
		termsClauses = append(termsClauses, exactClauses(field, ids)...)
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

	if _, err := updateByQuerySettled(ctx, indices, body); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SyncCascadingUnarchiveInOpenSearchCombined failed: %v", err)
		return err
	}
	return nil
}

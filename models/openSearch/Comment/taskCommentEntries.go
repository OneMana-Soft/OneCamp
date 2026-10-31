package models

// The task comments' search entries that name no project, and their rewrite
// (business/Comment searchRepair.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	globalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// TaskCommentEntriesWithoutProject is a page of the task comments' entries
// that name no project, in comment id order, after the id given ("" for the
// first page): their ids (each entry's id is its comment's), and next, the
// id to read on from ("" when this page ends them). Paged by id rather than
// by offset, so entries rewritten meanwhile, which leave the set, move
// nothing.
func TaskCommentEntriesWithoutProject(ctx context.Context, after string, size int) (ids []string, next string, err error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, "", fmt.Errorf("search is not connected")
	}
	query := map[string]any{
		"size":    size,
		"_source": false,
		"query": map[string]any{"bool": map[string]any{
			"filter":   []any{map[string]any{"exists": map[string]any{"field": "comment_task_id"}}},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "comment_project_id"}}},
		}},
		"sort": []any{map[string]any{"comment_id": "asc"}},
	}
	if after != "" {
		query["search_after"] = []string{after}
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, "", err
	}
	resp, err := opensearchInit.OpenSearchClient.Search(ctx, &opensearchapi.SearchReq{Indices: []string{openSearchStruct.COMMENT_INDEX}, Body: strings.NewReader(string(body))})
	if err != nil {
		return nil, "", err
	}
	hits := resp.Hits.Hits
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	if len(hits) == size && len(hits[len(hits)-1].Sort) == 1 {
		next, _ = hits[len(hits)-1].Sort[0].(string)
	}
	return ids, next, nil
}

// taskCommentEntryScript writes a task comment's entry as it is written now
// (params.entries, by comment id) onto one that still names no project: one
// rewritten already, or written since, is left as it is. An entry edited
// since it was written (updated_date) keeps its text, which the edit wrote
// as plain text. Whether and why it is deleted stays as it was, except that
// a comment of an archived project (params.archived, its archiving by
// comment id) is marked deleted with the project as archiving it marked its
// other comments, by the cascade's rule: an entry deleted already keeps its
// deletion. Marked in the same write, so no entry is ever rewritten and left
// unmarked; a later run would not find it, since it names a project.
const taskCommentEntryScript = `
def e = params.entries.get(ctx._id);
if (e == null || ctx._source.comment_project_id != null) { ctx.op = 'noop'; return; }
for (def f : e.entrySet()) {
	if (f.getKey() == 'comment_body' && ctx._source.updated_date != null) { continue; }
	ctx._source[f.getKey()] = f.getValue();
}
def archived = params.archived.get(ctx._id);
if (archived != null && !` + globalSearchModels.EntryDeletedPainless + `) {
	ctx._source.deleted_date = archived;
	ctx._source.deleted_by = params.deleted_by;
}`

// RewriteTaskCommentEntries writes each entry (by comment id) over the one
// search holds for its comment, if that one still names no project, in one
// update-by-query, and answers how many it changed. The entries' fields go
// through the index's guard (openSearchStruct.SanitizeIndexedJSON); their
// deletion is left out, so a deleted comment stays deleted. archived holds,
// by comment id, when the comment's project was archived (Unix seconds), for
// the comments of archived projects: those are marked deleted with it, as
// the project's archive cascade marks its comments ("cascade", which
// restoring the project clears).
func RewriteTaskCommentEntries(ctx context.Context, entries map[string]*openSearchStruct.OpenSearchComment, archived map[string]int64) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	if archived == nil {
		archived = map[string]int64{} // the script reads it; a nil map is sent as null
	}
	fields := make(map[string]map[string]any, len(entries))
	ids := make([]string, 0, len(entries))
	for id, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return 0, err
		}
		if raw, err = openSearchStruct.SanitizeIndexedJSON(raw); err != nil {
			return 0, err
		}
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			return 0, err
		}
		delete(f, "deleted_date")
		fields[id] = f
		ids = append(ids, id)
	}
	body, err := json.Marshal(map[string]any{
		"script": map[string]any{"source": taskCommentEntryScript, "lang": "painless",
			"params": map[string]any{"entries": fields, "archived": archived, "deleted_by": "cascade"}},
		"query": map[string]any{"bool": map[string]any{
			"filter":   []any{map[string]any{"ids": map[string]any{"values": ids}}},
			"must_not": []any{map[string]any{"exists": map[string]any{"field": "comment_project_id"}}},
		}},
	})
	if err != nil {
		return 0, err
	}
	return globalSearchModels.UpdateByQuerySettled(ctx, []string{openSearchStruct.COMMENT_INDEX}, string(body))
}

package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DocChildFields is what a doc's comments' and files' search entries carry of
// their doc, and whether the doc is live.
type DocChildFields struct {
	Title      string   `json:"title"`
	Private    bool     `json:"private"`
	Reading    []string `json:"reading"`
	Editing    []string `json:"editing"`
	Commenting []string `json:"commenting"`
	CreatedBy  string   `json:"createdBy"`
	Live       bool     `json:"live"`
}

// docChildrenScript writes onto a comment's or a file's entry what it carries
// of its doc, and brings back a comment on a live doc that was written as
// deleted (-1, never a real deletion time). An entry already right is left as
// it is (noop), so a second run changes nothing and says so.
const docChildrenScript = `
def d = params.docs.get(ctx._index == 'comments' ? ctx._source.comment_doc_id : ctx._source.attachment_doc_id);
if (d == null) { ctx.op = 'noop'; return; }
String p = ctx._index == 'comments' ? 'comment_doc_' : 'attachment_doc_';
boolean changed = false;
Map want = ['title': d.title, 'private': d.private, 'reading_users': d.reading, 'editing_users': d.editing,
	'commenting_users': d.commenting, 'created_by_user_id': d.createdBy];
for (def e : want.entrySet()) {
	String k = p + e.getKey();
	if (ctx._source[k] != e.getValue()) { ctx._source[k] = e.getValue(); changed = true; }
}
if (ctx._index == 'comments' && d.live) {
	def del = ctx._source.deleted_date;
	if (del != null && del instanceof Number && ((Number) del).longValue() < 0) { ctx._source.remove('deleted_date'); changed = true; }
}
if (!changed) { ctx.op = 'noop'; }
`

// RepairDocChildren writes onto every comment and file entry of the given
// docs (keyed by uuid) what it carries of its doc, in one update-by-query.
// It answers how many entries it changed, and how many of those were comments
// written as deleted that are now findable.
func RepairDocChildren(ctx context.Context, docs map[string]DocChildFields) (changed, revived int, err error) {
	if len(docs) == 0 {
		return 0, 0, nil
	}
	if opensearchInit.OpenSearchClient == nil {
		return 0, 0, fmt.Errorf("search is not connected")
	}
	ids := make([]string, 0, len(docs))
	var live []string
	for id, d := range docs {
		ids = append(ids, id)
		if d.Live {
			live = append(live, id)
		}
	}
	idsJSON, _ := json.Marshal(ids)

	// Counted first: the update says only how many entries it changed.
	if len(live) > 0 {
		liveJSON, _ := json.Marshal(live)
		resp, cerr := opensearchInit.OpenSearchClient.Indices.Count(ctx, &opensearchapi.IndicesCountReq{
			Indices: []string{"comments"},
			Body: strings.NewReader(fmt.Sprintf(`{"query": {"bool": {"filter": [
				{"terms": {"comment_doc_id": %s}},
				{"range": {"deleted_date": {"lt": 0}}}]}}}`, liveJSON)),
		})
		if cerr != nil {
			return 0, 0, cerr
		}
		revived = resp.Count
	}

	params, _ := json.Marshal(map[string]any{"docs": docs})
	scriptJSON, _ := json.Marshal(docChildrenScript)
	body := fmt.Sprintf(`{
		"script": {"source": %s, "lang": "painless", "params": %s},
		"query": {"bool": {"should": [
			{"terms": {"comment_doc_id": %s}},
			{"terms": {"attachment_doc_id": %s}}
		], "minimum_should_match": 1}}
	}`, scriptJSON, params, idsJSON, idsJSON)
	// Settled: an entry another write held at the same moment is written on a
	// later run, so the repair is never recorded done with it left behind.
	changed, err = updateByQuerySettled(ctx, []string{"comments", "attachments"}, body)
	if err != nil {
		return changed, revived, fmt.Errorf("comment and file entries: %w", err)
	}
	return changed, revived, nil
}

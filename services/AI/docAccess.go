package ai

// A doc's privacy and sharing, on its entries in the AI search index.
//
// A doc's own entry, and the entry of each comment on it, carry the doc's
// privacy and who it is shared with: that is what AI search filters them by
// (buildPermissionFilter). They are written when the doc or the comment is
// embedded. Making a doc private, or sharing it with someone or no longer,
// changes no text and so re-embeds nothing; SetDocAccess writes the doc's
// current privacy and sharing onto every entry it has. business/Doc calls it
// when either changes, and once over every private doc (its backfill).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// DocAccess is a doc's privacy and sharing, as its entries carry them. The
// people are user uuids.
type DocAccess struct {
	DocUUID    string
	Private    bool
	CreatedBy  string
	Reading    []string
	Editing    []string
	Commenting []string
}

// docAccessScript sets an entry's doc fields from params.docs, keyed by the
// doc the entry belongs to: its own uuid for a doc's entry, doc_uuid for a
// comment's. An entry of a doc not in params is left as it is.
const docAccessScript = "def id = ctx._source.content_type == 'doc' ? ctx._source.content_uuid : ctx._source.doc_uuid; " +
	"def a = id == null ? null : params.docs[id]; " +
	"if (a == null) { ctx.op = 'noop'; } else { " +
	"ctx._source.doc_private = a['is_private']; " +
	"ctx._source.doc_created_by_user_id = a['created_by']; " +
	"ctx._source.doc_reading_users = a['reading']; " +
	"ctx._source.doc_editing_users = a['editing']; " +
	"ctx._source.doc_commenting_users = a['commenting']; }"

// docAccessUpdateBody is the update-by-query that writes docs' access onto
// their entries. Pure, so its shape is tested without a cluster.
func docAccessUpdateBody(docs []DocAccess) ([]byte, error) {
	ids := make([]string, 0, len(docs))
	params := make(map[string]any, len(docs))
	list := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	for _, d := range docs {
		if strings.TrimSpace(d.DocUUID) == "" {
			continue
		}
		ids = append(ids, d.DocUUID)
		params[d.DocUUID] = map[string]any{
			"is_private": d.Private,
			"created_by": d.CreatedBy,
			"reading":    list(d.Reading),
			"editing":    list(d.Editing),
			"commenting": list(d.Commenting),
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return json.Marshal(map[string]any{
		"script": map[string]any{
			"source": docAccessScript,
			"lang":   "painless",
			"params": map[string]any{"docs": params},
		},
		"query": map[string]any{"bool": map[string]any{
			"should": []any{
				map[string]any{"bool": map[string]any{"filter": []any{
					map[string]any{"term": map[string]any{"content_type": "doc"}},
					map[string]any{"terms": map[string]any{"content_uuid": ids}},
				}}},
				// A comment names its doc in doc_uuid, a field the index maps
				// by itself (text, with a keyword sub-field), so both forms are
				// asked; the script decides what is written either way.
				map[string]any{"terms": map[string]any{"doc_uuid.keyword": ids}},
				map[string]any{"terms": map[string]any{"doc_uuid": ids}},
			},
			"minimum_should_match": 1,
		}},
	})
}

// SetDocAccess writes each doc's privacy and sharing onto its own entry and
// its comments' entries, in one request. Entries of other docs are untouched,
// and writing the same values twice is harmless.
func SetDocAccess(ctx context.Context, docs []DocAccess) error {
	body, err := docAccessUpdateBody(docs)
	if err != nil || body == nil {
		return err
	}
	if opensearchInit.OpenSearchClient == nil {
		return fmt.Errorf("OpenSearch client not initialized")
	}
	refresh := true
	resp, err := opensearchInit.OpenSearchClient.UpdateByQuery(ctx, opensearchapi.UpdateByQueryReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(string(body)),
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &refresh,
			WaitForCompletion: &refresh,
			Conflicts:         "proceed",
		},
	})
	if err != nil {
		return fmt.Errorf("writing docs' access to the AI index: %w", err)
	}
	if resp != nil && len(resp.Failures) > 0 {
		return fmt.Errorf("writing docs' access to the AI index: %d entries failed", len(resp.Failures))
	}
	return nil
}

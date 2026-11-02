package opensearchInit

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// docAccessProperties are the docs index's fields for who reaches a doc
// besides the people it is shared with by name (docs programme A5): its owner,
// its full-access people, the teams it is shared with and its space. Keywords,
// so a search filters on whole uuids.
const docAccessProperties = `
					"doc_owner_user_id": { "type": "keyword", "index": true },
					"doc_full_access_users": { "type": "keyword", "index": true },
					"doc_team_uuids": { "type": "keyword", "index": true },
					"doc_space_kind": { "type": "keyword", "index": true },
					"doc_space_uuid": { "type": "keyword", "index": true }`

// boardTextProperties are the boards index's field for what a board says
// (business/Board boardText.go): its frames' names and its words.
const boardTextProperties = `
					"board_text": { "type": "text", "index": true }`

// aiBoardAccessProperties are the AI index's fields for who reaches a board's
// entries (its text, its comments): keywords, so a filter matches whole uuids.
// An index that took them by itself before has them as text with a keyword
// sub-field, which the AI's board filter also asks (services/AI boardGrant).
const aiBoardAccessProperties = `
				"board_uuid": { "type": "keyword", "index": true },
				"board_public": { "type": "boolean", "index": true },
				"board_created_by_user_id": { "type": "keyword", "index": true },
				"board_reading_users": { "type": "keyword", "index": true },
				"board_editing_users": { "type": "keyword", "index": true },
				"board_commenting_users": { "type": "keyword", "index": true }`

// ensureDocAccessMapping adds the docs index's access fields to an index made
// before them.
func ensureDocAccessMapping(ctx context.Context) { ensureMapping(ctx, "docs", docAccessProperties) }

// ensureMapping adds props to an index made before them, before any entry
// carries them: left to dynamic mapping, a uuid would be text, split at its
// hyphens, and no search filter would match it. Adding a field is allowed on a
// live index. A field the index already holds as something else keeps its
// type (OpenSearch never changes one), which is said at INFO: the searches on
// such fields ask their keyword sub-field too. Any other failure is logged;
// every hit is checked against the graph either way.
func ensureMapping(ctx context.Context, index, props string) {
	if OpenSearchClient == nil {
		return
	}
	_, err := OpenSearchClient.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
		Indices: []string{index},
		Body:    strings.NewReader(`{ "properties": {` + props + ` } }`),
	})
	switch {
	case err == nil:
	case strings.Contains(err.Error(), "cannot be changed from type"):
		helpers.LogInfoWithContext(ctx, "opensearchInit: the %s index keeps the types it gave these fields itself: %v", index, err)
	default:
		helpers.LogErrorWithContext(ctx, "opensearchInit: the %s index didn't take its fields: %+v", index, err)
	}
}

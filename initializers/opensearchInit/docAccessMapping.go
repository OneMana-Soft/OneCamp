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

// ensureDocAccessMapping adds those fields to a docs index made before them,
// before any entry carries them: left to dynamic mapping, a uuid would be
// text, split at its hyphens, and no search filter would match it. Adding a
// field is allowed on a live index; a failure is logged, and global search
// then finds those docs only through the people they're shared with by name
// (every hit is checked against the graph either way).
func ensureDocAccessMapping(ctx context.Context) {
	if OpenSearchClient == nil {
		return
	}
	_, err := OpenSearchClient.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
		Indices: []string{"docs"},
		Body:    strings.NewReader(`{ "properties": {` + docAccessProperties + ` } }`),
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "opensearchInit: the docs index didn't take the access fields: %+v", err)
	}
}

package models

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// IDHit is one hit as an id: the index it came from and the uuid its entry
// names in idFields (the entry's own id when it names none).
type IDHit struct {
	Index string
	ID    string
}

// SearchIDs runs query over indices and returns each hit's index and id, best
// first: for a caller that wants which things matched, not their entries
// (Who knows' seeds). idFields is the source field holding the id, by index.
func SearchIDs(ctx context.Context, indices []string, query string, idFields map[string]string) ([]IDHit, error) {
	var resp struct {
		Hits struct {
			Hits []opensearchapi.SearchHit `json:"hits"`
		} `json:"hits"`
	}
	if _, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: indices,
		Body:    strings.NewReader(query),
	}, &resp); err != nil {
		return nil, err
	}
	out := make([]IDHit, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		id := h.ID
		if f := idFields[h.Index]; f != "" && len(h.Source) > 0 {
			var src map[string]any
			if json.Unmarshal(h.Source, &src) == nil {
				if s, ok := src[f].(string); ok && s != "" {
					id = s
				}
			}
		}
		out = append(out, IDHit{Index: h.Index, ID: id})
	}
	return out, nil
}

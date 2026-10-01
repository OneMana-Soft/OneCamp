package notion

import (
	"context"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Discover lists databases visible to the integration token. We filter
// to "task-shaped" databases (Title + Status / select-named-Status) so
// the operator's pick list stays scoped.
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	body := map[string]any{
		"filter":    map[string]any{"value": "database", "property": "object"},
		"page_size": 100,
	}
	var resp struct {
		Results    []notionDatabase `json:"results"`
		HasMore    bool             `json:"has_more"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := p.postJSON(ctx, token.AccessToken, "/search", body, &resp); err != nil {
		return nil, err
	}
	out := []importProvider.DiscoverItem{}
	for _, db := range resp.Results {
		out = append(out, importProvider.DiscoverItem{
			ID:   db.ID,
			Name: databaseTitle(&db),
			URL:  db.URL,
			Kind: "database",
			Meta: map[string]any{
				"task_shaped": looksLikeTaskDatabase(db.Properties),
			},
		})
	}
	return out, nil
}

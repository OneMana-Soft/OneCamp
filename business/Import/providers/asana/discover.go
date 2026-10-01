package asana

import (
	"context"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Discover lists Asana workspaces visible to the connected token.
// Endpoint: /workspaces.
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	var resp asanaWorkspacesResp
	if err := p.getJSON(ctx, token.AccessToken, "/workspaces?limit=100&opt_fields=gid,name", &resp); err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(resp.Data))
	for _, w := range resp.Data {
		out = append(out, importProvider.DiscoverItem{
			ID:   w.GID,
			Name: w.Name,
			Kind: "workspace",
			URL:  "https://app.asana.com/0/" + w.GID,
		})
	}
	return out, nil
}

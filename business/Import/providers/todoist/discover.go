package todoist

import (
	"context"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Discover lists Todoist projects visible to the connected token.
// We use the Sync API (a single POST) because it's already cached on
// the provider for jobs; here we issue a fresh tiny sync limited to
// the projects resource.
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	snap, err := p.sync(ctx, token.AccessToken, []string{"projects"})
	if err != nil {
		return nil, err
	}
	out := []importProvider.DiscoverItem{}
	for _, pr := range snap.activeProjects() {
		out = append(out, importProvider.DiscoverItem{
			ID:   pr.ID,
			Name: pr.Name,
			Kind: "project",
			URL:  "https://todoist.com/app/projects/" + pr.ID,
			Meta: map[string]any{"color": pr.Color},
		})
	}
	return out, nil
}

package trello

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Discover lists boards visible to the connected token.
// Endpoint: GET /1/members/me/boards?fields=id,name,desc,url,closed&filter=open
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	apiKey := ""
	if len(token.Metadata) > 0 {
		var md struct {
			APIKey string `json:"api_key"`
		}
		_ = json.Unmarshal(token.Metadata, &md)
		apiKey = md.APIKey
	}
	if apiKey == "" {
		return nil, errors.New("trello token missing api_key in metadata; reconnect")
	}
	u := fmt.Sprintf("https://api.trello.com/1/members/me/boards?filter=open&fields=id,name,desc,url,closed&key=%s&token=%s",
		url.QueryEscape(apiKey),
		url.QueryEscape(token.AccessToken))
	var boards []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Desc   string `json:"desc"`
		URL    string `json:"url"`
		Closed bool   `json:"closed"`
	}
	if err := p.getJSON(ctx, u, &boards); err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(boards))
	for _, b := range boards {
		if b.Closed {
			continue
		}
		out = append(out, importProvider.DiscoverItem{
			ID:          b.ID,
			Name:        b.Name,
			Description: b.Desc,
			URL:         b.URL,
			Kind:        "board",
		})
	}
	return out, nil
}

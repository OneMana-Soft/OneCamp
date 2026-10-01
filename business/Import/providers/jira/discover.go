package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// Discover lists Jira projects visible at the configured site URL.
// The token's metadata must include site_url + (email | bearer="true").
func (p *Provider) Discover(ctx context.Context, ownerUserId string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	var md struct {
		SiteURL string `json:"site_url"`
		Email   string `json:"email"`
		Bearer  string `json:"bearer"`
	}
	_ = json.Unmarshal(token.Metadata, &md)
	site := strings.TrimRight(md.SiteURL, "/")
	if site == "" {
		return nil, errors.New("jira token missing metadata.site_url")
	}
	auth := ""
	if md.Bearer == "true" {
		auth = "Bearer " + token.AccessToken
	} else {
		if md.Email == "" {
			return nil, errors.New("jira token missing metadata.email")
		}
		auth = base64.StdEncoding.EncodeToString([]byte(md.Email + ":" + token.AccessToken))
	}
	projects, err := p.listProjects(ctx, auth, site)
	if err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(projects))
	for _, pr := range projects {
		out = append(out, importProvider.DiscoverItem{
			ID:          pr.Key,
			Name:        pr.Name,
			Description: pr.Description,
			URL:         site + "/browse/" + pr.Key,
			Kind:        "project",
		})
	}
	return out, nil
}

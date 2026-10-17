package business

import (
	"testing"
	"time"

	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// A repository's webhook is signed with its oldest live link's secret: one
// signer per repository, its name in any case, and a link with no secret
// signs nothing.
func TestARepositorysWebhookIsSignedByItsOldestLink(t *testing.T) {
	secret := func(s string) *string { return &s }
	at := func(day int) time.Time { return time.Date(2026, 10, day, 9, 0, 0, 0, time.UTC) }
	link := func(owner, name string, day int, s *string) *githubLinkModel.GitHubLink {
		return &githubLinkModel.GitHubLink{Id: uuid.New(), RepoOwner: owner, RepoName: name, CreatedAt: at(day), WebhookSecret: s}
	}
	newestWeb, oldestWeb := link("acme", "web", 9, secret("newest")), link("Acme", "Web", 2, secret("oldest"))
	docs := link("acme", "docs", 5, secret("docs"))
	links := []*githubLinkModel.GitHubLink{
		newestWeb, nil, link("acme", "web", 1, nil), docs, oldestWeb, link("acme", "billing", 3, secret("")),
	}
	got := webhookSigners(links)
	if len(got) != 2 || got[0] != oldestWeb || got[1] != docs {
		t.Errorf("signers %v, want acme/web's oldest link with a secret, then acme/docs's", got)
	}
	if n := len(webhookSigners(nil)); n != 0 {
		t.Errorf("no links gave %d signers", n)
	}
}

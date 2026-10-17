package business

// A repository has one OneCamp webhook, whichever projects link it, and it
// sends only the events it was registered for. One registered before an event
// joined them never sends that one (branch deletions, "delete", joined
// later), so each start registers every linked repository's webhook again:
// the events it sends, and the secret it is signed with, its oldest live
// link's (webhookSigners), which every delivery's check accepts.

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
)

// StartWebhookRefresh registers each linked repository's webhook again, once,
// in the background.
func StartWebhookRefresh(ctx context.Context) {
	helpers.GoSafeNamed("github.webhook-refresh", func() {
		links, err := GetAllLinkedRepos(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "github: couldn't read the linked repositories to bring their webhooks up to date, will at the next start: %v", err)
			return
		}
		for _, signer := range webhookSigners(links) {
			registerGitHubWebhook(signer, *signer.WebhookSecret)
		}
	})
}

// webhookSigners is, for each repository the links reach (its name in any
// case), its oldest link with a webhook secret: the one its webhook is signed
// with. Pure.
func webhookSigners(links []*githubLinkModel.GitHubLink) []*githubLinkModel.GitHubLink {
	oldest := map[string]*githubLinkModel.GitHubLink{}
	var repos []string
	for _, l := range links {
		if l == nil || l.WebhookSecret == nil || *l.WebhookSecret == "" {
			continue
		}
		repo := linkCacheKey(l.RepoOwner, l.RepoName)
		current, seen := oldest[repo]
		if !seen {
			repos = append(repos, repo)
		}
		if !seen || l.CreatedAt.Before(current.CreatedAt) {
			oldest[repo] = l
		}
	}
	signers := make([]*githubLinkModel.GitHubLink, 0, len(repos))
	for _, repo := range repos {
		signers = append(signers, oldest[repo])
	}
	return signers
}

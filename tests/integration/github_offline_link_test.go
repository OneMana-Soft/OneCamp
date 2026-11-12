//go:build integration

// V-841: the demo's fixture link (business/DemoSeed CodeLinked writes it for
// Q4 launch) has nothing to reach GitHub with: no installation, no webhook
// secret, no sync. It is there only so the start's repair (RepairTaskLinks)
// keeps the seeded pull request on its task. Every read of live links leaves
// it out, so no list shows it and no reader calls GitHub for it. A link made
// in the app always has a secret (LinkRepositoryToProject), and reads as it
// did.
//
// This edition's form of the AI edition's test: the same test, less its leg
// for the AI's code tools (ProjectsLinking), which this edition doesn't have.
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAnOfflineFixtureLinkIsLeftOutOfEveryLiveRead(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	w := seedGitHubWorkspace(t, env, dg)
	project := uuid.MustParse(w.project)

	// The fixture, as the seed writes it, and a link made in the app with
	// every sync and auto-create off (LinkRepositoryToProject takes the syncs
	// from the client and never sets an installation): its webhook secret
	// alone tells it from the fixture, so every read below must keep it.
	offline, real := uuid.New(), uuid.New()
	if err := githubLinkModel.CreateGitHubLink(ctx, offline, project, "demo-team", "q4-launch-site", nil, nil, false, false, false, w.people["ana"], time.Now()); err != nil {
		t.Fatalf("the fixture link: %v", err)
	}
	secret := "a-real-link-secret"
	if err := githubLinkModel.CreateGitHubLink(ctx, real, project, "acme", "offline-site", nil, &secret, false, false, false, w.people["ana"], time.Now()); err != nil {
		t.Fatalf("a real link: %v", err)
	}

	ids := func(links []*githubLinkModel.GitHubLink) map[uuid.UUID]*githubLinkModel.GitHubLink {
		out := map[uuid.UUID]*githubLinkModel.GitHubLink{}
		for _, l := range links {
			if l != nil {
				out[l.Id] = l
			}
		}
		return out
	}
	unchanged := func(t *testing.T, where string, l *githubLinkModel.GitHubLink) {
		t.Helper()
		if l == nil {
			t.Errorf("%s: the real link is missing", where)
			return
		}
		if l.RepoOwner != "acme" || l.RepoName != "offline-site" || l.WebhookSecret == nil || *l.WebhookSecret != secret || l.SyncIssues || l.SyncPRs || l.AutoCreateTasks {
			t.Errorf("%s: the real link reads %s/%s secret kept %v sync %v/%v auto %v; want it as made", where, l.RepoOwner, l.RepoName,
				l.WebhookSecret != nil && *l.WebhookSecret == secret, l.SyncIssues, l.SyncPRs, l.AutoCreateTasks)
		}
	}

	t.Run("every live link leaves the fixture out and keeps the real one", func(t *testing.T) {
		all, err := githubLinkModel.GetAllGitHubLinks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(all); got[offline] != nil {
			t.Errorf("GetAllGitHubLinks gives the fixture link")
		} else {
			unchanged(t, "GetAllGitHubLinks", got[real])
		}
	})
	t.Run("a project's links leave the fixture out and keep the real one", func(t *testing.T) {
		links, err := githubLinkModel.GetGitHubLinksByProjectId(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(links); got[offline] != nil {
			t.Errorf("GetGitHubLinksByProjectId gives the fixture link")
		} else {
			unchanged(t, "GetGitHubLinksByProjectId", got[real])
		}
	})
	t.Run("a repository's links leave the fixture out and keep the real one", func(t *testing.T) {
		fixture, err := githubLinkModel.GetGitHubLinksByRepo(ctx, "demo-team", "q4-launch-site")
		if err != nil {
			t.Fatal(err)
		}
		if len(fixture) != 0 {
			t.Errorf("GetGitHubLinksByRepo gives %d links to the fixture repository; want none", len(fixture))
		}
		links, err := githubLinkModel.GetGitHubLinksByRepo(ctx, "acme", "offline-site")
		if err != nil {
			t.Fatal(err)
		}
		unchanged(t, "GetGitHubLinksByRepo", ids(links)[real])
	})
	t.Run("a live link by its id is never the fixture, and the real one reads as made", func(t *testing.T) {
		if l, err := githubLinkModel.GetGitHubLinkById(ctx, offline, true); err == nil && l != nil {
			t.Errorf("GetGitHubLinkById gives the fixture link")
		}
		l, err := githubLinkModel.GetGitHubLinkById(ctx, real, true)
		if err != nil {
			t.Fatal(err)
		}
		unchanged(t, "GetGitHubLinkById", l)
	})
	t.Run("a task's project links the real repository and never the fixture, asked for one task or for many", func(t *testing.T) {
		// One task's ask (LinkTaskToGitHub) and many tasks' (BulkLinkTasksToGitHub)
		// are the same question: an address in the fixture repository is
		// refused by both, one in the real repository taken by both.
		task := uuid.MustParse(w.task)
		if l, err := githubBusiness.TaskRepoLink(ctx, task, "demo-team", "q4-launch-site"); err != nil || l != nil {
			t.Errorf("TaskRepoLink(the fixture repository) gives a link %v (%v); want none", l != nil, err)
		}
		if l, err := githubBusiness.TaskRepoLink(ctx, task, "acme", "offline-site"); err != nil || l == nil || l.Id != real {
			t.Errorf("TaskRepoLink(the real repository) gives the real link %v (%v); want it", l != nil && l.Id == real, err)
		}
		fixture := githubBusiness.TaskRepo{Task: task, Owner: "demo-team", Name: "q4-launch-site"}
		made := githubBusiness.TaskRepo{Task: task, Owner: "acme", Name: "offline-site"}
		got, err := githubBusiness.TaskRepoLinks(ctx, []githubBusiness.TaskRepo{fixture, made})
		if err != nil {
			t.Fatal(err)
		}
		if got[fixture] || !got[made] {
			t.Errorf("TaskRepoLinks says the fixture repository is linked %v and the real one %v; want false and true", got[fixture], got[made])
		}
	})
	t.Run("a branch leads to a task only through a real link", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_branch = 'fix-unsubscribe' WHERE id = $1`, w.task)
		if got, err := taskDomain.FindTaskUUIDsByGitHubBranch(ctx, "demo-team", "q4-launch-site", "fix-unsubscribe"); err != nil || len(got) != 0 {
			t.Errorf("the fixture repository's branch leads to %v (%v); want no task", got, err)
		}
		if got, err := taskDomain.FindTaskUUIDsByGitHubBranch(ctx, "acme", "offline-site", "fix-unsubscribe"); err != nil || len(got) != 1 || got[0] != w.task {
			t.Errorf("the real repository's branch leads to %v (%v); want [%s]", got, err, w.task)
		}
	})
	t.Run("the start's repair keeps a pull request in the fixture repository and takes off one in a repository nothing links", func(t *testing.T) {
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/demo-team/q4-launch-site/pull/128', github_pr_number = 128 WHERE id = $1`, w.task)
		w.exec(`UPDATE tasks SET github_pr_url = 'https://github.com/demo-team/elsewhere/pull/7', github_pr_number = 7 WHERE id = $1`, w.other)
		if _, err := githubBusiness.RepairTaskLinks(ctx); err != nil {
			t.Fatal(err)
		}
		if _, pr := w.links(w.task); pr != "https://github.com/demo-team/q4-launch-site/pull/128" {
			t.Errorf("the repair took the fixture repository's pull request off its task: %q", pr)
		}
		if _, pr := w.links(w.other); pr != "" {
			t.Errorf("the repair kept %q, a pull request in a repository Website doesn't link", pr)
		}
	})
}

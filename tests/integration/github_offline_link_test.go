//go:build integration

// V-841: the demo's fixture link (business/DemoSeed CodeLinked writes it for
// Q4 launch) has nothing to reach GitHub with: no installation, no webhook
// secret, no sync. It is there only so the start's repair (RepairTaskLinks)
// keeps the seeded pull request on its task. Every read of live links leaves
// it out, so no list shows it and no reader calls GitHub for it. A link made
// in the app always has a secret (LinkRepositoryToProject), and reads as it
// did.
package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	syncBusiness "github.com/akashc777/OneCamp/business"
	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
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
	t.Run("the AI's code tools find no project linking the fixture repository, and the real one's project", func(t *testing.T) {
		// business/AI/codeExecutors.go projectsLinking: a repository no project
		// links is one read_repo_file and search never read on GitHub.
		got, err := githubBusiness.ProjectsLinking(ctx, "demo-team", "q4-launch-site")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("ProjectsLinking(demo-team/q4-launch-site) = %v; want none: the code tools would read the fixture repository on GitHub", got)
		}
		real, err := githubBusiness.ProjectsLinking(ctx, "acme", "offline-site")
		if err != nil {
			t.Fatal(err)
		}
		if len(real) != 1 || real[0] != project {
			t.Errorf("ProjectsLinking(acme/offline-site) = %v; want [%s]", real, project)
		}
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

// The demo's Q4-8 read "Sync failed" (CHAT76). Its close by the integration's
// bot (RecordLanded) queues its status for GitHub, and the check on what a
// sync may send read online links alone: it took the fixture repository for
// one Q4 launch never linked, marked the task's sync failed and refused. A
// repository the task's own project links offline has nothing to reach GitHub
// with: the sync sends nothing, reads nothing, is done, and leaves the task's
// sync state as it was, and so does a refresh. All else is as it was: a
// repository another project links, online or offline, or one no project
// links, is refused and the task's sync marked failed; the task's own
// project's online link syncs.
func TestASyncThroughItsOwnProjectsOfflineLinkSendsNothingAndIsDone(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	gh := recordGitHub(t)
	w := seedGitHubWorkspace(t, env, dg)
	// The seed links acme/web to Website and acme/billing to Billing, online.
	// The fixture is Website's offline link, as the seed writes it; Billing
	// has an offline link of its own.
	for project, repo := range map[string]string{w.project: "q4-launch-site", w.billing: "billing-site"} {
		if err := githubLinkModel.CreateGitHubLink(ctx, uuid.New(), uuid.MustParse(project), "demo-team", repo, nil, nil, false, false, false, w.people["ana"], time.Now()); err != nil {
			t.Fatalf("an offline link: %v", err)
		}
	}
	// Each a task of Website's, linked to an issue or a pull request in repo.
	cases := []struct {
		name, task, issue, pr, repo string
		want                        string // "done", "sent" or "refused"
	}{
		{"its own project's offline link (the fixture)", w.other, "", "https://github.com/demo-team/q4-launch-site/pull/128", "/repos/demo-team/q4-launch-site/", "done"},
		{"its own project's online link", w.task, "https://github.com/acme/web/issues/5", "", "/repos/acme/web/", "sent"},
		{"another project's online link", w.third, "https://github.com/acme/billing/issues/3", "", "/repos/acme/billing/", "refused"},
		{"another project's offline link", uuid.NewString(), "", "https://github.com/demo-team/billing-site/pull/4", "/repos/demo-team/billing-site/", "refused"},
		{"a repository no project links, beside its project's offline one", uuid.NewString(), "https://github.com/demo-team/elsewhere/issues/7", "", "/repos/demo-team/elsewhere/", "refused"},
	}
	for _, c := range cases[3:] {
		w.exec(`INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, c.task, w.project, w.people["ana"])
	}
	for _, c := range cases {
		w.exec(`UPDATE tasks SET github_issue_url = NULLIF($2, ''), github_pr_url = NULLIF($3, '') WHERE id = $1`, c.task, c.issue, c.pr)
	}
	syncState := func(task string) (status, reason string) {
		t.Helper()
		if err := env.PG.QueryRow(`SELECT COALESCE(github_sync_status, ''), COALESCE(github_sync_error, '') FROM tasks WHERE id = $1`, task).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		return status, reason
	}
	// item is the state of the task's sync item and how often it was tried.
	item := func(task string) (state string, tries int) {
		_ = env.PG.QueryRow(`SELECT status, attempts FROM github_sync_queue WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`, task).Scan(&state, &tries)
		return state, tries
	}
	// syncAsAsked is what the task's sync read the last time GitHub was asked
	// call ("METHOD /path"): the card says "Syncing…", and polls, only while
	// it reads pending. fail, when not nil, is how the call fails.
	syncAsAsked := func(task, call string, fail error) func() string {
		var mu sync.Mutex
		read := "nothing: GitHub wasn't asked " + call
		gh.atEachCall(func(asked string) error {
			if asked != call {
				return nil
			}
			var status string
			_ = env.PG.QueryRow(`SELECT COALESCE(github_sync_status, '') FROM tasks WHERE id = $1`, task).Scan(&status)
			mu.Lock()
			read = status
			mu.Unlock()
			return fail
		})
		return func() string {
			mu.Lock()
			defer mu.Unlock()
			return read
		}
	}

	t.Run("the sync job: the offline link's is done with nothing sent, the rest as before", func(t *testing.T) {
		workers, stopWorkers := context.WithCancel(ctx)
		defer stopWorkers()
		sending := syncAsAsked(cases[1].task, "PATCH "+cases[1].repo+"issues/5", nil)
		defer gh.atEachCall(nil)
		githubBusiness.StartGitHubSyncWorker(syncBusiness.SyncSignal, workers)
		// A closed task's status, as the demo's close queued Q4-8's.
		for _, c := range cases {
			syncBusiness.EnqueueGitHubSync(ctx, uuid.MustParse(c.task), "status", map[string]interface{}{"status": "done"})
		}
		// Every item tried once and none mid-try: a refusal is final, a send
		// or a skip done, and a retry waits for the worker's next pass.
		tried := func() bool {
			for _, c := range cases {
				if state, tries := item(c.task); tries == 0 || state == "processing" {
					return false
				}
			}
			return true
		}
		for deadline := time.Now().Add(60 * time.Second); !tried(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the sync didn't try every item: GitHub saw %v", gh.calls())
			}
		}
		stopWorkers()
		for _, c := range cases {
			state, tries := item(c.task)
			status, reason := syncState(c.task)
			switch c.want {
			case "done":
				if state != "completed" || tries != 1 || status != "synced" || reason != "" || gh.count(c.repo) != 0 {
					t.Errorf("%s: the item is %q after %d tries, the task's sync reads %q %q, GitHub saw %d calls there; want it done at once, nothing sent or read, the sync as it was",
						c.name, state, tries, status, reason, gh.count(c.repo))
				}
			case "sent":
				if state != "completed" || status != "synced" || gh.count("PATCH "+c.repo+"issues/5") != 1 {
					t.Errorf("%s: the item is %q, the task's sync reads %q %q, GitHub saw %v; want the status sent", c.name, state, status, reason, gh.calls())
				}
				if read := sending(); read != "pending" {
					t.Errorf("%s: as its status went to GitHub the task's sync read %s; want pending", c.name, read)
				}
			case "refused":
				if state != "failed" || status != "failed" || !strings.Contains(reason, "isn't linked to") || gh.count(c.repo) != 0 {
					t.Errorf("%s: the item is %q, the task's sync reads %q %q, GitHub saw %d calls there; want it refused as outside its project",
						c.name, state, status, reason, gh.count(c.repo))
				}
			}
		}
	})

	t.Run("a refresh through its own project's offline link reads nothing and is done, another project's is refused", func(t *testing.T) {
		done, refused := cases[0], cases[3]
		w.exec(`UPDATE tasks SET github_sync_status = 'synced', github_sync_error = NULL, github_sync_attempts = 0 WHERE id IN ($1, $2)`, done.task, refused.task)
		gh.reset()
		if err := githubBusiness.RefreshFromGitHub(ctx, uuid.MustParse(done.task), false, nil); err != nil {
			t.Errorf("%s: the refresh answered %v; want it done", done.name, err)
		}
		if status, reason := syncState(done.task); status != "synced" || reason != "" {
			t.Errorf("%s: after the refresh the task's sync reads %q %q; want it as it was", done.name, status, reason)
		}
		err := githubBusiness.RefreshFromGitHub(ctx, uuid.MustParse(refused.task), false, nil)
		if err == nil || !strings.Contains(err.Error(), "isn't linked to") {
			t.Errorf("%s: the refresh answered %v; want it refused as outside its project", refused.name, err)
		}
		if status, reason := syncState(refused.task); status != "failed" || !strings.Contains(reason, "isn't linked to") {
			t.Errorf("%s: after the refresh the task's sync reads %q %q; want it failed", refused.name, status, reason)
		}
		if calls := gh.calls(); len(calls) != 0 {
			t.Errorf("GitHub saw %v; want nothing sent or read", calls)
		}
	})

	t.Run("a refresh through its own project's online link reads pending while it reads GitHub", func(t *testing.T) {
		online := cases[1]
		ana, err := userDomain.GetDgraphUserInfoByUUID(ctx, w.people["ana"].String())
		if err != nil || ana == nil {
			t.Fatalf("Ana's node: %v", err)
		}
		w.exec(`UPDATE tasks SET github_sync_status = 'synced', github_sync_error = NULL, github_sync_attempts = 0 WHERE id = $1`, online.task)
		gh.reset()
		// GitHub's read fails, so nothing of an answer is applied: what is
		// read here is the task's sync while the read is made.
		reading := syncAsAsked(online.task, "GET "+online.repo+"issues/5", errors.New("GitHub can't be reached"))
		defer gh.atEachCall(nil)
		err = githubBusiness.RefreshFromGitHub(ctx, uuid.MustParse(online.task), false, ana)
		if read := reading(); read != "pending" {
			t.Errorf("%s: as the refresh read the issue from GitHub the task's sync read %s; want pending", online.name, read)
		}
		if err == nil || !strings.Contains(err.Error(), "GitHub can't be reached") {
			t.Errorf("%s: the refresh answered %v; want GitHub's failed read", online.name, err)
		}
		if status, reason := syncState(online.task); status != "failed" || !strings.Contains(reason, "GitHub can't be reached") {
			t.Errorf("%s: after the failed read the task's sync reads %q %q; want it failed, saying why", online.name, status, reason)
		}
	})
}

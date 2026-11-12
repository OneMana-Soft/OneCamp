package business

import (
	"context"
	"errors"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	"github.com/akashc777/OneCamp/helpers"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// A tag's commits put in each project's release only the tasks of that
// project its link may change and the bot may read: named in a commit's
// message, or by the branch a merge commit merged; never another project's,
// never a restricted one, each once.
func TestATagsCommitsNameEachProjectsReadableTasks(t *testing.T) {
	if helpers.MessageLogs == nil {
		discard := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: discard, ErrorLog: discard}
	}
	oldProject, oldLinks, oldKey, oldReads := readTaskProject, readProjectLinks, keyTask, botReads
	t.Cleanup(func() { readTaskProject, readProjectLinks, keyTask, botReads = oldProject, oldLinks, oldKey, oldReads })

	website, billing, elsewhere := uuid.New(), uuid.New(), uuid.New()
	one, two, private, bil, far := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	projects := map[uuid.UUID]uuid.UUID{one: website, two: website, private: website, bil: billing, far: elsewhere}
	keys := map[string]uuid.UUID{"WEB-1": one, "WEB-2": two, "WEB-3": private, "BIL-1": bil, "FAR-1": far}
	readTaskProject = func(_ context.Context, task uuid.UUID) (uuid.UUID, error) { return projects[task], nil }
	readProjectLinks = func(_ context.Context, project uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		repo := map[uuid.UUID]string{website: "shop", billing: "shop", elsewhere: "other"}[project]
		return []*githubLinkModel.GitHubLink{{ProjectId: project, RepoOwner: "acme", RepoName: repo, SyncPRs: true}}, nil
	}
	keyTask = func(_ context.Context, raw string) (uuid.UUID, error) {
		if id, ok := keys[raw]; ok {
			return id, nil
		}
		return uuid.Nil, taskKeys.ErrNoTask
	}
	botReads = func(_ context.Context, task uuid.UUID) bool { return task != private }

	got := tasksNamed(withEventReads(context.Background()), "acme", "shop", []string{
		"WEB-1 checkout totals",
		"Merge pull request #9 from acme/web-2-receipts\n\nReceipts by mail",
		"Fixes WEB-3", // restricted from the bot
		"BIL-1 invoices",
		"FAR-1 a project linking another repository",
		"Merge branch 'web-1-again' into main", // WEB-1 once
	})
	if w := got[website]; len(w) != 2 || w[0] != one || w[1] != two {
		t.Errorf("Website's tasks %v, want WEB-1 then WEB-2", w)
	}
	if b := got[billing]; len(b) != 1 || b[0] != bil {
		t.Errorf("Billing's tasks %v, want BIL-1", b)
	}
	if len(got) != 2 {
		t.Errorf("projects %v, want Website and Billing only", got)
	}
}

// The branch a merge commit merged, from GitHub's and git's own words.
func TestMergedBranch(t *testing.T) {
	for msg, want := range map[string]string{
		"Merge pull request #12 from acme/web-12-export\n\nExport": "web-12-export",
		"Merge pull request #3 from fork-owner/feature/WEB-4":      "feature/WEB-4",
		"Merge branch 'web-7-fix' into main":                       "web-7-fix",
		"Merge branch 'hotfix'":                                    "hotfix",
		"WEB-1 not a merge":                                        "",
		"Merge remote-tracking branch 'origin/main'":               "",
	} {
		if got := mergedBranch(msg); got != want {
			t.Errorf("mergedBranch(%q) = %q, want %q", msg, got, want)
		}
	}
}

// A deploy's status and a workflow run read as a release's stages: the
// environment or the workflow, where it stands, its commit and its tag.
func TestDeploysAndRunsReadAsStages(t *testing.T) {
	at := "2026-11-06T10:00:00Z"
	when, _ := time.Parse(time.RFC3339, at)
	ref, s, ok := deployStage([]byte(`{"deployment_status": {"state": "success", "environment": "production",
		"target_url": "https://shop.example", "log_url": "https://github.com/acme/shop/actions/runs/1", "updated_at": "` + at + `"},
		"deployment": {"sha": "abc123", "ref": "v1.1", "environment": "production"}}`))
	if !ok || ref != "v1.1" || s.Kind != "deploy" || s.Name != "production" || s.Status != "success" || s.SHA != "abc123" ||
		s.URL != "https://github.com/acme/shop/actions/runs/1" || s.Provider != "github" || !s.ReportedAt.Equal(when) {
		t.Errorf("deploy: %q %+v %v", ref, s, ok)
	}
	ref, s, ok = runStage([]byte(`{"action": "completed", "workflow_run": {"name": "CI", "status": "completed",
		"conclusion": "failure", "head_sha": "abc123", "head_branch": "v1.1", "html_url": "https://github.com/acme/shop/actions/runs/2",
		"updated_at": "` + at + `"}}`))
	if !ok || ref != "v1.1" || s.Kind != "build" || s.Name != "CI" || s.Status != "failure" || s.SHA != "abc123" || !s.ReportedAt.Equal(when) {
		t.Errorf("run: %q %+v %v", ref, s, ok)
	}
	if _, s, ok = runStage([]byte(`{"action": "in_progress", "workflow_run": {"name": "CI", "status": "in_progress", "head_sha": "abc123"}}`)); !ok || s.Status != "in_progress" {
		t.Errorf("a running run: %+v %v", s, ok)
	}
	for _, body := range []string{`{"workflow_run": {"status": "completed", "head_sha": "abc"}}`, `{"workflow_run": {"name": "CI"}}`, `not json`} {
		if _, s, ok := runStage([]byte(body)); ok {
			t.Errorf("%s read as %+v", body, s)
		}
	}
	if _, s, ok := deployStage([]byte(`{"deployment_status": {"state": "success"}, "deployment": {"sha": "abc"}}`)); ok {
		t.Errorf("a deploy to no environment read as %+v", s)
	}
}

// A tag or a stage reaches the projects whose link to the repository syncs
// pull requests and that aren't archived (an archived project's releases
// change no more), each once, through one read of which are live. A failed
// read is the delivery's error, so it is tried again rather than written.
func TestOnlyLiveSyncingProjectsTakeATag(t *testing.T) {
	live, archived, quiet := uuid.New(), uuid.New(), uuid.New()
	githubLinkCache.Purge()
	t.Cleanup(githubLinkCache.Purge)
	githubLinkCache.Set(linkCacheKey("acme", "shop"), githubLinkCacheEntry{generation: atomic.LoadUint64(&githubLinkGeneration),
		links: []*githubLinkModel.GitHubLink{{ProjectId: live, SyncPRs: true}, {ProjectId: archived, SyncPRs: true},
			{ProjectId: quiet}, {ProjectId: live, SyncPRs: true}}})
	was := liveProjects
	t.Cleanup(func() { liveProjects = was })
	reads := 0
	liveProjects = func(_ context.Context, ids []string) (map[string]bool, error) {
		reads++
		return map[string]bool{live.String(): true}, nil
	}
	got, err := syncingProjects(context.Background(), "acme", "shop")
	if err != nil || len(got) != 1 || got[0] != live || reads != 1 {
		t.Errorf("syncing %v (%v) in %d reads, want only %s in one", got, err, reads, live)
	}
	liveProjects = func(context.Context, []string) (map[string]bool, error) { return nil, errors.New("graph down") }
	if got, err := syncingProjects(context.Background(), "acme", "shop"); err == nil {
		t.Errorf("a failed read answered %v, want its error", got)
	}
}

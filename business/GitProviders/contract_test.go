package business

import (
	"context"
	"errors"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// GitHub is today's functions behind the interface; a branch isn't made here
// yet.
func TestGitHubIsTodaysFunctions(t *testing.T) {
	oldLinked, oldPR, oldRefresh := linkedRepos, openPR, refresh
	t.Cleanup(func() { linkedRepos, openPR, refresh = oldLinked, oldPR, oldRefresh })
	project, task := uuid.New(), uuid.New()
	var refreshed []bool
	linkedRepos = func(_ context.Context, id uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		if id != project {
			t.Errorf("asked about %s", id)
		}
		return []*githubLinkModel.GitHubLink{{RepoOwner: "acme", RepoName: "web"}, nil}, nil
	}
	openPR = func(_ context.Context, id uuid.UUID, title, body, head string) (*struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}, error) {
		return &struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}{Number: 42, HTMLURL: "https://github.com/acme/web/pull/42"}, nil
	}
	var refresher *dgraphStruct.DgraphUser
	refresh = func(_ context.Context, id uuid.UUID, backfill bool, by *dgraphStruct.DgraphUser) error {
		refreshed, refresher = append(refreshed, backfill), by
		return nil
	}
	gh := Providers[GitHub]
	repos, err := gh.LinkedRepos(context.Background(), project)
	if err != nil || len(repos) != 1 || repos[0] != (Repo{Owner: "acme", Name: "web"}) {
		t.Errorf("repos %v %v", repos, err)
	}
	pr, err := gh.OpenPullRequest(context.Background(), task, "Fix it", "", "fix-it")
	if err != nil || pr.Number != 42 || pr.URL == "" {
		t.Errorf("pr %+v %v", pr, err)
	}
	ana := &dgraphStruct.DgraphUser{Uid: "0xana"}
	if err := gh.Refresh(context.Background(), task, ana); err != nil || len(refreshed) != 1 || refreshed[0] || refresher != ana {
		t.Errorf("refresh %v %v by %v: once, without the older comments, as whoever refreshed", refreshed, err, refresher)
	}
	if err := gh.MakeBranch(context.Background(), task, "fix-it"); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("branch: %v", err)
	}
}

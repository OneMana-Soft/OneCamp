// Package business (GitProviders) is Git Providers
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #97; B10 builds it): GitHub
// and GitLab behind one interface — a project's repositories, branches, pull
// and merge requests, commits, reviews, checks and deploys — with task keys
// resolved only within the repository's own projects.
//
// DAY 0. GitHub is today's business/GitHub behind the interface: a project's
// linked repositories, a pull request opened for a task, and a task refreshed
// from its issue. A branch is still made in the task route itself
// (controllers/Task CreateGitHubBranchForTask), so MakeBranch answers
// ErrNotBuilt until B10 moves it here. Nothing calls Providers yet.
package business

import (
	"context"
	"errors"

	githubBusiness "github.com/akashc777/OneCamp/business/GitHub"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Repo is a repository a project is linked to.
type Repo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// PullRequest is one opened for a task.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Provider is one place code lives.
type Provider interface {
	// LinkedRepos is the repositories the project is linked to, newest first.
	LinkedRepos(ctx context.Context, projectID uuid.UUID) ([]Repo, error)
	// OpenPullRequest opens a pull request for the task from its branch.
	OpenPullRequest(ctx context.Context, taskID uuid.UUID, title, body, head string) (*PullRequest, error)
	// MakeBranch makes a branch for the task in its project's repository.
	MakeBranch(ctx context.Context, taskID uuid.UUID, name string) error
	// Refresh brings the task up to date with its issue, without the
	// issue's older comments (as the refresh route does unless asked), as by:
	// what it changes it changes as them.
	Refresh(ctx context.Context, taskID uuid.UUID, by *dgraphStruct.DgraphUser) error
}

// ErrNotBuilt is something a provider doesn't do yet.
var ErrNotBuilt = errors.New("git: that isn't built yet")

// GitHub is the name of today's provider.
const GitHub = "github"

// Providers are the providers by name.
var Providers = map[string]Provider{GitHub: github{}}

// Seams for tests: today's GitHub functions.
var (
	linkedRepos = githubBusiness.GetLinkedRepos
	openPR      = githubBusiness.CreatePullRequestForTask
	refresh     = githubBusiness.RefreshFromGitHub
)

type github struct{}

func (github) LinkedRepos(ctx context.Context, projectID uuid.UUID) ([]Repo, error) {
	links, err := linkedRepos(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]Repo, 0, len(links))
	for _, l := range links {
		if l != nil {
			out = append(out, Repo{Owner: l.RepoOwner, Name: l.RepoName})
		}
	}
	return out, nil
}

func (github) OpenPullRequest(ctx context.Context, taskID uuid.UUID, title, body, head string) (*PullRequest, error) {
	pr, err := openPR(ctx, taskID, title, body, head)
	if err != nil || pr == nil {
		return nil, err
	}
	return &PullRequest{Number: pr.Number, URL: pr.HTMLURL}, nil
}

func (github) MakeBranch(context.Context, uuid.UUID, string) error { return ErrNotBuilt }

func (github) Refresh(ctx context.Context, taskID uuid.UUID, by *dgraphStruct.DgraphUser) error {
	return refresh(ctx, taskID, false, by)
}

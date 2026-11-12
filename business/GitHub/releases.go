package business

// Releases from GitHub (P10-1b job 2, jira-04 + jira-09). Each project whose
// link to a repository syncs pull requests (eventTask's rule), while it isn't
// archived, takes, as the integration's bot:
//
//   - a tag (its create, or a published GitHub release; one shipTag): its
//     release made or shipped (business/Release ShipTag), and the tasks the
//     tag's commits since the repository's previous tag in that project name,
//     read as the bot (tasksNamed, through codeTask) before the ship, put in
//     it as git in the ship's own transaction;
//   - a deploy's status and a workflow run: a stage on the releases of the
//     commit or tag they ran on. A run on a commit no release was tagged at
//     reports nothing (named gap: deploys of untagged commits).
//
// One compare call per tag and previous tag: GitHub answers at most 250
// commits, so a tag further than that from the previous one names only the
// tasks of the first 250 (logged). A project's first
// tag from a repository has nothing to compare with and takes no tasks.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	releaseBusiness "github.com/akashc777/OneCamp/business/Release"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// liveProjects is a seam for tests: which projects aren't archived (an
// archive is the project's deletion time, as workAccess's Archived reads it),
// in one read of the graph however many.
var liveProjects = projectDomain.LiveProjects

var (
	mergedPRLine     = regexp.MustCompile(`^Merge pull request #\d+ from [^/\s]+/(\S+)`)
	mergedBranchLine = regexp.MustCompile(`^Merge branch '([^']+)'`)
)

// mergedBranch is the branch a merge commit's message says it merged; "" for
// a message that isn't a merge's. Pure.
func mergedBranch(message string) string {
	first, _, _ := strings.Cut(message, "\n")
	if m := mergedPRLine.FindStringSubmatch(first); m != nil {
		return m[1]
	}
	if m := mergedBranchLine.FindStringSubmatch(first); m != nil {
		return m[1]
	}
	return ""
}

// tasksNamed is, for each project, the tasks texts (commit messages) on
// owner/name name that an event there may change (codeTask: that project's
// own link syncing pull requests, and the bot may read them), by a key or
// address in the text or the branch a merge commit merged, each once, in the
// order first named.
func tasksNamed(ctx context.Context, owner, name string, texts []string) map[uuid.UUID][]uuid.UUID {
	out := map[uuid.UUID][]uuid.UUID{}
	named := map[uuid.UUID]bool{}
	for _, text := range texts {
		refs := taskKeys.RefsIn(text)
		if branch := mergedBranch(text); branch != "" {
			refs = append(refs, taskKeys.BranchRefs(branch)...)
		}
		for _, ref := range refs {
			id, link, ok := codeTask(ctx, ref, owner, name)
			if !ok || named[id] {
				continue
			}
			named[id] = true
			out[link.ProjectId] = append(out[link.ProjectId], id)
		}
	}
	return out
}

// syncingProjects is the projects whose links to owner/name sync pull
// requests and that aren't archived (an archived project's releases change
// no more), each once, through one read of which are live; none for a
// repository no project syncs.
func syncingProjects(ctx context.Context, owner, name string) ([]uuid.UUID, error) {
	links, err := repositoryLinks(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the links to %s/%s: %w", owner, name, err)
	}
	var linked []uuid.UUID
	var ids []string
	seen := map[uuid.UUID]bool{}
	for _, l := range links {
		if l != nil && syncsPRs(l) && !seen[l.ProjectId] {
			seen[l.ProjectId] = true
			linked = append(linked, l.ProjectId)
			ids = append(ids, l.ProjectId.String())
		}
	}
	if len(linked) == 0 {
		return nil, nil
	}
	live, err := liveProjects(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("couldn't read which projects linking %s/%s are archived: %w", owner, name, err)
	}
	var out []uuid.UUID
	for _, p := range linked {
		if live[p.String()] {
			out = append(out, p)
		}
	}
	return out, nil
}

// botID is the integration's bot's user id, as releases record who did it.
func botID(ctx context.Context) (uuid.UUID, error) {
	bot, err := integrationBot(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("couldn't resolve the integration's bot: %w", err)
	}
	return uuid.Parse(bot.Uuid)
}

// tagCommit is the commit tag points at, through an annotated tag's object.
func tagCommit(ctx context.Context, owner, name, tag string) (string, error) {
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct{ SHA, Type string } `json:"object"`
	}
	address := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/tags/%s", url.PathEscape(owner), url.PathEscape(name), url.PathEscape(tag))
	for hop := 0; hop < 3; hop++ {
		body, err := githubGet(ctx, client, address)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(body, &ref); err != nil {
			return "", fmt.Errorf("couldn't read tag %s of %s/%s: %w", tag, owner, name, err)
		}
		if ref.Object.Type != "tag" {
			return ref.Object.SHA, nil
		}
		address = fmt.Sprintf("https://api.github.com/repos/%s/%s/git/tags/%s", url.PathEscape(owner), url.PathEscape(name), url.PathEscape(ref.Object.SHA))
	}
	return "", fmt.Errorf("tag %s of %s/%s points at tags too deep", tag, owner, name)
}

// commitsBetween is the messages of the commits on owner/name after base up
// to head, in one compare call: GitHub answers the first 250, and a compare
// past that is logged.
func commitsBetween(ctx context.Context, owner, name, base, head string) ([]string, error) {
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	body, err := githubGet(ctx, client, fmt.Sprintf("https://api.github.com/repos/%s/%s/compare/%s...%s",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(base), url.PathEscape(head)))
	if err != nil {
		return nil, err
	}
	var cmp struct {
		Total   int `json:"total_commits"`
		Commits []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		return nil, fmt.Errorf("couldn't read the compare of %s...%s on %s/%s: %w", base, head, owner, name, err)
	}
	if cmp.Total > len(cmp.Commits) {
		helpers.MessageLogs.InfoLog.Printf("github: %s/%s %s...%s has %d commits; the release takes the tasks of the first %d",
			owner, name, base, head, cmp.Total, len(cmp.Commits))
	}
	out := make([]string, 0, len(cmp.Commits))
	for _, c := range cmp.Commits {
		out = append(out, c.Commit.Message)
	}
	return out, nil
}

// shipTag ships tag of owner/name in each project syncing the repository,
// with the tasks its commits since that project's previous tag name
// (releaseBusiness.ShipTag): one compare for the projects that share a
// previous tag.
func shipTag(ctx context.Context, owner, name, tag string) error {
	projects, err := syncingProjects(ctx, owner, name)
	if err != nil || len(projects) == 0 || strings.TrimSpace(tag) == "" {
		return err
	}
	bot, err := botID(ctx)
	if err != nil {
		return err
	}
	sha, err := tagCommit(ctx, owner, name, tag)
	if err != nil {
		return fmt.Errorf("couldn't read the commit of tag %s on %s/%s: %w", tag, owner, name, err)
	}
	return releaseBusiness.ShipTag(ctx, projects, owner+"/"+name, tag, sha, func(ctx context.Context, since string) (map[uuid.UUID][]uuid.UUID, error) {
		messages, err := commitsBetween(ctx, owner, name, since, tag)
		if err != nil {
			return nil, err
		}
		return tasksNamed(ctx, owner, name, messages), nil
	}, bot)
}

// handleReleaseEvent ships a published GitHub release's tag.
func handleReleaseEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action  string `json:"action"`
		Release struct {
			TagName string `json:"tag_name"`
			Draft   bool   `json:"draft"`
		} `json:"release"`
		Repository repositoryNamed `json:"repository"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse release event: %v", err)
	}
	if event.Action != "published" || event.Release.Draft {
		return nil
	}
	return shipTag(ctx, event.Repository.Owner.Login, event.Repository.Name, event.Release.TagName)
}

// repositoryNamed is the repository a webhook's event names.
type repositoryNamed struct {
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Name string `json:"name"`
}

// parsedTime is an event's RFC 3339 time; zero for none. Pure.
func parsedTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// deployStage is a deployment_status event as a stage, with the ref it
// deployed; ok is false for one that names no environment or commit. Pure.
func deployStage(body []byte) (string, releaseModel.Stage, bool) {
	var event struct {
		Status struct {
			State       string `json:"state"`
			Environment string `json:"environment"`
			TargetURL   string `json:"target_url"`
			LogURL      string `json:"log_url"`
			UpdatedAt   string `json:"updated_at"`
		} `json:"deployment_status"`
		Deployment struct {
			SHA         string `json:"sha"`
			Ref         string `json:"ref"`
			Environment string `json:"environment"`
		} `json:"deployment"`
	}
	if json.Unmarshal(body, &event) != nil {
		return "", releaseModel.Stage{}, false
	}
	env := event.Status.Environment
	if env == "" {
		env = event.Deployment.Environment
	}
	link := event.Status.LogURL
	if link == "" {
		link = event.Status.TargetURL
	}
	s := releaseModel.Stage{Provider: provider, Kind: "deploy", Name: env, Status: event.Status.State, SHA: event.Deployment.SHA,
		URL: link, ReportedAt: parsedTime(event.Status.UpdatedAt)}
	return event.Deployment.Ref, s, env != "" && s.Status != "" && s.SHA != ""
}

// runStage is a workflow_run event as a stage, with the branch or tag it ran
// on: its conclusion once completed, where it stands before. Pure.
func runStage(body []byte) (string, releaseModel.Stage, bool) {
	var event struct {
		Run struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HeadSHA    string `json:"head_sha"`
			HeadBranch string `json:"head_branch"`
			HTMLURL    string `json:"html_url"`
			UpdatedAt  string `json:"updated_at"`
		} `json:"workflow_run"`
	}
	if json.Unmarshal(body, &event) != nil {
		return "", releaseModel.Stage{}, false
	}
	status := event.Run.Status
	if status == "completed" {
		status = event.Run.Conclusion
	}
	s := releaseModel.Stage{Provider: provider, Kind: "build", Name: event.Run.Name, Status: status, SHA: event.Run.HeadSHA,
		URL: event.Run.HTMLURL, ReportedAt: parsedTime(event.Run.UpdatedAt)}
	return event.Run.HeadBranch, s, s.Name != "" && s.Status != "" && s.SHA != ""
}

// reportStage keeps a deploy's or a run's stage (read by read) on the
// releases of the projects syncing the repository.
func reportStage(ctx context.Context, body []byte, read func([]byte) (string, releaseModel.Stage, bool)) error {
	var event struct {
		Repository repositoryNamed `json:"repository"`
	}
	ref, s, ok := read(body)
	if !ok || json.Unmarshal(body, &event) != nil {
		return nil
	}
	owner, name := event.Repository.Owner.Login, event.Repository.Name
	projects, err := syncingProjects(ctx, owner, name)
	if err != nil || len(projects) == 0 {
		return err
	}
	bot, err := botID(ctx)
	if err != nil {
		return err
	}
	s.Repo = owner + "/" + name
	return releaseBusiness.ReportStage(ctx, projects, ref, s, bot.String())
}

package business

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importJobModel "github.com/akashc777/OneCamp/models/postgres/GitHubImportJob"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Why this file exists
// --------------------
// The original ImportIssuesAsTasks / ImportPRsAsTasks functions ran
// inline in the HTTP handler. Each item triggered ~5 DB roundtrips
// (idempotency check + CreateTask in PG + Dgraph + activity log +
// GitHub fields update) and the total wall time scaled linearly with
// repo size. Repos with more than ~200 open issues/PRs would either:
//   - time out in the reverse proxy (default 60s)
//   - cause the controller's request context to cancel mid-import
//
// This file replaces that flow with:
//   1. A persistent github_import_jobs row created at request time.
//      The HTTP handler returns the job id immediately.
//   2. A background worker that drains pending rows in FIFO order,
//      claiming with FOR UPDATE SKIP LOCKED so multi-replica is safe.
//   3. A *batched* idempotency check: instead of one
//      FindTaskUUIDByGitHubIssueURL per issue, every page of fetched
//      issues is checked in a single ANY-array query.
//   4. A *bounded-parallelism* item creation loop: errgroup-style
//      worker pool with a small concurrency (4) so we don't blast
//      Dgraph with 100 simultaneous mutations while still being much
//      faster than serial.
//   5. Periodic progress writes so the admin UI can render a live
//      counter via MQTT.

const (
	importPagesMax        = 100 // upper bound on /issues + /pulls pagination
	importPageSize        = 100 // GitHub maximum
	importItemConcurrency = 4   // concurrent task creations per page
	importWorkerPoll      = 30 * time.Second
	importStaleAfter      = 15 * time.Minute
	importRetention       = 30 * 24 * time.Hour
)

// importSignal wakes the worker the moment a new job is enqueued so
// users don't wait for the next periodic poll. Buffered (1) so we
// never block the handler.
var importSignal = make(chan struct{}, 1)

// signalImportWorker is non-blocking. A missed signal just means the
// worker handles the row on the next poll, which is fine.
func signalImportWorker() {
	select {
	case importSignal <- struct{}{}:
	default:
	}
}

// EnqueueImportIssues records an issues-import job and returns the
// row id. The actual work runs in the background worker.
func EnqueueImportIssues(ctx context.Context, linkId uuid.UUID, triggeredBy uuid.UUID) (*importJobModel.Job, error) {
	if err := validateImportLink(ctx, linkId); err != nil {
		return nil, err
	}
	job, err := importJobModel.Create(ctx, linkId, importJobModel.KindIssues, &triggeredBy)
	if err != nil {
		return nil, err
	}
	signalImportWorker()
	return job, nil
}

// EnqueueImportPRs is the PR twin of EnqueueImportIssues.
func EnqueueImportPRs(ctx context.Context, linkId uuid.UUID, triggeredBy uuid.UUID) (*importJobModel.Job, error) {
	if err := validateImportLink(ctx, linkId); err != nil {
		return nil, err
	}
	job, err := importJobModel.Create(ctx, linkId, importJobModel.KindPRs, &triggeredBy)
	if err != nil {
		return nil, err
	}
	signalImportWorker()
	return job, nil
}

// validateImportLink resolves the link and verifies it still exists.
// We do this at enqueue time so an obviously-bad request fails fast
// instead of being deferred to the worker (and surfacing as a vague
// "import failed" later).
func validateImportLink(ctx context.Context, linkId uuid.UUID) error {
	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, linkId)
	if err != nil {
		return fmt.Errorf("link lookup failed: %w", err)
	}
	if link == nil {
		return fmt.Errorf("link not found")
	}
	return nil
}

// GetImportJob looks up a single job for the admin-panel polling
// endpoint.
func GetImportJob(ctx context.Context, id uuid.UUID) (*importJobModel.Job, error) {
	return importJobModel.GetById(ctx, id)
}

// ListImportJobs returns the most recent jobs for a link.
func ListImportJobs(ctx context.Context, linkId uuid.UUID, limit int) ([]*importJobModel.Job, error) {
	return importJobModel.ListByLink(ctx, linkId, limit)
}

// StartGitHubImportWorker runs the background worker goroutine.
// Call once at application startup with a shutdown context.
//
// Behaviour mirrors StartGitHubSyncWorker: wake on signal, fall back
// to a periodic poll, recover from panics, exit cleanly on shutdown.
func StartGitHubImportWorker(shutdownCtx context.Context) {
	go importWorkerLoop(shutdownCtx)
	go importCleanupLoop(shutdownCtx)
	helpers.MessageLogs.InfoLog.Println("GitHub import worker started")
}

func importWorkerLoop(shutdownCtx context.Context) {
	// Brief startup pause so other initializers settle.
	time.Sleep(8 * time.Second)

	ticker := time.NewTicker(importWorkerPoll)
	defer ticker.Stop()

	for {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					helpers.LogErrorWithContext(context.Background(),
						"github/importWorkerLoop panic recovered: %v", rec)
				}
			}()
			reapStaleImports()
			drainImportQueue(shutdownCtx)
		}()

		select {
		case <-shutdownCtx.Done():
			helpers.MessageLogs.InfoLog.Println("GitHub import worker shutting down")
			return
		case <-importSignal:
			// new job enqueued
		case <-ticker.C:
			// periodic check
		}
	}
}

func importCleanupLoop(shutdownCtx context.Context) {
	time.Sleep(2 * time.Minute)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if n, err := importJobModel.CleanupOld(ctx, importRetention); err == nil && n > 0 {
			helpers.MessageLogs.InfoLog.Printf("GitHub import cleanup: removed %d old job rows", n)
		}
		cancel()
		select {
		case <-shutdownCtx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

func reapStaleImports() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n, err := importJobModel.ReapStale(ctx, importStaleAfter); err == nil && n > 0 {
		helpers.MessageLogs.InfoLog.Printf("GitHub import worker reaped %d stale rows", n)
	}
}

// drainImportQueue claims and runs as many pending jobs as it can
// before yielding back to the outer loop. Each job runs to completion
// before the next is claimed; we don't parallelise across jobs
// because the per-item parallelism inside processImportJob is already
// the right unit of concurrency.
func drainImportQueue(shutdownCtx context.Context) {
	for {
		select {
		case <-shutdownCtx.Done():
			return
		default:
		}

		// Each iteration uses a fresh context with the worker
		// shutdown deadline as a parent so the pipeline can be
		// interrupted cleanly.
		claimCtx, cancel := context.WithTimeout(shutdownCtx, 30*time.Second)
		job, err := importJobModel.ClaimNext(claimCtx)
		cancel()
		if err != nil {
			helpers.LogErrorWithContext(shutdownCtx,
				"github/drainImportQueue Failed to claim next: %v", err)
			return
		}
		if job == nil {
			return
		}
		processImportJob(shutdownCtx, job)
	}
}

// processImportJob runs the actual import for a claimed job.
// Pre-fetches the link + user info once, then walks GitHub pages
// applying batch idempotency checks and bounded-concurrency creation.
func processImportJob(shutdownCtx context.Context, job *importJobModel.Job) {
	defer func() {
		if rec := recover(); rec != nil {
			helpers.LogErrorWithContext(context.Background(),
				"github/processImportJob panic recovered for job=%s: %v", job.Id.String(), rec)
			_ = importJobModel.Finalize(context.Background(), job.Id, false,
				job.ItemsTotal, job.ItemsImported, job.ItemsSkipped, job.ItemsFailed,
				fmt.Sprintf("worker panic: %v", rec))
		}
	}()

	// Use a long-lived but cancellable context so a slow GitHub repo
	// can't block forever. 30 minutes is generous enough for ~5000
	// items at our concurrency setting; a real shutdown still cancels
	// via shutdownCtx.
	ctx, cancel := context.WithTimeout(shutdownCtx, 30*time.Minute)
	defer cancel()
	// Every task below is made from an issue, with the title and body anyone
	// could write on GitHub, so the import is marked as the webhook marks its
	// work: what it writes is not sent back, and the events it sets off are
	// asked for by nobody identified (eventAsker).
	ctx = helpers.WithGitHubOrigin(ctx)

	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, job.LinkId)
	if err != nil || link == nil {
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "link not found")
		return
	}

	if job.TriggeredBy == nil {
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "missing triggered_by")
		return
	}
	userId := *job.TriggeredBy

	userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userId.String())
	if err != nil || userDgraphInfo == nil {
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "user dgraph info missing")
		return
	}
	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: userId},
		UserDgraphInfo:   *userDgraphInfo,
	}
	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, link.ProjectId.String(), userDgraphInfo.Uid)
	if err != nil || dgraphProjectInfo == nil {
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "project dgraph info missing")
		return
	}

	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "github not connected")
		return
	}

	switch job.ImportKind {
	case importJobModel.KindIssues:
		runIssueImport(ctx, job, link, client, userInfo, dgraphProjectInfo)
	case importJobModel.KindPRs:
		runPRImport(ctx, job, link, client, userInfo, dgraphProjectInfo)
	default:
		_ = importJobModel.Finalize(ctx, job.Id, false, 0, 0, 0, 0, "unknown import kind: "+job.ImportKind)
	}
}

// importCounters track progress safely across goroutines.
type importCounters struct {
	total    atomic.Int64
	imported atomic.Int64
	skipped  atomic.Int64
	failed   atomic.Int64
}

func (c *importCounters) snapshot() (total, imported, skipped, failed int) {
	return int(c.total.Load()), int(c.imported.Load()), int(c.skipped.Load()), int(c.failed.Load())
}

// flushProgress writes counter snapshots back to the DB. Cheap to
// call; admin UI polling reads these values directly.
func (c *importCounters) flushProgress(ctx context.Context, jobId uuid.UUID) {
	total, imported, skipped, failed := c.snapshot()
	if err := importJobModel.UpdateProgress(ctx, jobId, total, imported, skipped, failed); err != nil {
		helpers.LogErrorWithContext(ctx, "github/import flushProgress failed for job=%s: %v", jobId.String(), err)
	}
}

// runIssueImport drives the issue-import pipeline.
func runIssueImport(ctx context.Context, job *importJobModel.Job, link *githubLinkModel.GitHubLink, client *http.Client, userInfo *userModels.UserInfo, dgraphProjectInfo *dgraphStruct.DgraphProject) {
	c := &importCounters{}

	page := 1
	finalErr := ""
	for page <= importPagesMax {
		select {
		case <-ctx.Done():
			finalErr = "context cancelled"
			break
		default:
		}

		issues, hasMore, err := fetchIssuesPage(ctx, client, link, page)
		if err != nil {
			finalErr = err.Error()
			break
		}
		if len(issues) == 0 {
			break
		}

		// Batch idempotency: one DB roundtrip per page instead of
		// one per issue.
		urls := make([]string, 0, len(issues))
		for _, issue := range issues {
			if issue.PullRequest != nil {
				continue
			}
			urls = append(urls, issue.HTMLURL)
		}
		existing, _ := taskDomain.FindTasksByGitHubIssueURLs(ctx, urls)

		runItemPool(ctx, len(issues), func(idx int) {
			issue := issues[idx]
			if issue.PullRequest != nil {
				// /issues includes PRs; we only want true issues.
				return
			}
			c.total.Add(1)
			if _, ok := existing[issue.HTMLURL]; ok {
				c.skipped.Add(1)
				return
			}
			if err := createTaskFromIssue(ctx, link, userInfo, dgraphProjectInfo, &issue); err != nil {
				helpers.LogErrorWithContext(ctx,
					"github/runIssueImport Failed for issue #%d: %v", issue.Number, err)
				c.failed.Add(1)
				return
			}
			c.imported.Add(1)
		})

		c.flushProgress(ctx, job.Id)
		if !hasMore {
			break
		}
		page++
	}

	total, imported, skipped, failed := c.snapshot()
	success := finalErr == ""
	_ = importJobModel.Finalize(ctx, job.Id, success, total, imported, skipped, failed, finalErr)
	helpers.MessageLogs.InfoLog.Printf(
		"GitHub issue import job=%s repo=%s/%s total=%d imported=%d skipped=%d failed=%d success=%v",
		job.Id.String(), link.RepoOwner, link.RepoName, total, imported, skipped, failed, success)
}

// runPRImport drives the PR-import pipeline. Same shape as the issue
// path with different fetcher + creator.
func runPRImport(ctx context.Context, job *importJobModel.Job, link *githubLinkModel.GitHubLink, client *http.Client, userInfo *userModels.UserInfo, dgraphProjectInfo *dgraphStruct.DgraphProject) {
	c := &importCounters{}

	page := 1
	finalErr := ""
	for page <= importPagesMax {
		select {
		case <-ctx.Done():
			finalErr = "context cancelled"
			break
		default:
		}

		prs, hasMore, err := fetchPRsPage(ctx, client, link, page)
		if err != nil {
			finalErr = err.Error()
			break
		}
		if len(prs) == 0 {
			break
		}

		urls := make([]string, 0, len(prs))
		for _, pr := range prs {
			urls = append(urls, pr.HTMLURL)
		}
		existing, _ := taskDomain.FindTasksByGitHubPRURLs(ctx, urls)

		runItemPool(ctx, len(prs), func(idx int) {
			pr := prs[idx]
			c.total.Add(1)
			if _, ok := existing[pr.HTMLURL]; ok {
				c.skipped.Add(1)
				return
			}
			if err := createTaskFromPR(ctx, link, userInfo, dgraphProjectInfo, &pr); err != nil {
				helpers.LogErrorWithContext(ctx,
					"github/runPRImport Failed for PR #%d: %v", pr.Number, err)
				c.failed.Add(1)
				return
			}
			c.imported.Add(1)
		})

		c.flushProgress(ctx, job.Id)
		if !hasMore {
			break
		}
		page++
	}

	total, imported, skipped, failed := c.snapshot()
	success := finalErr == ""
	_ = importJobModel.Finalize(ctx, job.Id, success, total, imported, skipped, failed, finalErr)
	helpers.MessageLogs.InfoLog.Printf(
		"GitHub PR import job=%s repo=%s/%s total=%d imported=%d skipped=%d failed=%d success=%v",
		job.Id.String(), link.RepoOwner, link.RepoName, total, imported, skipped, failed, success)
}

// runItemPool runs `count` tasks via `fn(i)` with bounded
// concurrency. We use a small pool (4) so we don't overwhelm Dgraph
// with concurrent mutations on the same project — the bottleneck is
// the Dgraph mutation, not the GitHub API.
func runItemPool(ctx context.Context, count int, fn func(idx int)) {
	if count <= 0 {
		return
	}
	sem := make(chan struct{}, importItemConcurrency)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if rec := recover(); rec != nil {
					helpers.LogErrorWithContext(ctx,
						"github/runItemPool panic recovered: %v", rec)
				}
			}()
			fn(idx)
		}(i)
	}
	wg.Wait()
}

// importedIssue is the subset of GitHub /issues we care about.
type importedIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PullRequest *struct{} `json:"pull_request"`
}

// importedPR is the subset of GitHub /pulls we care about.
type importedPR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func fetchIssuesPage(ctx context.Context, client *http.Client, link *githubLinkModel.GitHubLink, page int) ([]importedIssue, bool, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open&per_page=%d&page=%d",
		link.RepoOwner, link.RepoName, importPageSize, page)
	body, err := githubGet(ctx, client, url)
	if err != nil {
		return nil, false, err
	}
	var issues []importedIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return nil, false, fmt.Errorf("failed to parse issues: %w", err)
	}
	return issues, len(issues) == importPageSize, nil
}

func fetchPRsPage(ctx context.Context, client *http.Client, link *githubLinkModel.GitHubLink, page int) ([]importedPR, bool, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=%d&page=%d",
		link.RepoOwner, link.RepoName, importPageSize, page)
	body, err := githubGet(ctx, client, url)
	if err != nil {
		return nil, false, err
	}
	var prs []importedPR
	if err := json.Unmarshal(body, &prs); err != nil {
		return nil, false, fmt.Errorf("failed to parse PRs: %w", err)
	}
	return prs, len(prs) == importPageSize, nil
}

// githubGet is a thin wrapper for paginated reads. We bound the
// response size to 10MB to defend against runaway responses; a
// well-formed page is always under ~500KB.
func githubGet(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github fetch: %w", err)
	}
	defer resp.Body.Close()
	if err := checkGitHubRateLimit(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github API error %d: %s", resp.StatusCode, string(body))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
}

// createTaskFromIssue is the per-item creation step. Mirrors the old
// inline body but lives in one place so both the legacy synchronous
// and new async paths can share it.
func createTaskFromIssue(ctx context.Context, link *githubLinkModel.GitHubLink, userInfo *userModels.UserInfo, dgraphProjectInfo *dgraphStruct.DgraphProject, issue *importedIssue) error {
	createTaskInput := adapter.CreateOrUpdateTaskInput{
		ProjectUuid:     link.ProjectId.String(),
		TaskName:        issue.Title,
		TaskDescription: issue.Body,
		Priority:        dgraphStruct.TASK_PRIORITY_MEDIUM,
		Status:          dgraphStruct.TASK_STATUS_TODO,
		GitHubIssueNum:  &issue.Number,
		GitHubIssueURL:  &issue.HTMLURL,
	}
	taskUUID, err := taskBusiness.CreateTask(ctx, link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, nil)
	if err != nil {
		return err
	}
	if err := taskDomain.SetGitHubIssueFieldsOnTask(ctx, taskUUID, issue.Number, issue.HTMLURL); err != nil {
		helpers.LogErrorWithContext(ctx, "github/createTaskFromIssue PG fields update failed for task=%s: %v", taskUUID.String(), err)
	}
	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:               "uid(task)",
		Uuid:              taskUUID.String(),
		GitHubIssueNumber: &issue.Number,
		GitHubIssueURL:    &issue.HTMLURL,
	}
	if _, dgErr := taskDomain.CreateOrUpdateDgraphTask(ctx, dgraphTask); dgErr != nil {
		helpers.LogErrorWithContext(ctx, "github/createTaskFromIssue Dgraph fields update failed for task=%s: %v", taskUUID.String(), dgErr)
	}
	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "issue_opened",
		nil, nil, nil, &issue.Title, &issue.Body, nil)
	return nil
}

// createTaskFromPR mirrors createTaskFromIssue for PRs.
func createTaskFromPR(ctx context.Context, link *githubLinkModel.GitHubLink, userInfo *userModels.UserInfo, dgraphProjectInfo *dgraphStruct.DgraphProject, pr *importedPR) error {
	createTaskInput := adapter.CreateOrUpdateTaskInput{
		ProjectUuid:     link.ProjectId.String(),
		TaskName:        pr.Title,
		TaskDescription: pr.Body,
		Priority:        dgraphStruct.TASK_PRIORITY_MEDIUM,
		Status:          dgraphStruct.TASK_STATUS_TODO,
		GitHubPRNum:     &pr.Number,
		GitHubPRURL:     &pr.HTMLURL,
	}
	taskUUID, err := taskBusiness.CreateTask(ctx, link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, nil)
	if err != nil {
		return err
	}
	if err := taskDomain.SetGitHubPRFieldsOnTask(ctx, taskUUID, pr.Number, pr.HTMLURL, pr.Head.Ref); err != nil {
		helpers.LogErrorWithContext(ctx, "github/createTaskFromPR PG fields update failed for task=%s: %v", taskUUID.String(), err)
	}
	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:            "uid(task)",
		Uuid:           taskUUID.String(),
		GitHubPRNumber: &pr.Number,
		GitHubPRURL:    &pr.HTMLURL,
	}
	if _, dgErr := taskDomain.CreateOrUpdateDgraphTask(ctx, dgraphTask); dgErr != nil {
		helpers.LogErrorWithContext(ctx, "github/createTaskFromPR Dgraph fields update failed for task=%s: %v", taskUUID.String(), dgErr)
	}
	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
		nil, nil, nil, &pr.Title, &pr.Body, nil)
	return nil
}

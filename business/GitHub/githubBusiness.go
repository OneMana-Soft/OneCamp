package business

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	githubCommentMappingDomain "github.com/akashc777/OneCamp/domain/GitHubCommentMapping"
	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// markInboundSyncFailed updates github_sync_status to failed when a GitHub webhook
// event cannot be applied to OneCamp. This gives users immediate visibility that
// the task is out of sync.
func markInboundSyncFailed(ctx context.Context, taskID uuid.UUID, reason string) {
	_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", &reason, 0)
}

// LinkRepoInput captures link creation parameters.
type LinkRepoInput struct {
	ProjectId       uuid.UUID `json:"project_id"`
	RepoOwner       string    `json:"repo_owner"`
	RepoName        string    `json:"repo_name"`
	SyncIssues      bool      `json:"sync_issues"`
	SyncPRs         bool      `json:"sync_prs"`
	AutoCreateTasks bool      `json:"auto_create_tasks"`
}

// GitHubStatusResponse represents the current integration status.
type GitHubStatusResponse struct {
	Connected   bool                  `json:"connected"`
	LinkedRepos []*GitHubLinkResponse `json:"linked_repos,omitempty"`
}

// GitHubRepo represents a minimal GitHub repository for listing.
type GitHubRepo struct {
	FullName    string `json:"full_name"`
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Private     bool   `json:"private"`
	HTMLURL     string `json:"html_url"`
}

// GitHubLinkResponse is the API-friendly representation of a linked repository.
type GitHubLinkResponse struct {
	Id                uuid.UUID         `json:"id"`
	ProjectId         uuid.UUID         `json:"project_id"`
	RepoOwner         string            `json:"repo_owner"`
	RepoName          string            `json:"repo_name"`
	InstallationId    *int64            `json:"installation_id,omitempty"`
	WebhookSecret     *string           `json:"webhook_secret,omitempty"`
	SyncIssues        bool              `json:"sync_issues"`
	SyncPRs           bool              `json:"sync_prs"`
	AutoCreateTasks   bool              `json:"auto_create_tasks"`
	DefaultTaskStatus string            `json:"default_task_status"`
	LabelMapping      *string           `json:"label_mapping,omitempty"`
	AutomationRules   map[string]string `json:"automation_rules,omitempty"`
	BranchFormat      string            `json:"branch_format"`
	CreatedBy         uuid.UUID         `json:"created_by"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
	DeletedAt         *time.Time        `json:"deleted_at,omitempty"`
}

func mapGitHubLinkToResponse(link *githubLinkModel.GitHubLink) *GitHubLinkResponse {
	if link == nil {
		return nil
	}
	resp := &GitHubLinkResponse{
		Id:                link.Id,
		ProjectId:         link.ProjectId,
		RepoOwner:         link.RepoOwner,
		RepoName:          link.RepoName,
		InstallationId:    link.InstallationId,
		WebhookSecret:     link.WebhookSecret,
		SyncIssues:        link.SyncIssues,
		SyncPRs:           link.SyncPRs,
		AutoCreateTasks:   link.AutoCreateTasks,
		DefaultTaskStatus: link.DefaultTaskStatus,
		LabelMapping:      link.LabelMapping,
		BranchFormat:      link.BranchFormat,
		CreatedBy:         link.CreatedBy,
		CreatedAt:         link.CreatedAt,
		UpdatedAt:         link.UpdatedAt,
		DeletedAt:         link.DeletedAt,
	}
	if link.AutomationRules != nil && *link.AutomationRules != "" {
		var rules map[string]string
		if err := json.Unmarshal([]byte(*link.AutomationRules), &rules); err == nil {
			resp.AutomationRules = rules
		}
	}
	return resp
}

// ExchangeCodeAndSave exchanges OAuth code and stores the integration.
func ExchangeCodeAndSave(ctx context.Context, code string, userId uuid.UUID) error {
	appCfg := GetGitHubAppConfig(ctx)
	clientId := appCfg.ClientID
	clientSecret := appCfg.ClientSecret

	if clientId == "" || clientSecret == "" {
		return fmt.Errorf("GitHub App credentials not configured")
	}

	// Exchange code for access token
	reqBody := fmt.Sprintf(`{"client_id":"%s","client_secret":"%s","code":"%s"}`, clientId, clientSecret, code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create token exchange request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to exchange code: %v", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken           string `json:"access_token"`
		TokenType             string `json:"token_type"`
		Scope                 string `json:"scope"`
		ExpiresIn             int    `json:"expires_in,omitempty"`
		RefreshToken          string `json:"refresh_token,omitempty"`
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in,omitempty"`
		Error                 string `json:"error,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("failed to parse token response")
	}

	if tokenResp.Error != "" {
		return fmt.Errorf("GitHub OAuth error: %s", tokenResp.Error)
	}

	if tokenResp.AccessToken == "" {
		return fmt.Errorf("no access token received from GitHub")
	}

	// Store in integrations table (org-level integration shared across admins)
	var refreshToken *string
	if tokenResp.RefreshToken != "" {
		refreshToken = &tokenResp.RefreshToken
	}
	// Persist expiry so the oauth2 library knows when to refresh.
	// GitHub OAuth Apps with token rotation enabled return expires_in
	// (8 hours / 28800s by default). Legacy non-rotating tokens omit
	// it; in that case we leave expires_at NULL.
	var expiresAt *time.Time
	if tokenResp.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		expiresAt = &t
	}

	err = integrationDomain.UpsertIntegration(ctx, "org", uuid.Nil, "github", &tokenResp.AccessToken, refreshToken, nil, nil, expiresAt)
	if err != nil {
		return fmt.Errorf("failed to save GitHub integration: %v", err)
	}

	invalidateBotLoginCache()
	invalidateGitHubTokenSourceCache()

	return nil
}

// GetGitHubStatus returns the current connection status and linked repos.
func GetGitHubStatus(ctx context.Context) (*GitHubStatusResponse, error) {
	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil {
		return nil, err
	}

	status := &GitHubStatusResponse{
		Connected: integration != nil && integration.AccessToken != nil,
	}

	if status.Connected {
		links, err := GetAllLinkedRepos(ctx)
		if err == nil {
			status.LinkedRepos = make([]*GitHubLinkResponse, 0, len(links))
			for _, link := range links {
				status.LinkedRepos = append(status.LinkedRepos, mapGitHubLinkToResponse(link))
			}
		}
	}

	return status, nil
}

// FetchRepositories lists repos accessible by the connected GitHub account.
// Follows pagination via the Link header to fetch all repos (not just 100).
func FetchRepositories(ctx context.Context) ([]*GitHubRepo, error) {
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	var allRepos []*GitHubRepo

	nextURL := "https://api.github.com/user/repos?per_page=100&sort=updated"
	pageCount := 0

	for nextURL != "" && pageCount < 10 {
		pageCount++
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch repos: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return nil, fmt.Errorf("GitHub API error (%d): %s", resp.StatusCode, string(body))
		}

		var ghRepos []struct {
			FullName    string `json:"full_name"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Private     bool   `json:"private"`
			HTMLURL     string `json:"html_url"`
			Owner       struct {
				Login string `json:"login"`
			} `json:"owner"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&ghRepos); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to parse repos response")
		}
		resp.Body.Close()

		for _, r := range ghRepos {
			allRepos = append(allRepos, &GitHubRepo{
				FullName:    r.FullName,
				Owner:       r.Owner.Login,
				Name:        r.Name,
				Description: r.Description,
				Private:     r.Private,
				HTMLURL:     r.HTMLURL,
			})
		}

		nextURL = extractNextPageURL(resp.Header.Get("Link"))
	}

	return allRepos, nil
}

// extractNextPageURL parses the GitHub Link header to find the next page URL.
func extractNextPageURL(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	links := strings.Split(linkHeader, ",")
	for _, link := range links {
		parts := strings.Split(strings.TrimSpace(link), ";")
		if len(parts) != 2 {
			continue
		}
		urlPart := strings.TrimSpace(parts[0])
		relPart := strings.TrimSpace(parts[1])
		if strings.Contains(relPart, `rel="next"`) {
			urlPart = strings.TrimPrefix(urlPart, "<")
			urlPart = strings.TrimSuffix(urlPart, ">")
			return urlPart
		}
	}
	return ""
}

// LinkRepositoryToProject creates a link between a GitHub repo and OneCamp project.
func LinkRepositoryToProject(ctx context.Context, input LinkRepoInput, createdBy uuid.UUID) (*githubLinkModel.GitHubLink, error) {
	if input.RepoOwner == "" || input.RepoName == "" {
		return nil, fmt.Errorf("repo_owner and repo_name are required")
	}
	if input.ProjectId == uuid.Nil {
		return nil, fmt.Errorf("project_id is required")
	}

	id := uuid.New()
	now := time.Now()

	// Generate webhook secret for this link
	secret := uuid.New().String()

	query := `
		INSERT INTO github_links (id, project_id, repo_owner, repo_name, installation_id, webhook_secret, sync_issues, sync_prs, auto_create_tasks, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)
	`
	err := githubLinkModel.CreateGitHubLink(query, id, input.ProjectId, input.RepoOwner, input.RepoName, nil, &secret, input.SyncIssues, input.SyncPRs, input.AutoCreateTasks, createdBy, now)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("this repository is already linked to this project")
		}
		return nil, err
	}

	// Fetch and return the created link
	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, id)
	if err != nil {
		return nil, err
	}

	// Invalidate the cached "no link for this repo" entry produced by
	// any earlier delivery from the same repo before linking.
	invalidateLinkByRepo(input.RepoOwner, input.RepoName)

	go registerGitHubWebhook(link, secret)

	return link, nil
}

// UnlinkRepository soft-deletes a link and cleans up task metadata in both PG and Dgraph.
func UnlinkRepository(ctx context.Context, linkId uuid.UUID) error {
	// Get the link first
	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, linkId)
	if err != nil {
		return err
	}
	if link == nil {
		return fmt.Errorf("link not found")
	}

	now := time.Now()

	// 1. Soft-delete the link
	deleteQuery := `UPDATE github_links SET deleted_at = $1, updated_at = $2 WHERE id = $3 AND deleted_at IS NULL`
	err = githubLinkModel.SoftDeleteGitHubLink(deleteQuery, now, linkId)
	if err != nil {
		return fmt.Errorf("failed to delete link: %v", err)
	}

	// 2. Batch-clear GitHub metadata from tasks in this project (PostgreSQL)
	clearQuery := `
		UPDATE tasks SET
			github_issue_number = NULL,
			github_issue_url = NULL,
			github_pr_number = NULL,
			github_pr_url = NULL,
			github_branch = NULL,
			updated_at = NOW()
		WHERE project_id = $1 AND github_issue_url IS NOT NULL
	`
	err = githubLinkModel.BatchClearGitHubFieldsByProject(clearQuery, link.ProjectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnlinkRepository Failed to clear PG task fields err: %+v", err)
	}

	// Delete webhook synchronously before returning so the operation is durable.
	deleteGitHubWebhook(link)

	// Invalidate the cached link for this repo so the next inbound
	// webhook delivery (if any in flight) doesn't see a stale entry.
	invalidateLinkByLink(link)

	helpers.MessageLogs.InfoLog.Printf("GitHub link %s unlinked from project %s. Task metadata cleared.", linkId.String(), link.ProjectId.String())

	return nil
}

// DisconnectGitHub fully disconnects: delete all links, revoke token, batch-clear all task metadata.
func DisconnectGitHub(ctx context.Context) error {
	allLinks, _ := GetAllLinkedRepos(ctx)

	// 1. Fetch integration token BEFORE deleting it (for revocation)
	integration, _ := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	var accessToken *string
	if integration != nil {
		accessToken = integration.AccessToken
	}

	now := time.Now()

	// 2. Soft-delete ALL github_links
	deleteAllQuery := `UPDATE github_links SET deleted_at = $1, updated_at = $2 WHERE deleted_at IS NULL`
	err := githubLinkModel.SoftDeleteAllGitHubLinks(deleteAllQuery, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DisconnectGitHub Failed to delete all links err: %+v", err)
	}

	// 3. Batch-clear ALL GitHub metadata from ALL tasks
	clearAllQuery := `
		UPDATE tasks SET
			github_issue_number = NULL,
			github_issue_url = NULL,
			github_pr_number = NULL,
			github_pr_url = NULL,
			github_branch = NULL,
			updated_at = NOW()
		WHERE github_issue_url IS NOT NULL OR github_pr_url IS NOT NULL
	`
	err = githubLinkModel.BatchClearAllGitHubFields(clearAllQuery)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DisconnectGitHub Failed to clear all task fields err: %+v", err)
	}

	// 4. Delete webhooks from GitHub synchronously BEFORE deleting integration
	// so the access token is still valid for GitHub API calls.
	if accessToken != nil && *accessToken != "" {
		for _, link := range allLinks {
			deleteGitHubWebhookWithToken(link, *accessToken)
		}
	}

	// 5. Delete the integration record
	err = integrationDomain.DeleteIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DisconnectGitHub Failed to delete integration err: %+v", err)
	}

	// 6. Best-effort: revoke token on GitHub
	if accessToken != nil {
		go revokeGitHubToken(context.Background(), *accessToken)
	}

	// 7. Clear cached bot login + cached OAuth token source + per-repo
	//    link cache. The bulk delete bypasses the per-repo
	//    invalidator, so a generation bump is the correct broad
	//    invalidation here.
	invalidateBotLoginCache()
	invalidateGitHubTokenSourceCache()
	invalidateGitHubLinkCache()

	helpers.MessageLogs.InfoLog.Println("GitHub integration fully disconnected. All links and task metadata cleared.")

	return nil
}

// GetLinkedRepos returns all repos linked to a specific project.
func GetLinkedRepos(ctx context.Context, projectId uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
	query := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE project_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC`
	return githubLinkModel.GetGitHubLinksByProjectId(query, projectId)
}

// GetAllLinkedRepos returns all active GitHub links.
func GetAllLinkedRepos(ctx context.Context) ([]*githubLinkModel.GitHubLink, error) {
	query := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE deleted_at IS NULL ORDER BY created_at DESC`
	return githubLinkModel.GetAllGitHubLinks(query)
}

// GetGitHubLinkById returns a link by its ID.
func GetGitHubLinkById(ctx context.Context, linkId uuid.UUID) (*githubLinkModel.GitHubLink, error) {
	query := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	return githubLinkModel.GetGitHubLinkById(query, linkId)
}

// ExecRaw executes a raw SQL query with args.
func ExecRaw(ctx context.Context, query string, args ...interface{}) error {
	ctx2, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx2, query, args...)
	return err
}

// ImportIssuesAsTasks imports open GitHub issues from a linked repo as OneCamp tasks.
//
// Deprecated: this synchronous variant is kept for backwards
// compatibility with any internal callers. New flows should call
// EnqueueImportIssues which routes through the background worker
// and returns immediately. This function delegates the actual work
// to the same per-page batch helpers so behaviour is identical.
func ImportIssuesAsTasks(ctx context.Context, linkId uuid.UUID, userId uuid.UUID) (int, error) {
	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, linkId)
	if err != nil || link == nil {
		return 0, fmt.Errorf("link not found")
	}

	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return 0, fmt.Errorf("GitHub not connected")
	}

	userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userId.String())
	if err != nil {
		return 0, fmt.Errorf("failed to get user dgraph info: %v", err)
	}
	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: userId},
		UserDgraphInfo:   *userDgraphInfo,
	}
	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, link.ProjectId.String(), userDgraphInfo.Uid)
	if err != nil {
		return 0, fmt.Errorf("failed to get project dgraph info: %v", err)
	}

	imported := 0
	page := 1
	for page <= importPagesMax {
		issues, hasMore, err := fetchIssuesPage(ctx, client, link, page)
		if err != nil {
			return imported, err
		}
		if len(issues) == 0 {
			break
		}
		urls := make([]string, 0, len(issues))
		for _, issue := range issues {
			if issue.PullRequest == nil {
				urls = append(urls, issue.HTMLURL)
			}
		}
		existing, _ := taskDomain.FindTasksByGitHubIssueURLs(ctx, urls)
		for i := range issues {
			issue := issues[i]
			if issue.PullRequest != nil {
				continue
			}
			if _, ok := existing[issue.HTMLURL]; ok {
				continue
			}
			if err := createTaskFromIssue(ctx, link, userInfo, dgraphProjectInfo, &issue); err != nil {
				helpers.LogErrorWithContext(ctx, "business/ImportIssuesAsTasks Failed to create task for issue #%d err: %+v", issue.Number, err)
				continue
			}
			imported++
		}
		if !hasMore {
			break
		}
		page++
	}

	return imported, nil
}

// ImportPRsAsTasks imports open GitHub PRs from a linked repo as OneCamp tasks.
//
// Deprecated: see the doc-comment on ImportIssuesAsTasks. New flows
// should call EnqueueImportPRs.
func ImportPRsAsTasks(ctx context.Context, linkId uuid.UUID, userId uuid.UUID) (int, error) {
	getQuery := `SELECT ` + githubLinkModel.GITHUB_LINK_COLS + ` FROM github_links WHERE id = $1 AND deleted_at IS NULL`
	link, err := githubLinkModel.GetGitHubLinkById(getQuery, linkId)
	if err != nil || link == nil {
		return 0, fmt.Errorf("link not found")
	}

	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return 0, fmt.Errorf("GitHub not connected")
	}

	userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, userId.String())
	if err != nil {
		return 0, fmt.Errorf("failed to get user dgraph info: %v", err)
	}
	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: userId},
		UserDgraphInfo:   *userDgraphInfo,
	}
	dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, link.ProjectId.String(), userDgraphInfo.Uid)
	if err != nil {
		return 0, fmt.Errorf("failed to get project dgraph info: %v", err)
	}

	imported := 0
	page := 1
	for page <= importPagesMax {
		prs, hasMore, err := fetchPRsPage(ctx, client, link, page)
		if err != nil {
			return imported, err
		}
		if len(prs) == 0 {
			break
		}
		urls := make([]string, 0, len(prs))
		for _, pr := range prs {
			urls = append(urls, pr.HTMLURL)
		}
		existing, _ := taskDomain.FindTasksByGitHubPRURLs(ctx, urls)
		for i := range prs {
			pr := prs[i]
			if _, ok := existing[pr.HTMLURL]; ok {
				continue
			}
			if err := createTaskFromPR(ctx, link, userInfo, dgraphProjectInfo, &pr); err != nil {
				helpers.LogErrorWithContext(ctx, "business/ImportPRsAsTasks Failed to create task for PR #%d err: %+v", pr.Number, err)
				continue
			}
			imported++
		}
		if !hasMore {
			break
		}
		page++
	}

	return imported, nil
}

// HandleGitHubWebhookEvent processes an incoming GitHub webhook event.
func HandleGitHubWebhookEvent(ctx context.Context, eventType string, body []byte) error {
	// Everything below applies a change that came FROM GitHub, through the same
	// business functions a person's edit goes through, and those enqueue an
	// outbound sync. Marking the context once here is what stops the value we
	// just received being PATCHed straight back onto the issue it came from.
	// business.EnqueueGitHubSync reads the marker.
	ctx = helpers.WithGitHubOrigin(ctx)

	switch eventType {
	case "issues":
		return handleIssueEvent(ctx, body)
	case "pull_request":
		return handlePullRequestEvent(ctx, body)
	case "push":
		return handlePushEvent(ctx, body)
	case "issue_comment":
		return handleIssueCommentEvent(ctx, body)
	case "create":
		return handleCreateEvent(ctx, body)
	case "pull_request_review":
		return handlePullRequestReviewEvent(ctx, body)
	case "check_run":
		return handleCheckRunEvent(ctx, body)
	default:
		helpers.MessageLogs.InfoLog.Printf("Ignoring unhandled GitHub event: %s", eventType)
		return nil
	}
}

// applyAutomationRules applies per-link automation rules to update task status.
//
// We use the cached, pre-parsed rules map keyed by (owner, name) to
// avoid a json.Unmarshal of the same blob on every webhook event.
func applyAutomationRules(ctx context.Context, link *githubLinkModel.GitHubLink, taskUUIDStr string, trigger string) {
	if link == nil || taskUUIDStr == "" {
		return
	}
	targetStatus := ruleTarget(parseAutomationRules(link), trigger)
	if targetStatus == "" {
		return
	}
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		return
	}
	// The rule may name one of the project's own statuses. One that was
	// renamed or deleted a moment ago on another server is still in this
	// server's cached copy of the rules, so on a miss read the link again.
	to, err := taskStatusBusiness.Resolve(ctx, link.ProjectId.String(), targetStatus)
	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		if fresh, ferr := GetGitHubLinkById(ctx, link.Id); ferr == nil && fresh != nil {
			invalidateLinkByLink(fresh)
			if targetStatus = ruleTarget(parseAutomationRules(fresh), trigger); targetStatus == "" {
				return
			}
			to, err = taskStatusBusiness.Resolve(ctx, link.ProjectId.String(), targetStatus)
		}
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/applyAutomationRules rule %s on %s/%s names a status the project does not have (%q): %v",
			trigger, link.RepoOwner, link.RepoName, targetStatus, err)
		return
	}
	// Through the same path as every other status change, acting as the person
	// who linked the repository. This used to write only the Postgres column,
	// which nothing in the app reads, so a rule never visibly moved a task.
	actor, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
	if err != nil || actor == nil {
		helpers.LogErrorWithContext(ctx, "business/applyAutomationRules Failed to load the linking user: %v", err)
		return
	}
	dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, actor.Uid)
	if err != nil || dgraphTaskInfo == nil {
		helpers.LogErrorWithContext(ctx, "business/applyAutomationRules Failed to load task: %v", err)
		return
	}
	if err := taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, to.Value(), dgraphTaskInfo, actor); err != nil {
		helpers.LogErrorWithContext(ctx, "business/applyAutomationRules Failed to update task status: %v", err)
		return
	}

	// The activity says the status as people know it, "QA", not an id.
	shown := to.Display()
	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "status_synced",
		nil, nil, nil, &shown, nil, nil)
	publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "status_synced", statusSyncPayload(to))
	helpers.MessageLogs.InfoLog.Printf("Applied automation rule %s -> %s for task %s", trigger, shown, taskUUIDStr)
}

func handleIssueEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action string `json:"action"`
		Issue  struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			HTMLURL   string `json:"html_url"`
			State     string `json:"state"`
			UpdatedAt string `json:"updated_at"`
			Assignee  *struct {
				Login string `json:"login"`
			} `json:"assignee"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"issue"`
		Label *struct {
			Name string `json:"name"`
		} `json:"label"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse issue event: %v", err)
	}

	helpers.MessageLogs.InfoLog.Printf("GitHub issue event: action=%s issue=#%d repo=%s", event.Action, event.Issue.Number, event.Repository.FullName)

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil {
		helpers.MessageLogs.InfoLog.Printf("No GitHub link found for repo: %s", event.Repository.FullName)
		return nil
	}

	if !link.SyncIssues {
		helpers.MessageLogs.InfoLog.Printf("Skipping issue event for %s — sync_issues disabled", event.Repository.FullName)
		return nil
	}

	// Loop prevention: check if webhook was triggered by our own sync
	webhookTime, parseErr := time.Parse(time.RFC3339, event.Issue.UpdatedAt)
	if parseErr == nil {
		lastSyncedAt, err := taskDomain.GetTaskLastSyncedAtByIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to get last_synced_at for issue #%d: %v", event.Issue.Number, err)
		}
		if lastSyncedAt != nil && !webhookTime.After(*lastSyncedAt) {
			helpers.MessageLogs.InfoLog.Printf("Skipping issue webhook for #%d — triggered by our own sync", event.Issue.Number)
			return nil
		}
	}

	switch event.Action {
	case "opened":
		if !link.AutoCreateTasks {
			return nil
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil {
			return fmt.Errorf("failed to get user dgraph info: %v", err)
		}
		userInfo := &userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: link.CreatedBy},
			UserDgraphInfo:   *userDgraphInfo,
		}

		dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, link.ProjectId.String(), userDgraphInfo.Uid)
		if err != nil {
			return fmt.Errorf("failed to get project dgraph info: %v", err)
		}

		// Use automation rule if configured, otherwise fall back to Todo
		defaultStatus := dgraphStruct.TASK_STATUS_TODO
		if link.AutomationRules != nil {
			var rules map[string]string
			if err := json.Unmarshal([]byte(*link.AutomationRules), &rules); err == nil {
				if s, ok := rules["issue_opened"]; ok && s != "" && s != "_none" {
					defaultStatus = s
				}
			}
		}

		createTaskInput := adapter.CreateOrUpdateTaskInput{
			ProjectUuid:     link.ProjectId.String(),
			TaskName:        event.Issue.Title,
			TaskDescription: event.Issue.Body,
			Priority:        dgraphStruct.TASK_PRIORITY_MEDIUM,
			Status:          defaultStatus,
			GitHubIssueNum:  &event.Issue.Number,
			GitHubIssueURL:  &event.Issue.HTMLURL,
		}

		var mentionsUsers []*dgraphStruct.DgraphUser
		taskUUID, err := taskBusiness.CreateTask(ctx, link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, mentionsUsers)
		if err != nil {
			return fmt.Errorf("failed to auto-create task from issue: %v", err)
		}

		if err := taskDomain.SetGitHubIssueFieldsOnTask(ctx, taskUUID, event.Issue.Number, event.Issue.HTMLURL); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to set GitHub fields for task %s: %v", taskUUID.String(), err)
		}

		dgraphTask := &dgraphStruct.DgraphTask{
			Uid: "uid(task)", Uuid: taskUUID.String(),
			GitHubIssueNumber: &event.Issue.Number,
			GitHubIssueURL:    &event.Issue.HTMLURL,
		}
		taskDomain.CreateOrUpdateDgraphTask(ctx, dgraphTask)

		go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "issue_opened",
			&event.Sender.Login, nil, nil, &event.Issue.Title, nil, nil)
		publishGitHubSyncMqtt(taskUUID.String(), link.ProjectId.String(), "issue_opened", map[string]interface{}{
			"name":        event.Issue.Title,
			"description": event.Issue.Body,
			"status":      defaultStatus,
		})

		helpers.MessageLogs.InfoLog.Printf("Auto-created task %s for opened issue #%d", taskUUID.String(), event.Issue.Number)

		// Fan out an internal event so the AI issue-triage agent (if enabled)
		// can analyse the issue against the repo code and post a proposed fix as
		// a task comment. Decoupled via the event bus so this package never
		// imports the AI/code-agent packages (which import this one).
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.issue.opened", map[string]interface{}{
			"owner":        event.Repository.Owner.Login,
			"repo":         event.Repository.Name,
			"issue_number": event.Issue.Number,
			"issue_url":    event.Issue.HTMLURL,
			"title":        event.Issue.Title,
			"body":         event.Issue.Body,
			"task_uuid":    taskUUID.String(),
			"project_id":   link.ProjectId.String(),
			"created_by":   link.CreatedBy.String(),
		})

	case "closed", "reopened":
		trigger := "issue_closed"
		defaultStatus := dgraphStruct.TASK_STATUS_DONE
		if event.Action == "reopened" {
			trigger = "issue_reopened"
			defaultStatus = dgraphStruct.TASK_STATUS_TODO
		}

		taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr == "" {
			helpers.MessageLogs.InfoLog.Printf("No task found for issue URL: %s", event.Issue.HTMLURL)
			return nil
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
			return nil
		}

		// Try automation rule first, fall back to hardcoded default
		if link.AutomationRules != nil {
			var rules map[string]string
			if err := json.Unmarshal([]byte(*link.AutomationRules), &rules); err == nil {
				if s, ok := rules[trigger]; ok && s != "" && s != "_none" {
					applyAutomationRules(ctx, link, taskUUIDStr, trigger)
					return nil
				}
			}
		}

		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to get user info: %v", err)
			return nil
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to get task info: %v", err)
			return nil
		}
		err = taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, defaultStatus, dgraphTaskInfo, userDgraphInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update status: %v", err)
			markInboundSyncFailed(ctx, taskUUID, "GitHub issue status sync failed")
			return nil
		}
		helpers.MessageLogs.InfoLog.Printf("Updated task %s status to %s based on issue %s", taskUUIDStr, defaultStatus, event.Action)
		activityType := "issue_closed"
		if event.Action == "reopened" {
			activityType = "issue_reopened"
		}
		go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
			&event.Sender.Login, nil, nil, &event.Issue.Title, nil, nil)
		publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), activityType, map[string]interface{}{
			"status": defaultStatus,
		})

	case "edited":
		taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr == "" {
			return nil
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
			return nil
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			return nil
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			return nil
		}
		if event.Issue.Title != dgraphTaskInfo.Name {
			if err := taskBusiness.UpdateTaskNameByTaskUUID(ctx, taskUUID, event.Issue.Title, dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update task name: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub issue title sync failed")
			}
		}
		if event.Issue.Body != "" && (dgraphTaskInfo.Description == nil || event.Issue.Body != *dgraphTaskInfo.Description) {
			if err := taskBusiness.UpdateTaskDesByTaskUUID(ctx, taskUUID, event.Issue.Body, nil, dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update task desc: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub issue description sync failed")
			}
		}
		publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "issue_edited", map[string]interface{}{
			"name":        event.Issue.Title,
			"description": event.Issue.Body,
		})

	case "assigned":
		if event.Issue.Assignee == nil {
			break
		}
		assignedUUID := MapGitHubUserToOneCampUser(event.Issue.Assignee.Login)
		if assignedUUID == "" {
			break
		}
		taskUUIDStr2, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr2 == "" {
			break
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr2)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr2, parseErr)
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, assignedUUID)
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr2, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		oldAssigneeUID := ""
		if dgraphTaskInfo.Assignee != nil {
			oldAssigneeUID = dgraphTaskInfo.Assignee.Uid
		}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, userDgraphInfo, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, userDgraphInfo); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update assignee: %v", err)
		}
		publishGitHubSyncMqtt(taskUUIDStr2, link.ProjectId.String(), "assignee_synced", map[string]interface{}{
			"assignee_uuid":  userDgraphInfo.Uuid,
			"assignee_name":  userDgraphInfo.UserName,
			"assignee_login": event.Issue.Assignee.Login,
		})

	case "unassigned":
		taskUUIDStr3, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr3 == "" {
			break
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr3)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr3, parseErr)
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr3, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		oldAssigneeUID := ""
		if dgraphTaskInfo.Assignee != nil {
			oldAssigneeUID = dgraphTaskInfo.Assignee.Uid
		}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, nil, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, userDgraphInfo); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to unassign: %v", err)
		}
		publishGitHubSyncMqtt(taskUUIDStr3, link.ProjectId.String(), "assignee_synced", map[string]interface{}{
			"assignee_uuid": "",
			"assignee_name": "",
		})

	case "labeled":
		if event.Label == nil || event.Label.Name == "" {
			break
		}
		taskUUIDStr4, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr4 == "" {
			break
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr4)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr4, parseErr)
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr4, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		// Only update if the new label actually differs from the current task label
		if dgraphTaskInfo.Label == nil || *dgraphTaskInfo.Label != event.Label.Name {
			if err := taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, event.Label.Name, dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update label: %v", err)
			} else {
				publishGitHubSyncMqtt(taskUUIDStr4, link.ProjectId.String(), "label_synced", map[string]interface{}{
					"label": event.Label.Name,
				})
			}
		}

	case "unlabeled":
		if event.Label == nil || event.Label.Name == "" {
			break
		}
		taskUUIDStr5, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr5 == "" {
			break
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr5)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Invalid task UUID %s: %v", taskUUIDStr5, parseErr)
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr5, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		// Only clear the task label if the removed GitHub label matches the current task label
		if dgraphTaskInfo.Label != nil && *dgraphTaskInfo.Label == event.Label.Name {
			if err := taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, "", dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to remove label: %v", err)
			} else {
				publishGitHubSyncMqtt(taskUUIDStr5, link.ProjectId.String(), "label_synced", map[string]interface{}{
					"label": "",
				})
			}
		}
	}

	return nil
}

func handlePullRequestEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action      string `json:"action"`
		PullRequest struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			HTMLURL   string `json:"html_url"`
			State     string `json:"state"`
			Merged    bool   `json:"merged"`
			UpdatedAt string `json:"updated_at"`
			Body      string `json:"body"`
			Draft     bool   `json:"draft"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Assignee *struct {
				Login string `json:"login"`
			} `json:"assignee,omitempty"`
		} `json:"pull_request"`
		Label *struct {
			Name string `json:"name"`
		} `json:"label,omitempty"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
		RequestedReviewers []struct {
			Login string `json:"login"`
		} `json:"requested_reviewers"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse PR event: %v", err)
	}

	helpers.MessageLogs.InfoLog.Printf("GitHub PR event: action=%s pr=#%d branch=%s repo=%s", event.Action, event.PullRequest.Number, event.PullRequest.Head.Ref, event.Repository.FullName)

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil {
		return nil
	}

	if !link.SyncPRs {
		helpers.MessageLogs.InfoLog.Printf("Skipping PR event for %s — sync_prs disabled", event.Repository.FullName)
		return nil
	}

	// Loop prevention for PR events
	webhookTime, parseErr := time.Parse(time.RFC3339, event.PullRequest.UpdatedAt)
	if parseErr == nil {
		lastSyncedAt, err := taskDomain.GetTaskLastSyncedAtByPRURL(ctx, event.PullRequest.HTMLURL)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to get last_synced_at for PR #%d: %v", event.PullRequest.Number, err)
		}
		if lastSyncedAt != nil && !webhookTime.After(*lastSyncedAt) {
			helpers.MessageLogs.InfoLog.Printf("Skipping PR webhook for #%d — triggered by our own sync", event.PullRequest.Number)
			return nil
		}
	}

	// Try to find an existing task linked by branch name first, then by issue URL references in PR body
	taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubBranch(ctx, event.PullRequest.Head.Ref)
	if err != nil || taskUUIDStr == "" {
		// Fallback: look for task URLs in the PR body (Linear-style magic linking)
		taskUUIDStr = findTaskUUIDInText(ctx, event.PullRequest.Body)
	}

	// Validate taskUUIDStr is a valid UUID before proceeding
	var taskUUID uuid.UUID
	if taskUUIDStr != "" {
		var parseErr error
		taskUUID, parseErr = uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
			taskUUIDStr = ""
		}
	}

	// Determine PR state for tracking
	prState := event.PullRequest.State
	if event.PullRequest.Merged {
		prState = "merged"
	}
	isDraft := event.PullRequest.Draft

	// emitPRReview fans out an internal event so the AI agent (if enabled) can
	// review the PR diff and post feedback as a task comment. Decoupled via the
	// event bus so this package never imports the AI/code-agent packages. Draft
	// PRs are skipped (still a work in progress).
	emitPRReview := func(linkedTaskUUID string) {
		if linkedTaskUUID == "" || isDraft {
			return
		}
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.pr.opened", map[string]interface{}{
			"owner":      event.Repository.Owner.Login,
			"repo":       event.Repository.Name,
			"pr_number":  event.PullRequest.Number,
			"pr_url":     event.PullRequest.HTMLURL,
			"title":      event.PullRequest.Title,
			"body":       event.PullRequest.Body,
			"task_uuid":  linkedTaskUUID,
			"created_by": link.CreatedBy.String(),
		})
	}

	switch event.Action {
	case "opened":
		if taskUUIDStr != "" {
			if err := taskDomain.SetGitHubPRFieldsOnTask(ctx, taskUUID, event.PullRequest.Number, event.PullRequest.HTMLURL, event.PullRequest.Head.Ref); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to set PR fields: %v", err)
			}
			if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(isDraft)); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub PR state sync failed")
			}
			go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
				&event.PullRequest.User.Login, nil, nil, &event.PullRequest.Title, nil, nil)
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_opened", nil)
			trigger := "pr_opened"
			if isDraft {
				trigger = "pr_drafted"
			}
			applyAutomationRules(ctx, link, taskUUIDStr, trigger)
			helpers.MessageLogs.InfoLog.Printf("Linked PR #%d to task %s via branch %s", event.PullRequest.Number, taskUUIDStr, event.PullRequest.Head.Ref)
			emitPRReview(taskUUIDStr)
		} else if link.AutoCreateTasks {
			// Auto-create a task for the PR if no existing task is found
			userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
			if err != nil || userDgraphInfo == nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to get user dgraph info for auto-create: %v", err)
			} else {
				userInfo := &userModels.UserInfo{
					UserPostgresInfo: userModels.User{Id: link.CreatedBy},
					UserDgraphInfo:   *userDgraphInfo,
				}
				dgraphProjectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx, link.ProjectId.String(), userDgraphInfo.Uid)
				if err != nil || dgraphProjectInfo == nil {
					helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to get project dgraph info for auto-create: %v", err)
				} else {
					createTaskInput := adapter.CreateOrUpdateTaskInput{
						ProjectUuid:     link.ProjectId.String(),
						TaskName:        event.PullRequest.Title,
						TaskDescription: event.PullRequest.Body,
						Priority:        dgraphStruct.TASK_PRIORITY_MEDIUM,
						Status:          dgraphStruct.TASK_STATUS_INPROGRESS,
						GitHubPRNum:     &event.PullRequest.Number,
						GitHubPRURL:     &event.PullRequest.HTMLURL,
					}
					var mentionsUsers []*dgraphStruct.DgraphUser
					taskUUID, err := taskBusiness.CreateTask(ctx, link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, mentionsUsers)
					if err != nil {
						helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to auto-create task for PR #%d: %v", event.PullRequest.Number, err)
					} else {
						if err := taskDomain.SetGitHubPRFieldsOnTask(ctx, taskUUID, event.PullRequest.Number, event.PullRequest.HTMLURL, event.PullRequest.Head.Ref); err != nil {
							helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to set PR fields on auto-created task: %v", err)
						}
						if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(isDraft)); err != nil {
							helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state on auto-created task: %v", err)
							markInboundSyncFailed(ctx, taskUUID, "GitHub PR state sync failed")
						}
						go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
							&event.PullRequest.User.Login, nil, nil, &event.PullRequest.Title, nil, nil)
						publishGitHubSyncMqtt(taskUUID.String(), link.ProjectId.String(), "pr_opened", map[string]interface{}{
							"pr_state":    prState,
							"pr_is_draft": isDraft,
							"pr_number":   event.PullRequest.Number,
						})
						trigger := "pr_opened"
						if isDraft {
							trigger = "pr_drafted"
						}
						applyAutomationRules(ctx, link, taskUUID.String(), trigger)
						helpers.MessageLogs.InfoLog.Printf("Auto-created task %s for opened PR #%d", taskUUID.String(), event.PullRequest.Number)
						emitPRReview(taskUUID.String())
					}
				}
			}
		}
	case "converted_to_draft":
		if taskUUIDStr != "" {
			if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(true)); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state for draft: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub PR draft sync failed")
			}
			applyAutomationRules(ctx, link, taskUUIDStr, "pr_drafted")
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_drafted", map[string]interface{}{
				"pr_is_draft": true,
			})
		}
	case "ready_for_review":
		if taskUUIDStr != "" {
			if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(false)); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state for ready: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub PR ready sync failed")
			}
			applyAutomationRules(ctx, link, taskUUIDStr, "pr_opened")
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_ready_for_review", map[string]interface{}{
				"pr_is_draft": false,
			})
		}
	case "review_requested":
		if taskUUIDStr != "" {
			applyAutomationRules(ctx, link, taskUUIDStr, "review_requested")
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "review_requested", nil)
		}
	case "closed":
		// Capture the PR's terminal outcome for the agent code-PR learning loop
		// (merge rate is the ground-truth quality signal). Fanned out on the
		// event bus so this package never imports the AI/code-PR packages; the
		// handler matches the run by pr_url and is a no-op for human PRs. Fired
		// regardless of task linkage, since an agent PR may not be task-linked.
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.pr.closed", map[string]interface{}{
			"pr_url": event.PullRequest.HTMLURL,
			"merged": event.PullRequest.Merged,
		})
		if taskUUIDStr != "" {
			activityType := "pr_closed"
			trigger := "pr_closed_without_merge"
			if event.PullRequest.Merged {
				activityType = "pr_merged"
				trigger = "pr_merged"
				prState = "merged"
			}
			if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(isDraft)); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state for close: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub PR close sync failed")
			}
			go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
				&event.PullRequest.User.Login, nil, nil, &event.PullRequest.Title, nil, nil)
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), activityType, map[string]interface{}{
				"pr_state":    prState,
				"pr_is_draft": isDraft,
			})
			applyAutomationRules(ctx, link, taskUUIDStr, trigger)
		}
	case "reopened":
		if taskUUIDStr != "" {
			if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(isDraft)); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update PR state for reopen: %v", err)
				markInboundSyncFailed(ctx, taskUUID, "GitHub PR reopen sync failed")
			}
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_reopened", map[string]interface{}{
				"pr_state":    prState,
				"pr_is_draft": isDraft,
			})
		}
	case "edited":
		if taskUUIDStr != "" {
			userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
			if err == nil && userDgraphInfo != nil {
				dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
				if err == nil && dgraphTaskInfo != nil {
					if event.PullRequest.Title != dgraphTaskInfo.Name {
						if err := taskBusiness.UpdateTaskNameByTaskUUID(ctx, taskUUID, event.PullRequest.Title, dgraphTaskInfo, userDgraphInfo); err != nil {
							helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update task name from PR edit: %v", err)
						}
					}
					if event.PullRequest.Body != "" && (dgraphTaskInfo.Description == nil || event.PullRequest.Body != *dgraphTaskInfo.Description) {
						if err := taskBusiness.UpdateTaskDesByTaskUUID(ctx, taskUUID, event.PullRequest.Body, nil, dgraphTaskInfo, userDgraphInfo); err != nil {
							helpers.LogErrorWithContext(ctx, "business/handlePullRequestEvent Failed to update task desc from PR edit: %v", err)
						}
					}
				}
			}
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_edited", map[string]interface{}{
				"name":        event.PullRequest.Title,
				"description": event.PullRequest.Body,
			})
		}
	case "assigned":
		if taskUUIDStr != "" && event.PullRequest.Assignee != nil {
			assignedUUID := MapGitHubUserToOneCampUser(event.PullRequest.Assignee.Login)
			if assignedUUID != "" {
				assigneeDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, assignedUUID)
				if assigneeDgraphInfo != nil {
					dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, assigneeDgraphInfo.Uid)
					if dgraphTaskInfo != nil {
						oldAssigneeUID := ""
						if dgraphTaskInfo.Assignee != nil {
							oldAssigneeUID = dgraphTaskInfo.Assignee.Uid
						}
						_ = taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, assigneeDgraphInfo, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, assigneeDgraphInfo)
						publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "assignee_synced", map[string]interface{}{
							"assignee_uuid":  assigneeDgraphInfo.Uuid,
							"assignee_name":  assigneeDgraphInfo.UserName,
							"assignee_login": event.PullRequest.Assignee.Login,
						})
					}
				}
			}
		}
	case "unassigned":
		if taskUUIDStr != "" {
			dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
			if dgraphTaskInfo != nil {
				oldAssigneeUID := ""
				if dgraphTaskInfo.Assignee != nil {
					oldAssigneeUID = dgraphTaskInfo.Assignee.Uid
				}
				_ = taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, nil, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, dgraphTaskInfo.CreatedBy)
				publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "assignee_synced", map[string]interface{}{
					"assignee_uuid": "",
					"assignee_name": "",
				})
			}
		}
	case "labeled":
		if taskUUIDStr != "" && event.Label != nil && event.Label.Name != "" {
			dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
			if dgraphTaskInfo != nil {
				if dgraphTaskInfo.Label == nil || *dgraphTaskInfo.Label != event.Label.Name {
					userDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
					if userDgraphInfo != nil {
						_ = taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, event.Label.Name, dgraphTaskInfo, userDgraphInfo)
						publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "label_synced", map[string]interface{}{
							"label": event.Label.Name,
						})
					}
				}
			}
		}
	case "unlabeled":
		if taskUUIDStr != "" && event.Label != nil && event.Label.Name != "" {
			dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
			if dgraphTaskInfo != nil && dgraphTaskInfo.Label != nil && *dgraphTaskInfo.Label == event.Label.Name {
				userDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
				if userDgraphInfo != nil {
					_ = taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, "", dgraphTaskInfo, userDgraphInfo)
					publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "label_synced", map[string]interface{}{
						"label": "",
					})
				}
			}
		}
	}

	return nil
}

// handleIssueCommentEvent creates a task comment from a GitHub issue comment.
// prCommentMayDriveAgent reports whether a PR comment came from someone OTHER than us,
// and so may be allowed to continue an agent's work.
//
// TWO WAYS A COMMENT CAN BE OURS, and until now only one was checked.
//
// A GitHub App or bot account arrives with sender.type == "Bot", which the original guard
// caught. But OneCamp's own github_comment tool posts through the connected person's OAuth
// token — the tool says so, "on the user's behalf" — so a comment this product wrote
// arrives as sender.type == "User" and walked straight past that check.
//
// That closes a real loop. The event this guards emits continues the agent run that opened
// the PR. An agent granted github_comment could therefore comment, be re-driven by its own
// comment, comment again, and keep going — burning tokens and posting publicly on someone
// else's repository each time round. The guard's own comment already said a comment must
// never self-drive the agent; it just could not see this half.
//
// botLogin is the OAuth token owner's GitHub login, or "" when it could not be resolved.
// Empty deliberately does NOT block: failing to reach GitHub for a login must not silence
// every human comment on every PR. The Bot check still stands in that case.
//
// Pure, so the decision is testable without a webhook, a token or a network.
func prCommentMayDriveAgent(senderType, commenterLogin, botLogin string) bool {
	if strings.EqualFold(strings.TrimSpace(senderType), "Bot") {
		return false // a bot/app comment must never self-drive the agent
	}
	botLogin = strings.TrimSpace(botLogin)
	if botLogin != "" && strings.EqualFold(strings.TrimSpace(commenterLogin), botLogin) {
		return false // our own comment, posted through the user's token
	}
	return true
}

// dispatchAgentPRCommentIfAny emits a github.pr.comment workspace event when an
// issue_comment webhook is actually a comment on a pull request's conversation
// (issue.pull_request present), authored by someone other than us, on creation. It parses
// a minimal view of the raw payload so the shared issue-comment handlers keep their exact
// struct shape. The AI listener matches the PR to the run that opened it and continues
// that agent's work; a no-op for any PR no agent opened. Best-effort: a parse miss simply
// skips the fan-out.
func dispatchAgentPRCommentIfAny(ctx context.Context, body []byte, owner, repo string, issueNumber int) {
	var pv struct {
		Action  string `json:"action"`
		Comment struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"comment"`
		Issue struct {
			PullRequest *struct {
				HTMLURL string `json:"html_url"`
			} `json:"pull_request"`
		} `json:"issue"`
		Sender struct {
			Type string `json:"type"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &pv); err != nil {
		return
	}
	if pv.Action != "created" || pv.Issue.PullRequest == nil {
		return
	}
	if !prCommentMayDriveAgent(pv.Sender.Type, pv.Comment.User.Login, getGitHubBotLogin()) {
		return
	}
	prURL := strings.TrimSpace(pv.Issue.PullRequest.HTMLURL)
	if prURL == "" {
		prURL = fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, issueNumber)
	}
	webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.pr.comment", map[string]interface{}{
		"pr_url":          prURL,
		"body":            pv.Comment.Body,
		"commenter_login": pv.Comment.User.Login,
	})
}

func handleIssueCommentEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action  string `json:"action"`
		Comment struct {
			ID        int    `json:"id"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"comment"`
		Issue struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		} `json:"issue"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse issue comment event: %v", err)
	}

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil || (!link.SyncIssues && !link.SyncPRs) {
		return nil
	}

	// Agent PR follow-up: a human comment on the conversation of a pull request
	// an AI teammate opened should continue that teammate's work in the OneCamp
	// thread it posted to — EVEN when the PR isn't linked to a task (the common
	// case for an @mention-driven code PR). GitHub delivers PR conversation
	// comments as issue_comment with issue.pull_request set. Fan out on the
	// workspace bus so this package stays AI-free; the AI listener matches the
	// PR to its originating run and continues it (a no-op for any PR no agent
	// opened). Parsed into a separate minimal struct so the shared comment
	// handlers below keep their exact event shape. Bot senders are skipped so an
	// agent/app comment can't self-drive.
	dispatchAgentPRCommentIfAny(ctx, body, event.Repository.Owner.Login, event.Repository.Name, event.Issue.Number)

	taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
	if err != nil || taskUUIDStr == "" {
		prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", event.Repository.Owner.Login, event.Repository.Name, event.Issue.Number)
		taskUUIDStr, _ = taskDomain.FindTaskUUIDByPRURL(ctx, prURL)
		if taskUUIDStr == "" {
			helpers.MessageLogs.InfoLog.Printf("No task found for issue/PR #%d in repo %s/%s", event.Issue.Number, event.Repository.Owner.Login, event.Repository.Name)
			return nil
		}
	}

	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/handleIssueCommentEvent Invalid task UUID %s: %v", taskUUIDStr, err)
		return nil
	}

	switch event.Action {
	case "created":
		return handleIssueCommentCreated(ctx, event, taskUUID, taskUUIDStr, link)
	case "edited":
		return handleIssueCommentEdited(ctx, event, taskUUID, taskUUIDStr)
	case "deleted":
		return handleIssueCommentDeleted(ctx, event, taskUUID, taskUUIDStr)
	default:
		return nil
	}
}

func handleIssueCommentCreated(ctx context.Context, event struct {
	Action  string `json:"action"`
	Comment struct {
		ID        int    `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}, taskUUID uuid.UUID, taskUUIDStr string, link *githubLinkModel.GitHubLink) error {
	// Dedup: skip if this GitHub comment was already synced
	existingCommentUUID, _ := githubCommentMappingDomain.GetGitHubCommentUUID(ctx, event.Repository.Owner.Login, event.Repository.Name, int64(event.Comment.ID))
	if existingCommentUUID != "" {
		helpers.MessageLogs.InfoLog.Printf("Skipping duplicate GitHub comment %d for issue #%d — already synced", event.Comment.ID, event.Issue.Number)
		return nil
	}

	oneCampUserUUID := MapGitHubUserToOneCampUser(event.Comment.User.Login)
	var userDgraphInfo *dgraphStruct.DgraphUser
	var commenterUUID uuid.UUID
	if oneCampUserUUID != "" {
		var uErr error
		userDgraphInfo, uErr = userBusiness.GetDgraphUserInfoByUUID(ctx, oneCampUserUUID)
		if uErr == nil && userDgraphInfo != nil {
			commenterUUID, _ = uuid.Parse(oneCampUserUUID)
		}
	}
	if userDgraphInfo == nil {
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
		if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.CreatedBy == nil {
			markInboundSyncFailed(ctx, taskUUID, fmt.Sprintf("GitHub comment sync failed: could not resolve user @%s", event.Comment.User.Login))
			return nil
		}
		userDgraphInfo, err = userBusiness.GetDgraphUserInfoByUUID(ctx, dgraphTaskInfo.CreatedBy.Uuid)
		if err != nil || userDgraphInfo == nil {
			markInboundSyncFailed(ctx, taskUUID, "GitHub comment sync failed: could not resolve task creator")
			return nil
		}
		commenterUUID, _ = uuid.Parse(dgraphTaskInfo.CreatedBy.Uuid)
	}

	dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
	if err != nil || dgraphTaskInfo == nil {
		markInboundSyncFailed(ctx, taskUUID, "GitHub comment sync failed: task not found")
		return nil
	}

	userInfo := &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: commenterUUID},
		UserDgraphInfo:   *userDgraphInfo,
	}
	commentInput := &adapter.CreateOrUpdateTaskCommentInput{
		CommentBody:    event.Comment.Body,
		TaskUuid:       taskUUIDStr,
		SkipGitHubSync: true,
	}
	commentRes, err := taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTaskInfo, userInfo, commentInput, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/handleIssueCommentEvent Failed to create comment err: %+v", err)
		markInboundSyncFailed(ctx, taskUUID, "GitHub comment sync failed: could not create comment")
		return nil
	}

	if commentRes != nil && commentRes.Uuid != "" {
		commentUUID, _ := uuid.Parse(commentRes.Uuid)
		if commentUUID != uuid.Nil {
			_ = githubCommentMappingDomain.UpsertGitHubCommentMapping(ctx, event.Repository.Owner.Login, event.Repository.Name, int64(event.Comment.ID), commentUUID, taskUUID)
		}
	}

	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "comment",
		&event.Comment.User.Login, userDgraphInfo.ProfileKey, &userDgraphInfo.EmailID, nil, &event.Comment.Body, nil)

	if dgraphTaskInfo.Project != nil {
		publishGitHubSyncMqtt(taskUUIDStr, dgraphTaskInfo.Project.Uuid, "comment", nil)
	}
	return nil
}

func handleIssueCommentEdited(ctx context.Context, event struct {
	Action  string `json:"action"`
	Comment struct {
		ID        int    `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}, taskUUID uuid.UUID, taskUUIDStr string) error {
	commentUUIDStr, _ := githubCommentMappingDomain.GetGitHubCommentUUID(ctx, event.Repository.Owner.Login, event.Repository.Name, int64(event.Comment.ID))
	if commentUUIDStr == "" {
		helpers.MessageLogs.InfoLog.Printf("GitHub comment %d edited but no mapping found — skipping", event.Comment.ID)
		return nil
	}
	commentUUID, err := uuid.Parse(commentUUIDStr)
	if err != nil {
		return nil
	}

	oneCampUserUUID := MapGitHubUserToOneCampUser(event.Comment.User.Login)
	var userDgraphInfo *dgraphStruct.DgraphUser
	if oneCampUserUUID != "" {
		userDgraphInfo, _ = userBusiness.GetDgraphUserInfoByUUID(ctx, oneCampUserUUID)
	}
	if userDgraphInfo == nil {
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
		if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.CreatedBy == nil {
			return nil
		}
		userDgraphInfo, _ = userBusiness.GetDgraphUserInfoByUUID(ctx, dgraphTaskInfo.CreatedBy.Uuid)
	}
	if userDgraphInfo == nil {
		return nil
	}

	dgraphComment, err := businessComment.GetDgraphTaskCommentInfoByUUID(ctx, commentUUIDStr, userDgraphInfo.Uid)
	if err != nil || dgraphComment == nil {
		helpers.LogErrorWithContext(ctx, "handleIssueCommentEdited: comment %s not found: %v", commentUUIDStr, err)
		return nil
	}

	commentInput := &adapter.CreateOrUpdateTaskCommentInput{
		CommentBody:    event.Comment.Body,
		TaskUuid:       taskUUIDStr,
		SkipGitHubSync: true,
	}
	if err := taskBusiness.UpdateTaskCommentBody(ctx, commentUUID, commentInput, dgraphComment, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "handleIssueCommentEdited: failed to update comment %s: %v", commentUUIDStr, err)
		markInboundSyncFailed(ctx, taskUUID, "GitHub comment edit sync failed")
		return nil
	}

	helpers.MessageLogs.InfoLog.Printf("Updated comment %s from GitHub edit on comment %d", commentUUIDStr, event.Comment.ID)

	dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
	if dgraphTaskInfo != nil && dgraphTaskInfo.Project != nil {
		publishGitHubSyncMqtt(taskUUIDStr, dgraphTaskInfo.Project.Uuid, "comment_edited", map[string]interface{}{
			"comment_uuid": commentUUIDStr,
			"body":         event.Comment.Body,
		})
	}
	return nil
}

func handleIssueCommentDeleted(ctx context.Context, event struct {
	Action  string `json:"action"`
	Comment struct {
		ID        int    `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Issue struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		Name string `json:"name"`
	} `json:"repository"`
}, taskUUID uuid.UUID, taskUUIDStr string) error {
	commentUUIDStr, _ := githubCommentMappingDomain.GetGitHubCommentUUID(ctx, event.Repository.Owner.Login, event.Repository.Name, int64(event.Comment.ID))
	if commentUUIDStr == "" {
		helpers.MessageLogs.InfoLog.Printf("GitHub comment %d deleted but no mapping found — skipping", event.Comment.ID)
		return nil
	}
	commentUUID, err := uuid.Parse(commentUUIDStr)
	if err != nil {
		return nil
	}

	oneCampUserUUID := MapGitHubUserToOneCampUser(event.Comment.User.Login)
	var userDgraphInfo *dgraphStruct.DgraphUser
	if oneCampUserUUID != "" {
		userDgraphInfo, _ = userBusiness.GetDgraphUserInfoByUUID(ctx, oneCampUserUUID)
	}
	if userDgraphInfo == nil {
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
		if err != nil || dgraphTaskInfo == nil || dgraphTaskInfo.CreatedBy == nil {
			return nil
		}
		userDgraphInfo, _ = userBusiness.GetDgraphUserInfoByUUID(ctx, dgraphTaskInfo.CreatedBy.Uuid)
	}
	if userDgraphInfo == nil {
		return nil
	}

	dgraphComment, err := businessComment.GetDgraphTaskCommentInfoByUUID(ctx, commentUUIDStr, userDgraphInfo.Uid)
	if err != nil || dgraphComment == nil {
		helpers.LogErrorWithContext(ctx, "handleIssueCommentDeleted: comment %s not found: %v", commentUUIDStr, err)
		return nil
	}

	if err := taskBusiness.ArchiveCommentByCommentUUID(ctx, commentUUID, dgraphComment); err != nil {
		helpers.LogErrorWithContext(ctx, "handleIssueCommentDeleted: failed to delete comment %s: %v", commentUUIDStr, err)
		markInboundSyncFailed(ctx, taskUUID, "GitHub comment delete sync failed")
		return nil
	}

	helpers.MessageLogs.InfoLog.Printf("Deleted comment %s from GitHub delete on comment %d", commentUUIDStr, event.Comment.ID)

	dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
	if dgraphTaskInfo != nil && dgraphTaskInfo.Project != nil {
		publishGitHubSyncMqtt(taskUUIDStr, dgraphTaskInfo.Project.Uuid, "comment_deleted", map[string]interface{}{
			"comment_uuid": commentUUIDStr,
		})
	}
	return nil
}

// handleCreateEvent handles GitHub branch/tag creation events.
// Instead of blindly assigning the branch to all tasks, we only link it to a
// task when the branch name contains a recognisable task UUID or already
// matches a task's github_branch.
func handleCreateEvent(ctx context.Context, body []byte) error {
	var event struct {
		Ref          string `json:"ref"`
		RefType      string `json:"ref_type"`
		MasterBranch string `json:"master_branch"`
		Repository   struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return nil
	}

	if event.RefType != "branch" {
		return nil
	}

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil || !link.SyncPRs {
		return nil
	}

	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")

	// 1. Try to find an existing task already linked to this branch
	taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubBranch(ctx, branchName)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/handleCreateEvent Failed to find task by branch %s: %v", branchName, err)
	}

	// 2. If no direct match, try extracting a task UUID from the branch name
	if taskUUIDStr == "" {
		taskUUIDStr = findTaskUUIDInText(ctx, branchName)
	}

	if taskUUIDStr != "" {
		taskUUID, parseErr := uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleCreateEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
		} else {
			if err := taskDomain.SetGitHubBranchOnTaskByTaskID(ctx, branchName, taskUUID); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleCreateEvent Failed to set branch on task %s: %v", taskUUIDStr, err)
			} else {
				helpers.MessageLogs.InfoLog.Printf("Linked new branch %s to task %s in project %s", branchName, taskUUIDStr, link.ProjectId.String())
				publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "branch_created", map[string]interface{}{
					"branch": branchName,
				})
			}
		}
	} else {
		helpers.MessageLogs.InfoLog.Printf("New branch %s on %s — no matching task found in project %s", branchName, event.Repository.FullName, link.ProjectId.String())
	}

	return nil
}

// revokeGitHubToken revokes the OAuth token (best-effort, fire-and-forget).
func revokeGitHubToken(ctx context.Context, accessToken string) {
	appCfg := GetGitHubAppConfig(ctx)
	clientId := appCfg.ClientID
	clientSecret := appCfg.ClientSecret
	if clientId == "" || clientSecret == "" {
		return
	}

	url := fmt.Sprintf("https://api.github.com/applications/%s/token", clientId)
	body := fmt.Sprintf(`{"access_token":"%s"}`, accessToken)
	req, err := http.NewRequest(http.MethodDelete, url, strings.NewReader(body))
	if err != nil {
		return
	}
	req.SetBasicAuth(clientId, clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// VerifyGitHubWebhookSignature verifies the HMAC-SHA256 signature using per-link secrets.
// Returns true and the matching link if verified. Falls back to global GITHUB_WEBHOOK_SECRET.
func VerifyGitHubWebhookSignature(ctx context.Context, body []byte, signatureHeader, eventType string) (bool, *githubLinkModel.GitHubLink) {
	if !strings.HasPrefix(signatureHeader, "sha256=") {
		return false, nil
	}
	sig := strings.TrimPrefix(signatureHeader, "sha256=")

	repoOwner, repoName := extractRepoFromEvent(body)
	if repoOwner != "" && repoName != "" {
		link, _ := lookupGitHubLinkByRepo(ctx, repoOwner, repoName)
		if link != nil && link.WebhookSecret != nil && *link.WebhookSecret != "" {
			if verifyHMACSignature(*link.WebhookSecret, body, sig) {
				return true, link
			}
		}
	}

	globalSecret := GetGitHubAppConfig(ctx).WebhookSecret
	if globalSecret != "" && verifyHMACSignature(globalSecret, body, sig) {
		return true, nil
	}

	return false, nil
}

func verifyHMACSignature(secret string, payload []byte, sig string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expectedSig))
}

func extractRepoFromEvent(body []byte) (string, string) {
	var event struct {
		Repository struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return "", ""
	}
	return event.Repository.Owner.Login, event.Repository.Name
}

func boolPtr(b bool) *bool { return &b }

func getGitHubWebhookBaseURL() string {
	if host := os.Getenv("BACKEND_DOMAIN"); host != "" {
		return fmt.Sprintf("https://%s", host)
	}
	return "https://localhost:3000"
}

func registerGitHubWebhook(link *githubLinkModel.GitHubLink, secret string) {
	ctx := context.Background()
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: no access token, skip for %s/%s", link.RepoOwner, link.RepoName)
		return
	}

	webhookURL := getGitHubWebhookBaseURL() + "/integration/github/webhook"

	desiredEvents := []string{"issues", "pull_request", "push", "issue_comment", "create", "pull_request_review", "check_run"}

	// Idempotency: check if our webhook already exists
	listURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks?per_page=100", link.RepoOwner, link.RepoName)
	listReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	listReq.Header.Set("Accept", "application/vnd.github+json")
	listResp, err := client.Do(listReq)
	if err != nil {
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: failed to list hooks for %s/%s: %v", link.RepoOwner, link.RepoName, err)
	} else {
		defer listResp.Body.Close()
		if listResp.StatusCode == http.StatusOK {
			var hooks []struct {
				ID     int      `json:"id"`
				Active bool     `json:"active"`
				Events []string `json:"events"`
				Config struct {
					URL string `json:"url"`
				} `json:"config"`
			}
			if json.NewDecoder(listResp.Body).Decode(&hooks) == nil {
				for _, hook := range hooks {
					if hook.Config.URL == webhookURL {
						needsUpdate := false

						// Check events list
						for _, ev := range desiredEvents {
							found := false
							for _, he := range hook.Events {
								if he == ev {
									found = true
									break
								}
							}
							if !found {
								needsUpdate = true
								break
							}
						}
						if !hook.Active {
							needsUpdate = true
						}

						helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: existing webhook (id=%d) events=%v active=%v needsUpdate=%v for %s/%s", hook.ID, hook.Events, hook.Active, needsUpdate, link.RepoOwner, link.RepoName)

						// Always PATCH config.secret to ensure it matches our current link secret.
						// Also always PATCH events so the list stays fresh (GitHub may drop events).
						patchPayload := map[string]interface{}{
							"active": true,
							"events": desiredEvents,
							"config": map[string]interface{}{
								"url":          webhookURL,
								"content_type": "json",
								"secret":       secret,
								"insecure_ssl": "0",
							},
						}
						patchBody, _ := json.Marshal(patchPayload)
						patchURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks/%d", link.RepoOwner, link.RepoName, hook.ID)
						patchReq, _ := http.NewRequestWithContext(ctx, http.MethodPatch, patchURL, bytes.NewReader(patchBody))
						patchReq.Header.Set("Accept", "application/vnd.github+json")
						patchReq.Header.Set("Content-Type", "application/json")
						patchResp, pErr := client.Do(patchReq)
						if patchResp != nil {
							defer patchResp.Body.Close()
						}
						if pErr == nil && patchResp.StatusCode == http.StatusOK {
							if needsUpdate {
								helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: updated events+secret+active for existing webhook (id=%d) for %s/%s", hook.ID, link.RepoOwner, link.RepoName)
							} else {
								helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: updated secret for existing webhook (id=%d) for %s/%s", hook.ID, link.RepoOwner, link.RepoName)
							}
						} else if patchResp != nil {
							pb, _ := io.ReadAll(io.LimitReader(patchResp.Body, 512))
							helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: failed to patch webhook (id=%d) for %s/%s: %d %s", hook.ID, link.RepoOwner, link.RepoName, patchResp.StatusCode, string(pb))
						}
						return
					}
				}
			}
		}
	}

	payload := map[string]interface{}{
		"name":   "web",
		"active": true,
		"events": []string{"issues", "pull_request", "push", "issue_comment", "create", "pull_request_review", "check_run"},
		"config": map[string]interface{}{
			"url":          webhookURL,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks", link.RepoOwner, link.RepoName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: failed for %s/%s: %v", link.RepoOwner, link.RepoName, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: registered for %s/%s", link.RepoOwner, link.RepoName)
	} else if resp.StatusCode == http.StatusUnprocessableEntity {
		// 422 = webhook already exists (race condition between our check and create)
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: webhook already exists (422) for %s/%s", link.RepoOwner, link.RepoName)
	} else {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		helpers.MessageLogs.InfoLog.Printf("registerGitHubWebhook: error %d for %s/%s: %s", resp.StatusCode, link.RepoOwner, link.RepoName, string(respBody))
	}
}

func deleteGitHubWebhook(link *githubLinkModel.GitHubLink) {
	ctx := context.Background()
	// We resolve a fresh client (auto-refreshed) and use the account
	// token for all calls — the legacy deleteGitHubWebhookWithToken
	// took a string for the disconnect path where we still have the
	// old token in hand even after the integration row is gone.
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return
	}
	deleteGitHubWebhookWithClient(ctx, link, client)
}

// deleteGitHubWebhookWithToken is kept for the disconnect path where
// the integration row is being deleted and we still have the raw
// access token in hand. It does NOT auto-refresh because at this point
// the row is about to disappear anyway.
func deleteGitHubWebhookWithToken(link *githubLinkModel.GitHubLink, accessToken string) {
	webhookURL := getGitHubWebhookBaseURL() + "/integration/github/webhook"
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks?per_page=100", link.RepoOwner, link.RepoName)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var hooks []struct {
		Id     int `json:"id"`
		Config struct {
			Url string `json:"url"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hooks); err != nil {
		return
	}

	for _, hook := range hooks {
		if hook.Config.Url == webhookURL {
			deleteURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks/%d", link.RepoOwner, link.RepoName, hook.Id)
			deleteReq, _ := http.NewRequest(http.MethodDelete, deleteURL, nil)
			deleteReq.Header.Set("Authorization", "Bearer "+accessToken)
			deleteReq.Header.Set("Accept", "application/vnd.github+json")
			deleteClient := githubHTTPClient
			deleteResp, err := deleteClient.Do(deleteReq)
			if err == nil {
				deleteResp.Body.Close()
			}
			if err == nil && (deleteResp.StatusCode == http.StatusNoContent || deleteResp.StatusCode == http.StatusOK) {
				helpers.MessageLogs.InfoLog.Printf("deleteGitHubWebhook: deleted webhook %d for %s/%s", hook.Id, link.RepoOwner, link.RepoName)
			}
			break
		}
	}
}

// deleteGitHubWebhookWithClient is the auto-refreshing path used by
// the per-link unlink flow. The client carries Authorization itself.
func deleteGitHubWebhookWithClient(ctx context.Context, link *githubLinkModel.GitHubLink, client *http.Client) {
	webhookURL := getGitHubWebhookBaseURL() + "/integration/github/webhook"
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks?per_page=100", link.RepoOwner, link.RepoName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var hooks []struct {
		Id     int `json:"id"`
		Config struct {
			Url string `json:"url"`
		} `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hooks); err != nil {
		return
	}

	for _, hook := range hooks {
		if hook.Config.Url == webhookURL {
			deleteURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/hooks/%d", link.RepoOwner, link.RepoName, hook.Id)
			deleteReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
			deleteReq.Header.Set("Accept", "application/vnd.github+json")
			deleteResp, err := client.Do(deleteReq)
			if err == nil {
				deleteResp.Body.Close()
				if deleteResp.StatusCode == http.StatusNoContent || deleteResp.StatusCode == http.StatusOK {
					helpers.MessageLogs.InfoLog.Printf("deleteGitHubWebhook: deleted webhook %d for %s/%s", hook.Id, link.RepoOwner, link.RepoName)
				}
			}
			break
		}
	}
}

// taskUUIDRegex matches OneCamp task URLs to extract UUIDs.
var taskUUIDRegex = regexp.MustCompile(`task/([0-9a-fA-F\-]{36})`)

// findTaskUUIDInText scans text for OneCamp task URLs and returns the first matching task UUID.
func findTaskUUIDInText(ctx context.Context, text string) string {
	if text == "" {
		return ""
	}
	matches := taskUUIDRegex.FindStringSubmatch(text)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func handlePushEvent(ctx context.Context, body []byte) error {
	var event struct {
		Ref     string `json:"ref"`
		Commits []struct {
			Message string `json:"message"`
			Author  struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
			URL string `json:"url"`
			SHA string `json:"id"`
		} `json:"commits"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Pusher struct {
			Name string `json:"name"`
		} `json:"pusher"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse push event: %v", err)
	}

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil || !link.SyncPRs {
		return nil
	}

	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")
	taskUUIDStr, branchErr := taskDomain.FindTaskUUIDByGitHubBranch(ctx, branchName)
	if branchErr != nil {
		helpers.LogErrorWithContext(ctx, "business/handlePushEvent Failed to find task by branch %s: %v", branchName, branchErr)
	}

	// Magic words: parse commit messages for task references
	for _, commit := range event.Commits {
		refs := parseCommitMessageForTaskRefs(commit.Message)
		for _, ref := range refs {
			if ref.TaskUUID == "" {
				continue
			}
			refTaskUUID, parseErr := uuid.Parse(ref.TaskUUID)
			if parseErr != nil {
				helpers.LogErrorWithContext(ctx, "business/handlePushEvent Invalid ref UUID %s: %v", ref.TaskUUID, parseErr)
				continue
			}
			go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, refTaskUUID, "commit_linked",
				&event.Sender.Login, nil, nil, &commit.Message, &commit.URL, nil)
			if ref.Action == "close" || ref.Action == "fix" {
				applyAutomationRules(ctx, link, ref.TaskUUID, "commit_linked")
			}
			publishGitHubSyncMqtt(ref.TaskUUID, link.ProjectId.String(), "commit_linked", nil)
		}
	}

	if taskUUIDStr != "" {
		taskUUID, parseErr := uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handlePushEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
		} else {
			for _, commit := range event.Commits {
				go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "commit_pushed",
					&event.Sender.Login, nil, nil, &commit.Message, &commit.URL, nil)
			}
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "commit_pushed", nil)
		}
	}

	return nil
}

// magicWordPatterns maps closing keywords to their action type.
var magicWordPatterns = []struct {
	regex  *regexp.Regexp
	action string
}{
	{regexp.MustCompile(`task/([0-9a-fA-F\-]{36})`), "ref"},
}

type commitTaskRef struct {
	TaskUUID string
	Action   string
}

// parseCommitMessageForTaskRefs extracts task UUID references from a commit message.
// Only task/UUID patterns are included; issue numbers are skipped because we cannot
// reliably map them to OneCamp tasks without the linked repo context.
func parseCommitMessageForTaskRefs(msg string) []commitTaskRef {
	var refs []commitTaskRef
	seen := make(map[string]bool)
	for _, pattern := range magicWordPatterns {
		matches := pattern.regex.FindAllStringSubmatch(msg, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			ref := m[1]
			// Validate that the capture group is a valid UUID (skip plain issue numbers like "123")
			if _, err := uuid.Parse(ref); err != nil {
				continue
			}
			key := ref + "-" + pattern.action
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, commitTaskRef{TaskUUID: ref, Action: pattern.action})
		}
	}
	return refs
}

func handlePullRequestReviewEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action string `json:"action"`
		Review struct {
			State       string `json:"state"`
			Body        string `json:"body"`
			HTMLURL     string `json:"html_url"`
			SubmittedAt string `json:"submitted_at"`
			User        struct {
				Login     string `json:"login"`
				AvatarURL string `json:"avatar_url"`
				HTMLURL   string `json:"html_url"`
			} `json:"user"`
		} `json:"review"`
		PullRequest struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse PR review event: %v", err)
	}

	if event.Action != "submitted" {
		return nil
	}

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil || !link.SyncPRs {
		return nil
	}

	// Fan out to the workspace event bus so a "PR-follow" agent (an event-trigger
	// agent subscribed to github.pr.review_submitted) can react when a review
	// lands — post an update, ping the author, etc. Decoupled via the bus so
	// this package never imports the AI/agent packages (loop-safe: agent writes
	// are workflow-tagged and never re-enter here). Fires for any review on a
	// linked repo, whether or not the PR is tied to a OneCamp task.
	webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.pr.review_submitted", map[string]interface{}{
		"owner":        event.Repository.Owner.Login,
		"repo":         event.Repository.Name,
		"pr_number":    event.PullRequest.Number,
		"pr_url":       event.PullRequest.HTMLURL,
		"review_state": strings.ToUpper(event.Review.State),
		"reviewer":     event.Review.User.Login,
		"body":         event.Review.Body,
	})

	taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubBranch(ctx, event.PullRequest.Head.Ref)
	if err != nil || taskUUIDStr == "" {
		taskUUIDStr, _ = taskDomain.FindTaskUUIDByPRURL(ctx, event.PullRequest.HTMLURL)
	}
	if taskUUIDStr == "" {
		return nil
	}

	taskUUID, _ := uuid.Parse(taskUUIDStr)

	// Upsert the review record
	if err := githubPRReviewDomain.UpsertGitHubPRReview(ctx, taskUUID, event.Review.User.Login, event.Review.User.AvatarURL, event.Review.User.HTMLURL, strings.ToUpper(event.Review.State), time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/handlePullRequestReviewEvent Failed to upsert review: %v", err)
		markInboundSyncFailed(ctx, taskUUID, "GitHub PR review sync failed")
	}

	// Update aggregate review state on task
	updateTaskReviewState(ctx, taskUUID)

	activityType := "pr_review_" + strings.ToLower(event.Review.State)
	go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
		&event.Review.User.Login, nil, nil, nil, nil, &event.Review.Body)

	trigger := ""
	switch strings.ToLower(event.Review.State) {
	case "changes_requested":
		trigger = "changes_requested"
	case "approved":
		trigger = "approved"
	}
	if trigger != "" {
		applyAutomationRules(ctx, link, taskUUIDStr, trigger)
	}

	publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), activityType, nil)
	return nil
}

func updateTaskReviewState(ctx context.Context, taskUUID uuid.UUID) {
	reviews, err := githubPRReviewDomain.GetGitHubPRReviewsByTaskId(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/updateTaskReviewState Failed to get reviews: %v", err)
		return
	}
	if len(reviews) == 0 {
		return
	}

	// Aggregate review state: if any changes_requested, that's the state; else if any approved, approved; else commented
	aggregateState := "commented"
	for _, r := range reviews {
		if r.ReviewState == "CHANGES_REQUESTED" {
			aggregateState = "changes_requested"
			break
		}
		if r.ReviewState == "APPROVED" {
			aggregateState = "approved"
		}
	}

	if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, "", "", aggregateState, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "business/updateTaskReviewState Failed to update review state: %v", err)
	}
}

func handleCheckRunEvent(ctx context.Context, body []byte) error {
	var event struct {
		Action   string `json:"action"`
		CheckRun struct {
			Name         string `json:"name"`
			Status       string `json:"status"`
			Conclusion   string `json:"conclusion"`
			HTMLURL      string `json:"html_url"`
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int    `json:"number"`
				URL    string `json:"url"`
			} `json:"pull_requests"`
		} `json:"check_run"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse check_run event: %v", err)
	}

	if event.Action != "completed" {
		return nil
	}

	link, err := lookupGitHubLinkByRepo(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil || link == nil || !link.SyncPRs {
		return nil
	}

	checkStatus := event.CheckRun.Conclusion
	if checkStatus == "" {
		checkStatus = event.CheckRun.Status
	}

	// Use pull_requests array to find the exact task(s) linked to this PR
	if len(event.CheckRun.PullRequests) == 0 {
		helpers.MessageLogs.InfoLog.Printf("check_run event for %s has no linked PRs, skipping", event.Repository.FullName)
		return nil
	}

	// PR-follow event: only ONCE per PR when ALL checks for this head commit
	// have concluded (not per individual check), with the aggregate result — so
	// a follow agent reacts a single time with "CI passed/failed", never N times
	// for N checks. Deduped per (repo, sha, PR). See maybeEmitCIConcluded.
	prNumbers := make([]int, 0, len(event.CheckRun.PullRequests))
	for _, pr := range event.CheckRun.PullRequests {
		prNumbers = append(prNumbers, pr.Number)
	}
	maybeEmitCIConcluded(ctx, event.Repository.Owner.Login, event.Repository.Name, event.CheckRun.HeadSHA, prNumbers)

	for _, pr := range event.CheckRun.PullRequests {
		prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", event.Repository.Owner.Login, event.Repository.Name, pr.Number)
		taskUUIDStr, err := taskDomain.FindTaskUUIDByPRURL(ctx, prURL)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleCheckRunEvent Failed to find task for PR %s: %v", prURL, err)
			continue
		}
		if taskUUIDStr == "" {
			// Fallback: try finding by branch pattern if task was linked via branch
			taskUUIDStr, _ = taskDomain.FindTaskUUIDByGitHubBranch(ctx, fmt.Sprintf("pull/%d/head", pr.Number))
			if taskUUIDStr == "" {
				continue
			}
		}
		taskUUID, parseErr := uuid.Parse(taskUUIDStr)
		if parseErr != nil {
			helpers.LogErrorWithContext(ctx, "business/handleCheckRunEvent Invalid task UUID %s: %v", taskUUIDStr, parseErr)
			continue
		}
		if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, "", checkStatus, "", nil); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleCheckRunEvent Failed to update task PR state: %v", err)
			markInboundSyncFailed(ctx, taskUUID, "GitHub check run sync failed")
		}
		publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "check_run", map[string]interface{}{
			"check_status": checkStatus,
		})
		helpers.MessageLogs.InfoLog.Printf("Updated check status '%s' for task %s from check_run on PR #%d", checkStatus, taskUUIDStr, pr.Number)
	}

	return nil
}

// ciConcludedDedup ensures we emit the aggregate "CI concluded" workspace event
// at most once per (repo, head sha, PR) within the TTL, even if two checks
// finish near-simultaneously and both observe "all complete". A re-run of CI on
// the same commit after the TTL legitimately re-emits.
var ciConcludedDedup = helpers.NewTTLCache[bool](15 * time.Minute)

// ciConclusion is the pure aggregate of a commit's check runs: whether every
// run has completed, and the roll-up counts + overall conclusion. Kept pure
// (no I/O) so the noisy-vs-clean decision is unit-testable.
type ciConclusion struct {
	AllComplete bool
	Total       int
	Passed      int
	Failed      int
	Conclusion  string // "success" | "failure" (only meaningful when AllComplete)
}

// aggregateCheckRuns rolls up a commit's check runs. A run still queued/in
// progress means CI is not done (AllComplete=false). A GitHub conclusion of
// failure/timed_out/cancelled/action_required/stale/startup_failure counts as a
// failure; success/neutral/skipped count as passed. Pure + DB-free.
func aggregateCheckRuns(runs []githubCheckRun) ciConclusion {
	out := ciConclusion{AllComplete: true, Conclusion: "success"}
	for _, r := range runs {
		if strings.ToLower(strings.TrimSpace(r.Status)) != "completed" {
			out.AllComplete = false
			return out
		}
		out.Total++
		switch strings.ToLower(strings.TrimSpace(r.Conclusion)) {
		case "failure", "timed_out", "cancelled", "action_required", "stale", "startup_failure":
			out.Failed++
		default: // success, neutral, skipped, or unknown-but-completed
			out.Passed++
		}
	}
	if out.Failed > 0 {
		out.Conclusion = "failure"
	}
	return out
}

// maybeEmitCIConcluded emits ONE github.check_run.completed workspace event per
// linked PR — the moment ALL checks for a head commit have concluded — carrying
// the aggregate result, so a PR-follow agent reacts once with "CI passed/failed"
// instead of once per individual check. Best-effort: a lookup/API miss simply
// means no aggregate event this round (the per-check task sync still ran). It
// costs one check-runs API read per completed check on a PR-syncing repo, and
// the dedup guarantees a single emit per commit.
func maybeEmitCIConcluded(ctx context.Context, owner, repo, headSHA string, prNumbers []int) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" || len(prNumbers) == 0 {
		return
	}
	token, err := GitHubAccessToken(ctx)
	if err != nil || strings.TrimSpace(token) == "" {
		return
	}
	runs, err := fetchGitHubCheckRuns(ctx, owner, repo, headSHA, token)
	if err != nil || len(runs) == 0 {
		return
	}
	agg := aggregateCheckRuns(runs)
	if !agg.AllComplete {
		return // CI still running — wait for the last check
	}
	for _, num := range prNumbers {
		dedupKey := fmt.Sprintf("%s/%s@%s#%d", owner, repo, headSHA, num)
		if _, seen := ciConcludedDedup.Get(dedupKey); seen {
			continue
		}
		ciConcludedDedup.Set(dedupKey, true)
		prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, num)
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.check_run.completed", map[string]interface{}{
			"owner":         owner,
			"repo":          repo,
			"pr_number":     num,
			"pr_url":        prURL,
			"conclusion":    agg.Conclusion,
			"checks_total":  agg.Total,
			"checks_passed": agg.Passed,
			"checks_failed": agg.Failed,
		})
	}
}

// publishGitHubSyncMqtt safely publishes a real-time sync notification to the project MQTT topic.
// payload carries the actual changed field values so the frontend can update Redux directly
// without triggering an SWR revalidation / network request.
func publishGitHubSyncMqtt(taskUUID string, projectUUID string, syncType string, payload map[string]interface{}) {
	if taskUUID == "" || projectUUID == "" {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.MessageLogs.InfoLog.Printf("publishGitHubSyncMqtt panic recovered: %v", rec)
			}
		}()
		mqttBusiness.PublishGitHubSync(&mqttStruct.MqttGitHubSync{
			TaskUuid:    taskUUID,
			SyncType:    syncType,
			ProjectUuid: projectUUID,
			Payload:     payload,
		}, projectUUID)
	}()
}

// CreatePullRequestForTask creates a draft PR on GitHub for a linked task.
func CreatePullRequestForTask(ctx context.Context, taskUUID uuid.UUID, title, body, headBranch string) (*struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}, error) {
	// 1. Find linked repo via project
	taskInfo, err := taskDomain.GetTaskProjectID(ctx, taskUUID)
	if err != nil {
		return nil, fmt.Errorf("failed to get task project: %w", err)
	}
	if taskInfo == uuid.Nil {
		return nil, fmt.Errorf("task has no project")
	}

	links, err := githubLinkModel.GetGitHubLinksByProjectId(`
		SELECT id, project_id, repo_owner, repo_name, installation_id, webhook_secret, sync_issues, sync_prs, auto_create_tasks, default_task_status, label_mapping, automation_rules, branch_format, created_by, created_at, updated_at, deleted_at
		FROM github_links WHERE project_id = $1 AND deleted_at IS NULL
	`, taskInfo)
	if err != nil || len(links) == 0 {
		return nil, fmt.Errorf("no linked repo for project: %w", err)
	}
	link := links[0]

	// 2. Get GitHub token (auto-refreshed)
	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return nil, fmt.Errorf("GitHub not connected")
	}
	token, tokErr := GitHubAccessToken(ctx)
	if tokErr != nil {
		token = *integration.AccessToken
	}

	// 3. Get default branch
	defaultBranch, err := getGitHubDefaultBranch(link.RepoOwner, link.RepoName, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get default branch: %w", err)
	}

	// 4. Create draft PR
	prBody := body
	if prBody == "" {
		prBody = "_No description provided._"
	}
	payload := map[string]interface{}{
		"title": title,
		"body":  prBody,
		"head":  headBranch,
		"base":  defaultBranch,
		"draft": true,
	}
	bodyBytes, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", link.RepoOwner, link.RepoName)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub response: %w", err)
	}

	return &result, nil
}

func getGitHubDefaultBranch(owner, repo, token string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GitHub API error %d", resp.StatusCode)
	}

	var result struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if result.DefaultBranch == "" {
		return "main", nil
	}
	return result.DefaultBranch, nil
}

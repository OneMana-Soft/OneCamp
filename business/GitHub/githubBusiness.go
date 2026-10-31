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
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	githubTaskActivityModel "github.com/akashc777/OneCamp/models/postgres/GitHubTaskActivity"
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

	err := githubLinkModel.CreateGitHubLink(ctx, id, input.ProjectId, input.RepoOwner, input.RepoName, nil, &secret, input.SyncIssues, input.SyncPRs, input.AutoCreateTasks, createdBy, now)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("this repository is already linked to this project")
		}
		return nil, err
	}

	// Fetch and return the created link
	link, err := githubLinkModel.GetGitHubLinkById(ctx, id, false)
	if err != nil {
		return nil, err
	}

	// Invalidate the cached "no link for this repo" entry produced by
	// any earlier delivery from the same repo before linking.
	invalidateLinkByRepo(input.RepoOwner, input.RepoName)

	helpers.GoSafeNamed("registerGitHubWebhook", func() { registerGitHubWebhook(link, secret) })

	return link, nil
}

// UnlinkRepository soft-deletes a link, unlinks the project's tasks from that
// repository, and deletes the repository's webhook unless another project
// still links it.
func UnlinkRepository(ctx context.Context, linkId uuid.UUID) error {
	// Get the link first
	link, err := githubLinkModel.GetGitHubLinkById(ctx, linkId, true)
	if err != nil {
		return err
	}
	if link == nil {
		return fmt.Errorf("link not found")
	}

	now := time.Now()

	// 1. Soft-delete the link
	err = githubLinkModel.SoftDeleteGitHubLink(ctx, now, linkId)
	if err != nil {
		return fmt.Errorf("failed to delete link: %v", err)
	}

	// 2. Unlink the project's tasks from the repository by the rule every start
	// applies (RepairTaskLinks), to this project's tasks alone: a task keeps an
	// issue or pull request only in a repository its own project links. A task
	// linked by its pull request alone is unlinked too, and an issue or pull
	// request in another of the project's repositories stays. One that can't be
	// unlinked now is never synced (syncAllowed) and is unlinked at the next
	// start.
	if _, err := repairTaskLinks(ctx, link.ProjectId); err != nil {
		helpers.LogErrorWithContext(ctx, "business/UnlinkRepository couldn't unlink every task from %s/%s, the next start tries again: %+v", link.RepoOwner, link.RepoName, err)
	}

	// 3. The repository has one webhook, whichever projects link it, signed
	// with the secret of the link that registered it last, which may be this
	// one. The last link to go takes the webhook with it. While another
	// remains, it stays, signed again with the oldest remaining link's secret
	// (webhookSigners) so their deliveries still verify
	// (VerifyGitHubWebhookSignature). Synchronous, so the operation is durable
	// when this returns.
	remaining, err := githubLinkModel.GetGitHubLinksByRepo(ctx, link.RepoOwner, link.RepoName)
	switch {
	case err != nil:
		helpers.LogErrorWithContext(ctx, "business/UnlinkRepository couldn't read the other links to %s/%s, so its webhook stays: %+v", link.RepoOwner, link.RepoName, err)
	case len(remaining) == 0:
		deleteGitHubWebhook(link)
	default:
		if signers := webhookSigners(remaining); len(signers) > 0 {
			registerGitHubWebhook(signers[0], *signers[0].WebhookSecret)
		} else {
			helpers.LogErrorWithContext(ctx, "business/UnlinkRepository: no link still to %s/%s has a webhook secret, so its webhook keeps the one it had", link.RepoOwner, link.RepoName)
		}
	}

	// Invalidate the cached links for this repo so the next inbound
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
	err := githubLinkModel.SoftDeleteAllGitHubLinks(ctx, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DisconnectGitHub Failed to delete all links err: %+v", err)
	}

	// 3. Batch-clear ALL GitHub metadata from ALL tasks
	err = githubLinkModel.BatchClearAllGitHubFields(ctx)
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
	return githubLinkModel.GetGitHubLinksByProjectId(ctx, projectId)
}

// GetAllLinkedRepos returns all active GitHub links.
func GetAllLinkedRepos(ctx context.Context) ([]*githubLinkModel.GitHubLink, error) {
	return githubLinkModel.GetAllGitHubLinks(ctx)
}

// GetGitHubLinkById returns a link by its ID.
func GetGitHubLinkById(ctx context.Context, linkId uuid.UUID) (*githubLinkModel.GitHubLink, error) {
	return githubLinkModel.GetGitHubLinkById(ctx, linkId, true)
}

// HandleGitHubWebhookEvent processes an incoming GitHub webhook event.
func HandleGitHubWebhookEvent(ctx context.Context, eventType string, body []byte) error {
	// Everything below applies a change that came FROM GitHub, through the same
	// business functions a person's edit goes through, and those enqueue an
	// outbound sync. Marking the context once here is what stops the value we
	// just received being PATCHed straight back onto the issue it came from.
	// business.EnqueueGitHubSync reads the marker.
	ctx = helpers.WithGitHubOrigin(ctx)
	// Each task the event names, and each of those tasks' projects, is read
	// once in it however often it is named (eventReads).
	ctx = withEventReads(ctx)

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
	case "delete":
		return handleDeleteEvent(ctx, body)
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
	taskUUID, err := uuid.Parse(taskUUIDStr)
	if err != nil {
		return
	}
	// A rule moves a task only in a project that linked the repository, by that
	// project's own link and rules, whoever calls (taskRepoLink.go).
	own, err := TaskRepoLink(ctx, taskUUID, link.RepoOwner, link.RepoName)
	if err != nil || own == nil {
		helpers.MessageLogs.InfoLog.Printf("github: a %s rule on %s/%s named task %s, which isn't in a project linked to it; left alone (err %v)",
			trigger, link.RepoOwner, link.RepoName, taskUUIDStr, err)
		return
	}
	link = own
	targetStatus := ruleTarget(parseAutomationRules(link), trigger)
	if targetStatus == "" {
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
	_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "status_synced",
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsIssues) == nil {
		helpers.MessageLogs.InfoLog.Printf("Skipping issue event for %s: no project's link to it syncs issues", event.Repository.FullName)
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

	// issueTask is the task linked to this issue and its own project's link to
	// the repository, which decides what the event does to the task and who is
	// told: several projects may link one repository.
	issueTask := func() (string, uuid.UUID, *githubLinkModel.GitHubLink, bool) {
		taskUUIDStr, err := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
		if err != nil || taskUUIDStr == "" {
			helpers.MessageLogs.InfoLog.Printf("No task found for issue URL: %s", event.Issue.HTMLURL)
			return "", uuid.Nil, nil, false
		}
		taskUUID, own, ok := eventTask(ctx, taskUUIDStr, event.Repository.Owner.Login, event.Repository.Name, syncsIssues)
		return taskUUIDStr, taskUUID, own, ok
	}

	switch event.Action {
	case "opened":
		// One task for the issue, made in the project of the first link (the
		// oldest) that syncs the repository's issues and makes tasks of new ones.
		link := linkSyncing(links, makesIssueTasks)
		if link == nil {
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
		// A sync is a data move: the issue's state is the task's status (DataMove).
		taskUUID, err := taskBusiness.CreateTask(taskBusiness.DataMove(ctx), link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, mentionsUsers)
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

		_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "issue_opened",
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

		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			return nil
		}

		// Try automation rule first, fall back to hardcoded default
		if own.AutomationRules != nil {
			var rules map[string]string
			if err := json.Unmarshal([]byte(*own.AutomationRules), &rules); err == nil {
				if s, ok := rules[trigger]; ok && s != "" && s != "_none" {
					applyAutomationRules(ctx, own, taskUUIDStr, trigger)
					return nil
				}
			}
		}

		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, own.CreatedBy.String())
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
		_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
			&event.Sender.Login, nil, nil, &event.Issue.Title, nil, nil)
		publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), activityType, map[string]interface{}{
			"status": defaultStatus,
		})

	case "edited":
		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			return nil
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, own.CreatedBy.String())
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
		publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "issue_edited", map[string]interface{}{
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
		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, assignedUUID)
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		oldAssigneeUID := ""
		if dgraphTaskInfo.Assignee != nil {
			oldAssigneeUID = dgraphTaskInfo.Assignee.Uid
		}
		if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(ctx), taskUUID, userDgraphInfo, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, userDgraphInfo); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update assignee: %v", err)
		}
		publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "assignee_synced", map[string]interface{}{
			"assignee_uuid":  userDgraphInfo.Uuid,
			"assignee_name":  userDgraphInfo.DisplayName(),
			"assignee_login": event.Issue.Assignee.Login,
		})

	case "unassigned":
		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, own.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
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
		publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "assignee_synced", map[string]interface{}{
			"assignee_uuid": "",
			"assignee_name": "",
		})

	case "labeled":
		if event.Label == nil || event.Label.Name == "" {
			break
		}
		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, own.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		// A label added on GitHub is one more tag on the task.
		current := labelOf(dgraphTaskInfo)
		if next := helpers.WithTag(current, event.Label.Name); next != current {
			if err := taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, next, dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to update label: %v", err)
			} else {
				publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "label_synced", map[string]interface{}{
					"label": next,
				})
			}
		}

	case "unlabeled":
		if event.Label == nil || event.Label.Name == "" {
			break
		}
		taskUUIDStr, taskUUID, own, ok := issueTask()
		if !ok {
			break
		}
		userDgraphInfo, err := userBusiness.GetDgraphUserInfoByUUID(ctx, own.CreatedBy.String())
		if err != nil || userDgraphInfo == nil {
			break
		}
		dgraphTaskInfo, err := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, userDgraphInfo.Uid)
		if err != nil || dgraphTaskInfo == nil {
			break
		}
		// A label removed on GitHub takes that tag off the task, and only that one.
		current := labelOf(dgraphTaskInfo)
		if next := helpers.WithoutTag(current, event.Label.Name); next != current {
			if err := taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, next, dgraphTaskInfo, userDgraphInfo); err != nil {
				helpers.LogErrorWithContext(ctx, "business/handleIssueEvent Failed to remove label: %v", err)
			} else {
				publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "label_synced", map[string]interface{}{
					"label": next,
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsPRs) == nil {
		helpers.MessageLogs.InfoLog.Printf("Skipping PR event for %s: no project's link to it syncs pull requests", event.Repository.FullName)
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

	// The task on the pull request's branch in a project linking this
	// repository, else one its body names (Linear-style magic linking). Only a
	// task in a project that linked this repository, through that project's
	// own link, which decides what the event does to the task and who is
	// told: the branch and the body are the author's words.
	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	taskUUIDStr, taskUUID, link, _ := leadTask(ctx, owner, repo, syncsPRs,
		onBranch(ctx, owner, repo, event.PullRequest.Head.Ref),
		func() string { return findTaskUUIDInText(ctx, event.PullRequest.Body) })

	// Determine PR state for tracking
	prState := event.PullRequest.State
	if event.PullRequest.Merged {
		prState = "merged"
	}
	isDraft := event.PullRequest.Draft

	// emitPRReview fans out an internal event so the AI agent (if enabled) can
	// review the PR diff and post feedback as a task comment. Decoupled via the
	// event bus so this package never imports the AI/code-agent packages. Draft
	// PRs are skipped (still a work in progress). link is the linked task's
	// project's link to the repository.
	emitPRReview := func(link *githubLinkModel.GitHubLink, linkedTaskUUID string) {
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
			"project_id": link.ProjectId.String(),
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
			_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
				&event.PullRequest.User.Login, nil, nil, &event.PullRequest.Title, nil, nil)
			publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "pr_opened", nil)
			trigger := "pr_opened"
			if isDraft {
				trigger = "pr_drafted"
			}
			applyAutomationRules(ctx, link, taskUUIDStr, trigger)
			helpers.MessageLogs.InfoLog.Printf("Linked PR #%d to task %s via branch %s", event.PullRequest.Number, taskUUIDStr, event.PullRequest.Head.Ref)
			emitPRReview(link, taskUUIDStr)
		} else if link := linkSyncing(links, makesPRTasks); link != nil {
			// Auto-create a task for the PR if no existing task is found, in the
			// project of the first link (the oldest) that syncs the repository's
			// pull requests and makes tasks of new ones.
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
					// A sync is a data move: the pull request's state is the task's status (DataMove).
					taskUUID, err := taskBusiness.CreateTask(taskBusiness.DataMove(ctx), link.ProjectId, userInfo, dgraphProjectInfo, nil, createTaskInput, mentionsUsers)
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
						_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "pr_opened",
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
						emitPRReview(link, taskUUID.String())
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
			_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
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
						_ = taskBusiness.UpdateTaskAssigneeByTaskUUID(taskBusiness.DataMove(ctx), taskUUID, assigneeDgraphInfo, oldAssigneeUID, dgraphTaskInfo.Uid, dgraphTaskInfo, assigneeDgraphInfo)
						publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "assignee_synced", map[string]interface{}{
							"assignee_uuid":  assigneeDgraphInfo.Uuid,
							"assignee_name":  assigneeDgraphInfo.DisplayName(),
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
				current := labelOf(dgraphTaskInfo)
				if next := helpers.WithTag(current, event.Label.Name); next != current {
					userDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
					if userDgraphInfo != nil {
						_ = taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, next, dgraphTaskInfo, userDgraphInfo)
						publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "label_synced", map[string]interface{}{
							"label": next,
						})
					}
				}
			}
		}
	case "unlabeled":
		if taskUUIDStr != "" && event.Label != nil && event.Label.Name != "" {
			dgraphTaskInfo, _ := taskBusiness.GetDgraphBasicTaskInfo(ctx, taskUUIDStr, "0x1")
			if dgraphTaskInfo != nil {
				current := labelOf(dgraphTaskInfo)
				if next := helpers.WithoutTag(current, event.Label.Name); next != current {
					userDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, link.CreatedBy.String())
					if userDgraphInfo != nil {
						_ = taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, next, dgraphTaskInfo, userDgraphInfo)
						publishGitHubSyncMqtt(taskUUIDStr, link.ProjectId.String(), "label_synced", map[string]interface{}{
							"label": next,
						})
					}
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
	if !prCommentMayDriveAgent(pv.Sender.Type, pv.Comment.User.Login, getGitHubBotLogin(ctx)) {
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsIssuesOrPRs) == nil {
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

	// The task linked to the issue, else to the pull request of that number.
	// The task's own project's link decides whether its comments sync, not
	// another project's link to the repository.
	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	taskUUIDStr, taskUUID, _, ok := leadTask(ctx, owner, repo, syncsIssuesOrPRs,
		func() string {
			taskUUIDStr, _ := taskDomain.FindTaskUUIDByGitHubIssueURL(ctx, event.Issue.HTMLURL)
			return taskUUIDStr
		},
		func() string {
			taskUUIDStr, _ := taskDomain.FindTaskUUIDByPRURL(ctx, fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, event.Issue.Number))
			return taskUUIDStr
		})
	if !ok {
		helpers.MessageLogs.InfoLog.Printf("No task of a project syncing %s/%s found for issue/PR #%d", owner, repo, event.Issue.Number)
		return nil
	}

	switch event.Action {
	case "created":
		return handleIssueCommentCreated(ctx, event, taskUUID, taskUUIDStr)
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
}, taskUUID uuid.UUID, taskUUIDStr string) error {
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

	_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "comment",
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsPRs) == nil {
		return nil
	}

	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")

	// The task already on this branch in a project linking this repository,
	// else the task whose id the branch's name carries.
	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	taskUUIDStr, taskUUID, own, ok := leadTask(ctx, owner, repo, syncsPRs,
		onBranch(ctx, owner, repo, branchName),
		func() string { return findTaskUUIDInText(ctx, branchName) })
	if !ok {
		helpers.MessageLogs.InfoLog.Printf("New branch %s on %s: it names no task of a project syncing it", branchName, event.Repository.FullName)
		return nil
	}
	if err := taskDomain.SetGitHubBranchOnTaskByTaskID(ctx, branchName, taskUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/handleCreateEvent Failed to set branch on task %s: %v", taskUUIDStr, err)
		return nil
	}
	helpers.MessageLogs.InfoLog.Printf("Linked new branch %s to task %s in project %s", branchName, taskUUIDStr, own.ProjectId.String())
	publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "branch_created", map[string]interface{}{
		"branch": branchName,
	})
	return nil
}

// handleDeleteEvent takes a branch deleted on GitHub off the tasks that carry
// it: those carrying its name in a project whose link to the repository syncs
// pull requests, unless a task's pull request is in another repository, where
// its branch is. The pull request stays linked, as it outlives its branch,
// merged or closed. Left on, the gone branch claimed any later branch of its
// name (onBranch), and a pull request opened from the task named it as head.
func handleDeleteEvent(ctx context.Context, body []byte) error {
	var event struct {
		Ref        string `json:"ref"`
		RefType    string `json:"ref_type"`
		Repository struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name string `json:"name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("failed to parse delete event: %v", err)
	}
	if event.RefType != "branch" {
		return nil
	}

	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	links, err := repositoryLinks(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsPRs) == nil {
		return nil
	}

	branch := strings.TrimPrefix(event.Ref, "refs/heads/")
	carrying, err := githubLinkModel.TasksOnBranch(ctx, owner, repo, branch)
	if err != nil {
		return fmt.Errorf("couldn't read the tasks on branch %s of %s: %w", branch, event.Repository.FullName, err)
	}
	var gone []uuid.UUID
	for _, task := range carrying {
		if task.PRURL != "" {
			prOwner, prRepo, _, err := ExtractGitHubIssueInfo(task.PRURL)
			if err == nil && !(strings.EqualFold(prOwner, owner) && strings.EqualFold(prRepo, repo)) {
				continue
			}
		}
		gone = append(gone, task.TaskID)
	}
	unset, err := githubLinkModel.UnsetBranch(ctx, gone, branch)
	if err != nil {
		return fmt.Errorf("couldn't take the deleted branch %s of %s off its tasks: %w", branch, event.Repository.FullName, err)
	}
	for _, task := range unset {
		helpers.MessageLogs.InfoLog.Printf("Branch %s deleted on %s: taken off task %s", branch, event.Repository.FullName, task.TaskID)
		publishGitHubSyncMqtt(task.TaskID.String(), task.ProjectID.String(), "branch_deleted", map[string]interface{}{
			"branch": branch,
		})
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

	// Each project's link to the repository has a secret of its own, and the
	// repository's one webhook is signed with the secret of whichever link
	// registered it last (registerGitHubWebhook), so any of them may verify it.
	repoOwner, repoName := extractRepoFromEvent(body)
	links, _ := repositoryLinks(ctx, repoOwner, repoName)
	for _, link := range links {
		if link.WebhookSecret != nil && *link.WebhookSecret != "" && verifyHMACSignature(*link.WebhookSecret, body, sig) {
			return true, link
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

	// What the webhook sends, each event HandleGitHubWebhookEvent handles. A
	// webhook registered before one was added doesn't send it until it is
	// registered again (StartWebhookRefresh does at each start).
	desiredEvents := []string{"issues", "pull_request", "push", "issue_comment", "create", "delete", "pull_request_review", "check_run"}

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
		"events": desiredEvents,
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	if linkSyncing(links, syncsPRs) == nil {
		return nil
	}

	owner, repo := event.Repository.Owner.Login, event.Repository.Name

	// Each commit naming a task, and each commit pushed to a task's branch, is
	// a line of the task's history. A push lists up to 2,048 commits: the
	// lines are written together, in line, before the delivery is recorded as
	// done (a goroutine and an insert apiece raced for the pool's connections,
	// and one that failed was lost after the delivery counted as handled), and
	// each task's project is told once, not once per commit.
	var history []githubTaskActivityModel.GitHubTaskActivity
	line := func(task uuid.UUID, kind string, message, address *string) {
		history = append(history, githubTaskActivityModel.GitHubTaskActivity{TaskID: task, ActivityType: kind,
			GitHubLogin: &event.Sender.Login, Title: message, Body: address})
	}
	type tell struct{ task, project, kind string }
	var tells []tell
	told := map[tell]bool{}

	// Magic words: parse commit messages for task references
	for _, commit := range event.Commits {
		refs := parseCommitMessageForTaskRefs(commit.Message)
		for _, ref := range refs {
			if ref.TaskUUID == "" {
				continue
			}
			refTaskUUID, own, ok := eventTask(ctx, ref.TaskUUID, owner, repo, syncsPRs)
			if !ok {
				continue
			}
			line(refTaskUUID, "commit_linked", &commit.Message, &commit.URL)
			if ref.Action == "close" || ref.Action == "fix" {
				applyAutomationRules(ctx, own, ref.TaskUUID, "commit_linked")
			}
			if t := (tell{ref.TaskUUID, own.ProjectId.String(), "commit_linked"}); !told[t] {
				told[t] = true
				tells = append(tells, t)
			}
		}
	}

	// The task on the pushed branch in a project linking this repository.
	branchName := strings.TrimPrefix(event.Ref, "refs/heads/")
	if taskUUIDStr, taskUUID, own, ok := leadTask(ctx, owner, repo, syncsPRs, onBranch(ctx, owner, repo, branchName)); ok {
		for _, commit := range event.Commits {
			line(taskUUID, "commit_pushed", &commit.Message, &commit.URL)
		}
		tells = append(tells, tell{taskUUIDStr, own.ProjectId.String(), "commit_pushed"})
	}

	if err := githubTaskActivityDomain.CreateGitHubTaskActivities(ctx, history); err != nil {
		return fmt.Errorf("couldn't write the push's commits into its tasks' history: %w", err)
	}
	for _, t := range tells {
		publishGitHubSyncMqtt(t.task, t.project, t.kind, nil)
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	syncing := linkSyncing(links, syncsPRs)
	if syncing == nil {
		return nil
	}

	// Fan out to the workspace event bus so a "PR-follow" agent (an event-trigger
	// agent subscribed to github.pr.review_submitted) can react when a review
	// lands — post an update, ping the author, etc. Decoupled via the bus so
	// this package never imports the AI/agent packages (loop-safe: agent writes
	// are workflow-tagged and never re-enter here). Fires for any review on a
	// repo some project's link syncs pull requests of, whether or not the PR is
	// tied to a OneCamp task; the place it names is the first such link's project.
	webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "github.pr.review_submitted", map[string]interface{}{
		"owner":        event.Repository.Owner.Login,
		"repo":         event.Repository.Name,
		"pr_number":    event.PullRequest.Number,
		"pr_url":       event.PullRequest.HTMLURL,
		"review_state": strings.ToUpper(event.Review.State),
		"reviewer":     event.Review.User.Login,
		"body":         event.Review.Body,
		"project_id":   syncing.ProjectId.String(),
	})

	// The task on the pull request's branch in a project linking this
	// repository, else the task linked to the pull request.
	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	taskUUIDStr, taskUUID, link, ok := leadTask(ctx, owner, repo, syncsPRs,
		onBranch(ctx, owner, repo, event.PullRequest.Head.Ref),
		func() string {
			taskUUIDStr, _ := taskDomain.FindTaskUUIDByPRURL(ctx, event.PullRequest.HTMLURL)
			return taskUUIDStr
		})
	if !ok {
		return nil
	}

	// Upsert the review record
	if err := githubPRReviewDomain.UpsertGitHubPRReview(ctx, taskUUID, event.Review.User.Login, event.Review.User.AvatarURL, event.Review.User.HTMLURL, strings.ToUpper(event.Review.State), time.Now()); err != nil {
		helpers.LogErrorWithContext(ctx, "business/handlePullRequestReviewEvent Failed to upsert review: %v", err)
		markInboundSyncFailed(ctx, taskUUID, "GitHub PR review sync failed")
	}

	// Update aggregate review state on task
	updateTaskReviewState(ctx, taskUUID)

	activityType := "pr_review_" + strings.ToLower(event.Review.State)
	_ = githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, activityType,
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

	links, err := repositoryLinks(ctx, event.Repository.Owner.Login, event.Repository.Name)
	if err != nil {
		return fmt.Errorf("couldn't read the links to %s: %w", event.Repository.FullName, err)
	}
	syncing := linkSyncing(links, syncsPRs)
	if syncing == nil {
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
	maybeEmitCIConcluded(ctx, event.Repository.Owner.Login, event.Repository.Name, syncing.ProjectId.String(), event.CheckRun.HeadSHA, prNumbers)

	owner, repo := event.Repository.Owner.Login, event.Repository.Name
	for _, pr := range event.CheckRun.PullRequests {
		// The task linked to the pull request, else the one on its head's
		// branch pattern in a project linking this repository.
		prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, pr.Number)
		taskUUIDStr, taskUUID, own, ok := leadTask(ctx, owner, repo, syncsPRs,
			func() string {
				taskUUIDStr, _ := taskDomain.FindTaskUUIDByPRURL(ctx, prURL)
				return taskUUIDStr
			},
			onBranch(ctx, owner, repo, fmt.Sprintf("pull/%d/head", pr.Number)))
		if !ok {
			continue
		}
		if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, "", checkStatus, "", nil); err != nil {
			helpers.LogErrorWithContext(ctx, "business/handleCheckRunEvent Failed to update task PR state: %v", err)
			markInboundSyncFailed(ctx, taskUUID, "GitHub check run sync failed")
		}
		publishGitHubSyncMqtt(taskUUIDStr, own.ProjectId.String(), "check_run", map[string]interface{}{
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
func maybeEmitCIConcluded(ctx context.Context, owner, repo, projectID, headSHA string, prNumbers []int) {
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
			"project_id":    projectID,
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
	// 1. Find linked repo via project. What this returns is shown to the
	// person (the controller puts it in msg), so a read of our own records
	// that failed is logged here with its detail and answered in their words:
	// wrapped, it showed the store's own error text.
	taskInfo, err := taskDomain.GetTaskProjectID(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GitHub/CreatePullRequestForTask task's project err: %+v", err)
		return nil, errors.New("couldn't read the task's project, try again")
	}
	if taskInfo == uuid.Nil {
		return nil, fmt.Errorf("task has no project")
	}

	links, err := githubLinkModel.GetGitHubLinksByProjectId(ctx, taskInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GitHub/CreatePullRequestForTask project's repos err: %+v", err)
		return nil, errors.New("couldn't read which repository the project is linked to, try again")
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("no linked repo for project")
	}
	// The project's first link (they come newest first): the read had no
	// order, which in practice was the order they were made.
	link := links[len(links)-1]

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

// labelOf is a task's label (its tags), "" when it has none.
func labelOf(t *dgraphStruct.DgraphTask) string {
	if t == nil || t.Label == nil {
		return ""
	}
	return *t.Label
}

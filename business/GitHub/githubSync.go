package business

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Task"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	githubCommentMappingDomain "github.com/akashc777/OneCamp/domain/GitHubCommentMapping"
	githubPRReviewDomain "github.com/akashc777/OneCamp/domain/GitHubPRReview"
	githubSyncQueueDomain "github.com/akashc777/OneCamp/domain/GitHubSyncQueue"
	githubTaskActivityDomain "github.com/akashc777/OneCamp/domain/GitHubTaskActivity"
	integrationDomain "github.com/akashc777/OneCamp/domain/Integration"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	prReviewModel "github.com/akashc777/OneCamp/models/postgres/GitHubPRReview"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	githubSyncMaxRetries   = 5
	githubSyncWorkerPoll   = 5 * time.Second
	githubSyncStaleTimeout = 10 * time.Minute
)

// ttlCacheEntry holds a cached value with an expiration time.
type ttlCacheEntry struct {
	value     string
	expiresAt time.Time
}

// ttlCache is a simple in-memory cache with TTL support.
type ttlCache struct {
	mu         sync.RWMutex
	items      map[string]ttlCacheEntry
	defaultTTL time.Duration
}

// newTTLCache creates a cache with the given default TTL.
func newTTLCache(defaultTTL time.Duration) *ttlCache {
	return &ttlCache{
		items:      make(map[string]ttlCacheEntry),
		defaultTTL: defaultTTL,
	}
}

// Load returns the cached value if it exists and has not expired.
func (c *ttlCache) Load(key string) (string, bool) {
	c.mu.RLock()
	entry, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			c.mu.Lock()
			delete(c.items, key)
			c.mu.Unlock()
		}
		return "", false
	}
	return entry.value, true
}

// Store stores a value with the default TTL.
func (c *ttlCache) Store(key, value string) {
	c.mu.Lock()
	c.items[key] = ttlCacheEntry{value: value, expiresAt: time.Now().Add(c.defaultTTL)}
	c.mu.Unlock()
}

// Delete removes a key from the cache.
func (c *ttlCache) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

// userMapCache caches GitHub login → OneCamp user UUID lookups (30min TTL).
var userMapCache = newTTLCache(30 * time.Minute)

// botLoginCache caches the GitHub login of the OAuth token owner (30min TTL).
var botLoginCache = newTTLCache(30 * time.Minute)

// githubHTTPClient is a shared HTTP client for all GitHub API calls.
// Using a single client enables connection pooling and TLS session reuse.
var githubHTTPClient = &http.Client{Timeout: 15 * time.Second}

// getGitHubBotLogin returns the GitHub login for the current OAuth token, cached.
// Retries up to 3 times with backoff to tolerate transient failures.
//
// Uses GitHubHTTPClient so the call sees an auto-refreshed token even
// when the cached row's access_token is past expiry.
func getGitHubBotLogin() string {
	if cached, ok := botLoginCache.Load("bot"); ok && cached != "" {
		return cached
	}

	ctx := context.Background()
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		return ""
	}

	var login string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
		req.Header.Set("Accept", "application/vnd.github+json")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			continue
		}

		var user struct {
			Login string `json:"login"`
		}
		if json.Unmarshal(body, &user) != nil || user.Login == "" {
			continue
		}
		login = user.Login
		break
	}

	if login != "" {
		botLoginCache.Store("bot", login)
	}
	return login
}

// invalidateBotLoginCache clears the cached bot login (call on disconnect/token change).
func invalidateBotLoginCache() {
	botLoginCache.Delete("bot")
}

// StartGitHubSyncWorker runs the background sync worker goroutine.
// Wakes immediately on signal, or checks every 60s as a fallback.
// Respects the provided context for graceful shutdown.
func StartGitHubSyncWorker(signal <-chan struct{}, shutdownCtx context.Context) {
	go func() {
		time.Sleep(10 * time.Second)
		for {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						helpers.LogErrorWithContext(context.Background(), "GitHub sync worker panic recovered: %v", rec)
					}
				}()
				reapStaleSyncItems()
				processSyncQueue()
			}()

			select {
			case <-shutdownCtx.Done():
				helpers.MessageLogs.InfoLog.Println("GitHub sync worker shutting down")
				return
			case <-signal:
				// woke by new enqueue — loop immediately
			case <-time.After(60 * time.Second):
				// periodic fallback check
			}
		}
	}()
	helpers.MessageLogs.InfoLog.Println("GitHub sync worker started (event-driven with 60s fallback)")
}

func reapStaleSyncItems() {
	ctx := context.Background()
	n, err := githubSyncQueueDomain.ReapStaleGitHubSyncQueueItems(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "GitHub sync worker failed to reap stale items: %v", err)
		return
	}
	if n > 0 {
		helpers.MessageLogs.InfoLog.Printf("GitHub sync worker reaped %d stale processing items", n)
	}
}

func processSyncQueue() {
	ctx := context.Background()

	// Skip processing if GitHub is not connected
	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		return
	}

	items, err := githubSyncQueueDomain.GetPendingGitHubSyncQueueItems(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "GitHub sync worker failed to fetch queue: %v", err)
		return
	}

	for _, item := range items {
		// Mark as processing
		if err := githubSyncQueueDomain.MarkGitHubSyncQueueProcessing(ctx, item.ID); err != nil {
			continue
		}

		syncErr := executeGitHubSync(item.TaskID, item.SyncType, item.Payload)

		if syncErr == nil {
			if err := githubSyncQueueDomain.MarkGitHubSyncQueueCompleted(ctx, item.ID); err != nil {
				helpers.LogErrorWithContext(ctx, "GitHub sync worker failed to mark complete: %v", err)
			}
		} else {
			errStr := syncErr.Error()
			if item.Attempts+1 >= githubSyncMaxRetries {
				if err := githubSyncQueueDomain.MarkGitHubSyncQueueFailed(ctx, item.ID, errStr, nil); err != nil {
					helpers.LogErrorWithContext(ctx, "GitHub sync worker failed to mark failed: %v", err)
				}
			} else {
				backoff := time.Duration(1<<uint(item.Attempts)) * time.Second
				nextRetry := time.Now().Add(backoff)
				if err := githubSyncQueueDomain.MarkGitHubSyncQueuePending(ctx, item.ID, nextRetry); err != nil {
					helpers.LogErrorWithContext(ctx, "GitHub sync worker failed to mark pending: %v", err)
				}
			}
		}
	}
}

func executeGitHubSync(taskID uuid.UUID, syncType string, payloadJSON string) error {
	ctx := context.Background()

	// Mark as pending at start
	_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "pending", nil, 0)

	githubIssueURL, githubPRURL, err := taskDomain.GetTaskGitHubURLs(ctx, taskID)
	if err != nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", strPtr("task not found"), 1)
		return fmt.Errorf("task not found: %w", err)
	}

	// Use issue URL if available; fall back to PR URL for PR-linked tasks.
	var targetURL string
	if githubIssueURL != nil && *githubIssueURL != "" {
		targetURL = *githubIssueURL
	} else if githubPRURL != nil && *githubPRURL != "" {
		targetURL = *githubPRURL
	}
	if targetURL == "" {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", strPtr("task has no linked GitHub issue or PR"), 1)
		return fmt.Errorf("task has no linked GitHub issue or PR")
	}

	owner, repo, number, err := ExtractGitHubIssueInfo(targetURL)
	if err != nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", strPtr("invalid GitHub URL"), 1)
		return fmt.Errorf("invalid GitHub URL: %w", err)
	}

	integration, err := integrationDomain.GetIntegration(context.Background(), "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", strPtr("GitHub not connected"), 1)
		return fmt.Errorf("GitHub not connected")
	}
	// Get a freshly-refreshed token. Falls back to the integration row's
	// raw access_token when no refresh path is available (e.g. a legacy
	// non-rotating GitHub OAuth App).
	token, tokErr := GitHubAccessToken(ctx)
	if tokErr != nil {
		token = *integration.AccessToken
	}

	var syncErr error
	switch syncType {
	case "status":
		var payload struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid status payload: %w", err)
		}
		syncErr = syncIssueStatusToGitHub(owner, repo, number, payload.Status, token)
	case "name":
		var payload struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid name payload: %w", err)
		}
		syncErr = syncIssueTitleToGitHub(owner, repo, number, payload.Name, token)
	case "description":
		var payload struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid description payload: %w", err)
		}
		syncErr = syncIssueBodyToGitHub(owner, repo, number, payload.Description, token)
	case "assignee":
		var payload struct {
			Assignee string `json:"assignee_uuid"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid assignee payload: %w", err)
		}
		syncErr = syncIssueAssigneeToGitHub(owner, repo, number, payload.Assignee, token)
	case "label":
		var payload struct {
			Label    string `json:"label"`
			OldLabel string `json:"old_label"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid label payload: %w", err)
		}
		syncErr = syncIssueLabelToGitHub(owner, repo, number, payload.OldLabel, payload.Label, token)
	case "comment":
		var payload struct {
			CommentBody string `json:"comment_body"`
			CommentUUID string `json:"comment_uuid"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid comment payload: %w", err)
		}
		var ghCommentID int64
		ghCommentID, syncErr = syncIssueCommentToGitHub(owner, repo, number, payload.CommentBody, token)
		if syncErr == nil && ghCommentID > 0 && payload.CommentUUID != "" {
			if commentUUID, parseErr := uuid.Parse(payload.CommentUUID); parseErr == nil {
				_ = githubCommentMappingDomain.UpsertGitHubCommentMapping(ctx, owner, repo, ghCommentID, commentUUID, taskID)
				helpers.MessageLogs.InfoLog.Printf("Mapped outbound comment %s to GitHub comment %d", payload.CommentUUID, ghCommentID)
			}
		}
	case "comment_edit":
		var payload struct {
			GitHubCommentID int64  `json:"github_comment_id"`
			CommentBody     string `json:"comment_body"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid comment_edit payload: %w", err)
		}
		if payload.GitHubCommentID > 0 {
			syncErr = syncIssueCommentEditToGitHub(owner, repo, payload.GitHubCommentID, payload.CommentBody, token)
		}
	case "comment_delete":
		var payload struct {
			GitHubCommentID int64 `json:"github_comment_id"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
			return fmt.Errorf("invalid comment_delete payload: %w", err)
		}
		if payload.GitHubCommentID > 0 {
			syncErr = syncIssueCommentDeleteFromGitHub(owner, repo, payload.GitHubCommentID, token)
		}
	default:
		return fmt.Errorf("unknown sync type: %s", syncType)
	}

	projectUUID, _ := taskDomain.GetTaskProjectID(ctx, taskID)

	if syncErr != nil {
		errStr := syncErr.Error()
		// If GitHub returns 404, the issue was deleted — clear GitHub fields from task
		if strings.Contains(errStr, "GitHub API error 404") {
			clearGitHubFieldsFromTask(taskID)
			_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "synced", nil, 0)
			publishGitHubSyncMqtt(taskID.String(), projectUUID.String(), "sync_status_changed", map[string]interface{}{
				"status": "synced",
				"reason": "issue_deleted",
			})
			return nil
		}
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "failed", &errStr, 1)
		publishGitHubSyncMqtt(taskID.String(), projectUUID.String(), "sync_status_changed", map[string]interface{}{
			"status": "failed",
			"error":  errStr,
		})
		return syncErr
	}

	// Set github_last_synced_at AFTER successful API call to prevent webhook loops.
	// Only set on success: a failed sync should NOT block future webhooks.
	now := time.Now()
	if err := taskDomain.SetGitHubLastSyncedAt(ctx, taskID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "executeGitHubSync failed to set last_synced_at for task %s: %v", taskID.String(), err)
	}
	_ = taskDomain.SetGitHubSyncStatus(ctx, taskID, "synced", nil, 0)
	publishGitHubSyncMqtt(taskID.String(), projectUUID.String(), "sync_status_changed", map[string]interface{}{
		"status": "synced",
	})
	return nil
}

func strPtr(s string) *string {
	return &s
}

func syncIssueStatusToGitHub(owner, repo string, number int, oneCampStatus string, token string) error {
	var state, stateReason string
	switch oneCampStatus {
	case "done":
		state = "closed"
		stateReason = "completed"
	case "canceled":
		state = "closed"
		stateReason = "not_planned"
	default:
		state = "open"
	}

	body := fmt.Sprintf(`{"state":"%s"`, state)
	if stateReason != "" {
		body += fmt.Sprintf(`,"state_reason":"%s"`, stateReason)
	}
	body += "}"

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number)
	req, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func syncIssueTitleToGitHub(owner, repo string, number int, title string, token string) error {
	bodyBytes, _ := json.Marshal(map[string]string{"title": title})
	req, _ := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number),
		bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()
	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func syncIssueBodyToGitHub(owner, repo string, number int, desc string, token string) error {
	if len(desc) > 65536 {
		desc = helpers.TruncateRunesWithSuffix(desc, 65500, "...(truncated)")
	}
	bodyBytes, _ := json.Marshal(map[string]string{"body": desc})
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()
	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func syncIssueAssigneeToGitHub(owner, repo string, number int, assigneeUUID string, token string) error {
	githubLogin := MapOneCampUserToGitHubLogin(assigneeUUID)
	var assignees []string
	if githubLogin != "" {
		assignees = []string{githubLogin}
	}
	bodyBytes, _ := json.Marshal(map[string]interface{}{"assignees": assignees})
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number)
	req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()
	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func syncIssueLabelToGitHub(owner, repo string, number int, oldLabel, newLabel, token string) error {
	client := githubHTTPClient
	baseURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number)

	// Step 1: Remove old label if it differs from new label and is not empty
	if oldLabel != "" && oldLabel != newLabel {
		delURL := fmt.Sprintf("%s/labels/%s", baseURL, url.PathEscape(oldLabel))
		req, _ := http.NewRequest(http.MethodDelete, delURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("GitHub API error removing old label: %w", err)
		}
		if err := checkGitHubRateLimit(resp); err != nil {
			resp.Body.Close()
			return err
		}
		// 404 is fine — label might not exist on the issue
		if resp.StatusCode >= 400 && resp.StatusCode != 404 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			return fmt.Errorf("GitHub API error %d removing old label: %s", resp.StatusCode, string(respBody))
		}
		resp.Body.Close()
	}

	// Step 2: Add new label if not empty
	if newLabel == "" {
		return nil
	}
	bodyBytes, _ := json.Marshal(map[string]interface{}{"labels": []string{newLabel}})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/labels", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error adding label: %w", err)
	}
	defer resp.Body.Close()
	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d adding label: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// MapGitHubUserToOneCampUser resolves a GitHub login to a OneCamp user UUID.
// Caches results in memory. Auto-provisions an external user if no match is found.
func MapGitHubUserToOneCampUser(githubLogin string) string {
	if githubLogin == "" {
		return ""
	}
	if cached, ok := userMapCache.Load(githubLogin); ok {
		return cached
	}

	ctx := context.Background()

	// 1. Check integrations table for existing mapping
	metadataStr, err := integrationDomain.GetIntegrationMetadataByGitHubLogin(ctx, githubLogin)
	if err == nil && metadataStr != nil {
		var meta struct {
			GithubLogin string `json:"github_login"`
			UserUUID    string `json:"user_uuid"`
		}
		if json.Unmarshal([]byte(*metadataStr), &meta) == nil && meta.GithubLogin == githubLogin && meta.UserUUID != "" {
			userMapCache.Store(githubLogin, meta.UserUUID)
			return meta.UserUUID
		}
	}

	// 2. Check users table for a previously provisioned external user
	existingUser, err := userDomain.GetUserByGitHubLogin(ctx, githubLogin)
	if err == nil && existingUser != nil {
		userUUID := existingUser.Id.String()
		meta, _ := json.Marshal(map[string]string{"github_login": githubLogin, "user_uuid": userUUID})
		metaStr := string(meta)
		integrationDomain.UpsertIntegration(ctx, "user", existingUser.Id, "github", nil, nil, nil, &metaStr, nil)
		userMapCache.Store(githubLogin, userUUID)
		return userUUID
	}

	// 3. Try fetching GitHub user's public email and match by email
	profile := fetchGitHubUserProfile(ctx, githubLogin)
	if profile.Email != "" {
		userID, err := userDomain.GetUserIDByEmail(ctx, profile.Email)
		if err == nil && userID != uuid.Nil {
			userUUID := userID.String()
			meta, _ := json.Marshal(map[string]string{"github_login": githubLogin, "user_uuid": userUUID})
			metaStr := string(meta)
			integrationDomain.UpsertIntegration(ctx, "user", userID, "github", nil, nil, nil, &metaStr, nil)
			userMapCache.Store(githubLogin, userUUID)
			return userUUID
		}
	}

	// 4. Auto-provision an external user so that comments/assignees/reactions are never lost
	if profile.Login != "" {
		displayName := profile.Name
		if displayName == "" {
			displayName = profile.Login
		}
		newUser, err := userBusiness.CreateExternalGitHubUser(ctx, profile.Login, displayName, profile.AvatarURL, profile.HTMLURL, profile.Email)
		if err == nil && newUser != nil {
			userUUID := newUser.Id.String()
			meta, _ := json.Marshal(map[string]string{"github_login": githubLogin, "user_uuid": userUUID})
			metaStr := string(meta)
			integrationDomain.UpsertIntegration(ctx, "user", newUser.Id, "github", nil, nil, nil, &metaStr, nil)
			userMapCache.Store(githubLogin, userUUID)
			helpers.MessageLogs.InfoLog.Printf("Auto-provisioned external OneCamp user %s for GitHub login @%s", userUUID, githubLogin)
			return userUUID
		}
	}

	userMapCache.Store(githubLogin, "")
	return ""
}

// MapOneCampUserToGitHubLogin resolves a OneCamp user UUID to a GitHub login.
func MapOneCampUserToGitHubLogin(userUUID string) string {
	if userUUID == "" {
		return ""
	}
	cacheKey := "rev:" + userUUID
	if cached, ok := userMapCache.Load(cacheKey); ok {
		return cached
	}

	ctx := context.Background()
	uid, err := uuid.Parse(userUUID)
	if err != nil {
		userMapCache.Store(cacheKey, "")
		return ""
	}

	metadataStr, err := integrationDomain.GetIntegrationMetadataByEntityID(ctx, uid)
	if err == nil && metadataStr != nil {
		var meta struct {
			GithubLogin string `json:"github_login"`
		}
		if json.Unmarshal([]byte(*metadataStr), &meta) == nil && meta.GithubLogin != "" {
			userMapCache.Store(cacheKey, meta.GithubLogin)
			return meta.GithubLogin
		}
	}

	userMapCache.Store(cacheKey, "")
	return ""
}

type githubUserProfile struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
	HTMLURL   string `json:"html_url"`
}

func fetchGitHubUserProfile(ctx context.Context, githubLogin string) githubUserProfile {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/users/%s", githubLogin), nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "OneCamp/1.0")

	// Use authenticated requests to avoid low unauthenticated rate
	// limits (60/hr). The auto-refresh client handles expiry; if we're
	// not connected we fall back to the unauthenticated path.
	client, err := GitHubHTTPClient(ctx)
	if err != nil {
		client = githubHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return githubUserProfile{Login: githubLogin}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return githubUserProfile{Login: githubLogin}
	}

	var user githubUserProfile
	if json.NewDecoder(resp.Body).Decode(&user) == nil {
		if user.Login == "" {
			user.Login = githubLogin
		}
		return user
	}
	return githubUserProfile{Login: githubLogin}
}

func ExtractGitHubIssueInfo(url string) (owner, repo string, number int, err error) {
	url = strings.TrimPrefix(url, "https://github.com/")
	url = strings.TrimPrefix(url, "http://github.com/")
	parts := strings.Split(url, "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return "", "", 0, fmt.Errorf("invalid GitHub issue/PR URL: %s", url)
	}
	owner = parts[0]
	repo = parts[1]
	fmt.Sscanf(parts[3], "%d", &number)
	if owner == "" || repo == "" || number == 0 {
		return "", "", 0, fmt.Errorf("invalid GitHub issue/PR URL: %s", url)
	}
	return owner, repo, number, nil
}

func syncIssueCommentToGitHub(owner, repo string, number int, commentBody string, token string) (githubCommentID int64, err error) {
	bodyBytes, _ := json.Marshal(map[string]string{"body": commentBody})
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, number),
		bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return 0, err
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}

	// Capture the GitHub comment ID from the response so we can map it back.
	var created struct {
		ID int64 `json:"id"`
	}
	if resp.StatusCode == http.StatusCreated {
		if decodeErr := json.NewDecoder(resp.Body).Decode(&created); decodeErr != nil {
			helpers.LogErrorWithContext(context.Background(), "syncIssueCommentToGitHub failed to decode response: %v", decodeErr)
			return 0, nil // non-fatal: comment was created, just can't map it
		}
		if created.ID > 0 {
			return created.ID, nil
		}
	}
	return 0, nil
}

func syncIssueCommentEditToGitHub(owner, repo string, githubCommentID int64, commentBody string, token string) error {
	bodyBytes, _ := json.Marshal(map[string]string{"body": commentBody})
	req, _ := http.NewRequest(http.MethodPatch,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", owner, repo, githubCommentID),
		bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func syncIssueCommentDeleteFromGitHub(owner, repo string, githubCommentID int64, token string) error {
	req, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", owner, repo, githubCommentID),
		nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return err
	}

	// 204 = deleted, 404 = already deleted or not found — both are fine.
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func clearGitHubFieldsFromTask(taskID uuid.UUID) {
	ctx := context.Background()
	if err := taskDomain.ClearGitHubIssueFieldsFromTask(ctx, taskID); err != nil {
		helpers.LogErrorWithContext(ctx, "clearGitHubFieldsFromTask failed for task %s: %v", taskID.String(), err)
	}
	helpers.MessageLogs.InfoLog.Printf("Cleared GitHub fields from task %s — linked GitHub issue was deleted", taskID.String())
}

func checkGitHubRateLimit(resp *http.Response) error {
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	if remaining == "0" {
		resetStr := resp.Header.Get("X-RateLimit-Reset")
		resetAt, _ := strconv.ParseInt(resetStr, 10, 64)
		waitSec := resetAt - time.Now().Unix()
		if waitSec < 1 {
			waitSec = 60
		}
		return fmt.Errorf("GitHub rate limit exceeded, retry after %d seconds", waitSec)
	}
	return nil
}

// RefreshFromGitHub pulls the current state of a linked GitHub issue/PR and
// overwrites the OneCamp task. It fetches title, body, status, labels, assignee,
// comments, PR state, checks, and reviews. It is idempotent: calling it multiple
// times is safe because GitHub comment IDs are deduplicated via
// github_comment_mappings.
func RefreshFromGitHub(ctx context.Context, taskUUID uuid.UUID, backfillComments bool) error {
	_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "pending", nil, 0)

	// 1. Resolve GitHub URLs
	issueURL, prURL, err := taskDomain.GetTaskGitHubURLs(ctx, taskUUID)
	if err != nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("task not found"), 1)
		return fmt.Errorf("task not found: %w", err)
	}
	var targetURL string
	var isPR bool
	if issueURL != nil && *issueURL != "" {
		targetURL = *issueURL
	} else if prURL != nil && *prURL != "" {
		targetURL = *prURL
		isPR = true
	}
	if targetURL == "" {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("task has no linked GitHub issue or PR"), 1)
		return fmt.Errorf("task has no linked GitHub issue or PR")
	}

	owner, repo, number, err := ExtractGitHubIssueInfo(targetURL)
	if err != nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("invalid GitHub URL"), 1)
		return fmt.Errorf("invalid GitHub URL: %w", err)
	}

	integration, err := integrationDomain.GetIntegration(ctx, "org", uuid.Nil, "github")
	if err != nil || integration == nil || integration.AccessToken == nil || *integration.AccessToken == "" {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("GitHub not connected"), 1)
		return fmt.Errorf("GitHub not connected")
	}
	token, tokErr := GitHubAccessToken(ctx)
	if tokErr != nil {
		token = *integration.AccessToken
	}

	// 2. Fetch current task info from Dgraph (needed as baseline for comparisons)
	// Use "0x1" (groot) as userUid to avoid Dgraph "ID can't be empty" error.
	// Auth fields are not used in server-side refresh operations.
	// The sync worker has no user: this read is the server refreshing its own
	// record of a task, so it says so rather than passing a uid nobody owns.
	dgraphTaskInfo, err := taskBusiness.GetDgraphTaskInfo(helpers.WithSystemRead(ctx), taskUUID.String(), "0x1")
	if err != nil || dgraphTaskInfo == nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("task not found in Dgraph"), 1)
		return fmt.Errorf("task not found in Dgraph: %w", err)
	}

	// Use task creator as the actor for refresh-generated activities
	var actorDgraphInfo *dgraphStruct.DgraphUser
	if dgraphTaskInfo.CreatedBy != nil {
		actorDgraphInfo, _ = userBusiness.GetDgraphUserInfoByUUID(ctx, dgraphTaskInfo.CreatedBy.Uuid)
	}
	if actorDgraphInfo == nil {
		actorDgraphInfo = dgraphTaskInfo.CreatedBy
	}
	if actorDgraphInfo == nil {
		helpers.LogErrorWithContext(ctx, "RefreshFromGitHub: no actorDgraphInfo for task %s", taskUUID.String())
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr("actor user not found"), 1)
		return fmt.Errorf("actor user not found")
	}

	// 3. Fetch GitHub issue data
	issueData, err := fetchGitHubIssue(ctx, owner, repo, number, token)
	if err != nil {
		_ = taskDomain.SetGitHubSyncStatus(ctx, taskUUID, "failed", strPtr(err.Error()), 1)
		return err
	}

	// 4. Sync fields
	syncTaskFieldsFromGitHub(ctx, taskUUID, dgraphTaskInfo, actorDgraphInfo, issueData)

	// 5. Sync comments (only when explicitly backfilling; routine refresh should not flood the task)
	if backfillComments {
		if err := syncCommentsFromGitHub(ctx, taskUUID, owner, repo, number, token, dgraphTaskInfo, actorDgraphInfo); err != nil {
			helpers.LogErrorWithContext(ctx, "RefreshFromGitHub comment sync failed: %v", err)
		}
	}

	// 6. For PRs: sync PR-specific data
	if isPR {
		if err := syncPRDetailsFromGitHub(ctx, taskUUID, owner, repo, number, token, dgraphTaskInfo, actorDgraphInfo); err != nil {
			helpers.LogErrorWithContext(ctx, "RefreshFromGitHub PR detail sync failed: %v", err)
		}
	}

	// 7. Success
	// Use a fresh context for the final DB writes so that even if the caller's
	// context timed out during long comment fetches, we can still update status.
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer statusCancel()
	now := time.Now()
	_ = taskDomain.SetGitHubLastSyncedAt(statusCtx, taskUUID, now)
	_ = taskDomain.SetGitHubSyncStatus(statusCtx, taskUUID, "synced", nil, 0)
	if dgraphTaskInfo.Project != nil {
		publishGitHubSyncMqtt(taskUUID.String(), dgraphTaskInfo.Project.Uuid, "refresh", nil)
	}
	return nil
}

// fetchGitHubIssue fetches the current state of a GitHub issue.
func fetchGitHubIssue(ctx context.Context, owner, repo string, number int, token string) (*githubIssueData, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, number), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API error: %w", err)
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GitHub API error 404: issue not found")
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
	}

	var data githubIssueData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub issue: %w", err)
	}
	return &data, nil
}

// githubIssueData holds the fields we care about from a GitHub issue.
type githubIssueData struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	Assignee    *struct {
		Login string `json:"login"`
	} `json:"assignee"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// syncTaskFieldsFromGitHub applies GitHub issue fields to the OneCamp task
// only when they differ. It logs activities for changes.
// statusFromIssue is the category a task takes from its GitHub issue, or ""
// to leave it. GitHub knows only open and closed, so only a disagreement about
// that moves the task: a closed issue closes an open task (canceled when
// closed as not planned), a reopened one reopens a closed task. An open issue
// leaves an open task where the team put it (In Progress, In Review, a
// project's own "QA"); it used to send every one of them back to Todo on each
// refresh. current is the task's category.
func statusFromIssue(state, stateReason, current string) string {
	closed := taskStatusBusiness.IsClosed(current)
	switch {
	case state == "closed" && closed:
		return ""
	case state == "closed" && stateReason == "not_planned":
		return dgraphStruct.TASK_STATUS_CANCELED
	case state == "closed":
		return dgraphStruct.TASK_STATUS_DONE
	case closed:
		return dgraphStruct.TASK_STATUS_TODO
	}
	return ""
}

func syncTaskFieldsFromGitHub(ctx context.Context, taskUUID uuid.UUID, dgraphTaskInfo *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser, issue *githubIssueData) {
	if actor == nil {
		return
	}

	// Title
	if issue.Title != "" && issue.Title != dgraphTaskInfo.Name {
		if err := taskBusiness.UpdateTaskNameByTaskUUID(ctx, taskUUID, issue.Title, dgraphTaskInfo, actor); err != nil {
			helpers.LogErrorWithContext(ctx, "RefreshFromGitHub title sync failed: %v", err)
		} else {
			dgraphTaskInfo.Name = issue.Title
		}
	}

	// Description
	if issue.Body != "" {
		var currentDesc string
		if dgraphTaskInfo.Description != nil {
			currentDesc = *dgraphTaskInfo.Description
		}
		if issue.Body != currentDesc {
			if err := taskBusiness.UpdateTaskDesByTaskUUID(ctx, taskUUID, issue.Body, nil, dgraphTaskInfo, actor); err != nil {
				helpers.LogErrorWithContext(ctx, "RefreshFromGitHub description sync failed: %v", err)
			} else {
				dgraphTaskInfo.Description = &issue.Body
			}
		}
	}

	targetStatus := statusFromIssue(issue.State, issue.StateReason, dgraphTaskInfo.Status)
	if targetStatus != "" && targetStatus != dgraphTaskInfo.Status {
		if err := taskBusiness.UpdateTaskStatusByTaskUUID(ctx, taskUUID, targetStatus, dgraphTaskInfo, actor); err != nil {
			helpers.LogErrorWithContext(ctx, "RefreshFromGitHub status sync failed: %v", err)
		} else {
			dgraphTaskInfo.Status = targetStatus
		}
	}

	// Assignee
	if issue.Assignee != nil {
		assignedUUID := MapGitHubUserToOneCampUser(issue.Assignee.Login)
		if assignedUUID != "" {
			if dgraphTaskInfo.Assignee == nil || dgraphTaskInfo.Assignee.Uuid != assignedUUID {
				assigneeDgraphInfo, _ := userBusiness.GetDgraphUserInfoByUUID(ctx, assignedUUID)
				if assigneeDgraphInfo != nil {
					var oldUID string
					if dgraphTaskInfo.Assignee != nil {
						oldUID = dgraphTaskInfo.Assignee.Uid
					}
					if err := taskBusiness.UpdateTaskAssigneeByTaskUUID(ctx, taskUUID, assigneeDgraphInfo, oldUID, dgraphTaskInfo.Uid, dgraphTaskInfo, actor); err != nil {
						helpers.LogErrorWithContext(ctx, "RefreshFromGitHub assignee sync failed: %v", err)
					}
				}
			}
		}
	}

	// Labels — only sync the first label as OneCamp tasks have a single label
	if len(issue.Labels) > 0 {
		newLabel := issue.Labels[0].Name
		var currentLabel string
		if dgraphTaskInfo.Label != nil {
			currentLabel = *dgraphTaskInfo.Label
		}
		if newLabel != currentLabel {
			if err := taskBusiness.UpdateTaskLabelByTaskUUID(ctx, taskUUID, newLabel, dgraphTaskInfo, actor); err != nil {
				helpers.LogErrorWithContext(ctx, "RefreshFromGitHub label sync failed: %v", err)
			}
		}
	}
}

// syncCommentsFromGitHub fetches all comments for an issue and creates any
// that are not already tracked in github_comment_mappings.
func syncCommentsFromGitHub(ctx context.Context, taskUUID uuid.UUID, owner, repo string, number int, token string, dgraphTaskInfo *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser) error {
	// Build set of existing GitHub comment IDs for this task
	existingMappings, err := githubCommentMappingDomain.GetGitHubCommentMappingsByTask(ctx, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "syncCommentsFromGitHub failed to load mappings: %v", err)
		existingMappings = make(map[int64]uuid.UUID)
	}

	// Fetch comments (paginated, up to 10 pages)
	comments, err := fetchGitHubIssueComments(ctx, owner, repo, number, token)
	if err != nil {
		return err
	}

	// Pre-resolve all unique GitHub users → OneCamp users in one Dgraph batch.
	uniqueUserUUIDs := make([]string, 0, len(comments))
	seen := make(map[string]bool)
	for _, c := range comments {
		if _, exists := existingMappings[c.ID]; exists {
			continue
		}
		oneCampUUID := MapGitHubUserToOneCampUser(c.User.Login)
		if oneCampUUID != "" && !seen[oneCampUUID] {
			seen[oneCampUUID] = true
			uniqueUserUUIDs = append(uniqueUserUUIDs, oneCampUUID)
		}
	}
	var userMap map[string]*dgraphStruct.DgraphUser
	if len(uniqueUserUUIDs) > 0 {
		users, _ := userBusiness.GetDgraphUserInfoByUUIDs(ctx, uniqueUserUUIDs)
		userMap = make(map[string]*dgraphStruct.DgraphUser, len(users))
		for _, u := range users {
			if u.Uuid != "" {
				userMap[u.Uuid] = u
			}
		}
	}

	for _, c := range comments {
		if _, exists := existingMappings[c.ID]; exists {
			continue // already synced via mapping dedup
		}

		// Resolve user from pre-fetched map (with fallback to actor)
		oneCampUserUUID := MapGitHubUserToOneCampUser(c.User.Login)
		var userDgraphInfo *dgraphStruct.DgraphUser
		var commenterUUID uuid.UUID
		if oneCampUserUUID != "" {
			userDgraphInfo = userMap[oneCampUserUUID]
			if userDgraphInfo != nil {
				commenterUUID, _ = uuid.Parse(oneCampUserUUID)
			}
		}
		if userDgraphInfo == nil && actor != nil {
			userDgraphInfo = actor
			commenterUUID, _ = uuid.Parse(actor.Uuid)
		}
		if userDgraphInfo == nil {
			helpers.LogErrorWithContext(ctx, "syncCommentsFromGitHub skipping comment %d: no valid user", c.ID)
			continue
		}

		// Preserve original GitHub comment creation time so ordering is correct after backfill.
		var createdAt *time.Time
		if c.CreatedAt != "" {
			if t, parseErr := time.Parse(time.RFC3339, c.CreatedAt); parseErr == nil {
				createdAt = &t
			}
		}
		commentInput := &adapter.CreateOrUpdateTaskCommentInput{
			CommentBody:    c.Body,
			TaskUuid:       taskUUID.String(),
			SkipGitHubSync: true,
			CreatedAt:      createdAt,
		}
		userInfo := &userModels.UserInfo{
			UserPostgresInfo: userModels.User{Id: commenterUUID},
			UserDgraphInfo:   *userDgraphInfo,
		}

		createdComment, err := taskBusiness.CreateTaskComment(ctx, taskUUID, dgraphTaskInfo, userInfo, commentInput, nil)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "syncCommentsFromGitHub failed to create comment %d: %v", c.ID, err)
			continue
		}

		// Track mapping
		if createdComment != nil && createdComment.Uuid != "" {
			commentUUID, _ := uuid.Parse(createdComment.Uuid)
			_ = githubCommentMappingDomain.UpsertGitHubCommentMapping(ctx, owner, repo, c.ID, commentUUID, taskUUID)
		}

		// Log activity
		go githubTaskActivityDomain.CreateGitHubTaskActivity(ctx, taskUUID, "comment",
			&c.User.Login, userDgraphInfo.ProfileKey, &userDgraphInfo.EmailID, nil, &c.Body, nil)
	}

	return nil
}

// fetchGitHubIssueComments fetches all comments for a GitHub issue (paginated).
func fetchGitHubIssueComments(ctx context.Context, owner, repo string, number int, token string) ([]githubComment, error) {
	var allComments []githubComment
	page := 1
	for page <= 10 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments?page=%d&per_page=100", owner, repo, number, page), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")

		client := githubHTTPClient
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GitHub API error: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()

		if err := checkGitHubRateLimit(resp); err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
		}

		var comments []githubComment
		if err := json.Unmarshal(body, &comments); err != nil {
			return nil, fmt.Errorf("failed to decode comments: %w", err)
		}
		if len(comments) == 0 {
			break
		}
		allComments = append(allComments, comments...)
		page++
	}
	return allComments, nil
}

// githubComment represents a GitHub issue comment.
type githubComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
}

// syncPRDetailsFromGitHub fetches PR details, checks, and reviews and syncs them.
func syncPRDetailsFromGitHub(ctx context.Context, taskUUID uuid.UUID, owner, repo string, number int, token string, dgraphTaskInfo *dgraphStruct.DgraphTask, actor *dgraphStruct.DgraphUser) error {
	// Fetch PR details
	prData, err := fetchGitHubPullRequest(ctx, owner, repo, number, token)
	if err != nil {
		return err
	}

	prState := prData.State
	if prData.Merged {
		prState = "merged"
	}
	if err := taskDomain.UpdateTaskPRState(ctx, taskUUID, prState, "", "", boolPtr(prData.Draft)); err != nil {
		helpers.LogErrorWithContext(ctx, "syncPRDetailsFromGitHub PR state update failed: %v", err)
	}

	// Fetch check runs
	checkStatus := ""
	checkRuns, err := fetchGitHubCheckRuns(ctx, owner, repo, prData.HeadSHA, token)
	if err == nil && len(checkRuns) > 0 {
		checkStatus = aggregateCheckStatus(checkRuns)
		_ = taskDomain.UpdateTaskPRState(ctx, taskUUID, "", checkStatus, "", nil)
	}

	// Fetch reviews and update aggregate state
	reviews, err := fetchGitHubPRReviews(ctx, owner, repo, number, token)
	if err == nil && len(reviews) > 0 {
		reviewModels := make([]prReviewModel.GitHubPRReview, 0, len(reviews))
		for _, r := range reviews {
			reviewModels = append(reviewModels, prReviewModel.GitHubPRReview{
				TaskId:          taskUUID,
				GitHubLogin:     r.User.Login,
				GitHubAvatarURL: r.User.AvatarURL,
				GitHubHTMLURL:   r.User.HTMLURL,
				ReviewState:     strings.ToUpper(r.State),
				SubmittedAt:     time.Now(),
			})
		}
		_ = githubPRReviewDomain.BatchUpsertGitHubPRReview(ctx, taskUUID, reviewModels)
		updateTaskReviewState(ctx, taskUUID)
	}

	return nil
}

// fetchGitHubPullRequest fetches PR details.
func fetchGitHubPullRequest(ctx context.Context, owner, repo string, number int, token string) (*githubPRData, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d", owner, repo, number), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
	}

	var data githubPRData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode PR: %w", err)
	}
	return &data, nil
}

// githubPRData holds PR fields we care about.
type githubPRData struct {
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	HeadSHA string `json:"head_sha"` // not directly in PR, will be filled from head.sha
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// fetchGitHubCheckRuns fetches check runs for a commit.
func fetchGitHubCheckRuns(ctx context.Context, owner, repo, sha, token string) ([]githubCheckRun, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/check-runs", owner, repo, sha), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GitHub API error %d", resp.StatusCode)
	}

	var wrapper struct {
		CheckRuns []githubCheckRun `json:"check_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, err
	}
	return wrapper.CheckRuns, nil
}

// githubCheckRun represents a GitHub check run.
type githubCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// fetchGitHubPRReviews fetches reviews for a PR.
func fetchGitHubPRReviews(ctx context.Context, owner, repo string, number int, token string) ([]githubPRReview, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%d/reviews", owner, repo, number), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := githubHTTPClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkGitHubRateLimit(resp); err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GitHub API error %d", resp.StatusCode)
	}

	var reviews []githubPRReview
	if err := json.NewDecoder(resp.Body).Decode(&reviews); err != nil {
		return nil, err
	}
	return reviews, nil
}

// githubPRReview represents a GitHub PR review.
type githubPRReview struct {
	State string `json:"state"`
	User  struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		HTMLURL   string `json:"html_url"`
	} `json:"user"`
}

// aggregateCheckStatus determines the overall check status from a list of runs.
func aggregateCheckStatus(runs []githubCheckRun) string {
	hasFailure := false
	allSuccess := true
	for _, r := range runs {
		if r.Status != "completed" {
			return "pending"
		}
		switch r.Conclusion {
		case "success":
			// ok
		case "failure", "cancelled", "timed_out", "action_required":
			hasFailure = true
			allSuccess = false
		default:
			allSuccess = false
		}
	}
	if hasFailure {
		return "failure"
	}
	if allSuccess {
		return "success"
	}
	return "pending"
}

type githubSyncPayload struct {
	Status   string `json:"status,omitempty"`
	Name     string `json:"name,omitempty"`
	Desc     string `json:"description,omitempty"`
	Assignee string `json:"assignee_uuid,omitempty"`
	Label    string `json:"label,omitempty"`
}

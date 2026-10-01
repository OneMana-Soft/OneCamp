package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/GitHub"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	githubWebhookDeliveryDomain "github.com/akashc777/OneCamp/domain/GitHubWebhookDelivery"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// HandleGetGitHubConfig returns the redacted GitHub App credential config for
// the admin UI (GET /admin/github/config). Secrets are never returned.
func HandleGetGitHubConfig(w http.ResponseWriter, r *http.Request) {
	status := business.GetGitHubConfigStatus(r.Context())
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": status})
}

// HandleUpdateGitHubConfig persists admin-entered GitHub App credentials
// (POST /admin/github/config). Secret fields follow omit=keep / ""=clear /
// value=set semantics. Secrets are encrypted at rest.
func HandleUpdateGitHubConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		ClientID      *string `json:"client_id,omitempty"`
		ClientSecret  *string `json:"client_secret,omitempty"`
		WebhookSecret *string `json:"webhook_secret,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	if err := business.SaveGitHubAppConfig(ctx, body.ClientID, body.ClientSecret, body.WebhookSecret); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleUpdateGitHubConfig Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save GitHub configuration"})
		return
	}

	auditBusiness.Record(r, "integration.github_app", auditBusiness.CategoryIntegration,
		"Updated GitHub App credentials", map[string]interface{}{
			"client_id_changed":      body.ClientID != nil,
			"client_secret_changed":  body.ClientSecret != nil,
			"webhook_secret_changed": body.WebhookSecret != nil,
		})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.GetGitHubConfigStatus(ctx)})
}

// HandleGetAuthURL returns the GitHub OAuth URL for admin to initiate connection.
func HandleGetAuthURL(w http.ResponseWriter, r *http.Request) {
	clientId := business.GetGitHubAppConfig(r.Context()).ClientID
	if clientId == "" {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"error": "GitHub integration not configured"})
		return
	}

	// Redirect URI points to the BE directly — avoids cross-domain cookie issues
	redirectURI := fmt.Sprintf("https://%s/admin/github/callback", os.Getenv("BACKEND_DOMAIN"))
	authURL := fmt.Sprintf("https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s&scope=repo,read:org", clientId, redirectURI)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"auth_url": authURL})
}

// HandleCallbackGet processes the GitHub OAuth callback from a GET redirect.
func HandleCallbackGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, fmt.Sprintf("https://%s/app/admin?tab=integrations&error=no_code", os.Getenv("FE_HOST_DOMAIN")), http.StatusFound)
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok {
		http.Redirect(w, r, fmt.Sprintf("https://%s/app/admin?tab=integrations&error=unauthorized", os.Getenv("FE_HOST_DOMAIN")), http.StatusFound)
		return
	}

	err := business.ExchangeCodeAndSave(ctx, code, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleCallbackGet Failed err: %+v", err)
		http.Redirect(w, r, fmt.Sprintf("https://%s/app/admin?tab=integrations&error=exchange_failed", os.Getenv("FE_HOST_DOMAIN")), http.StatusFound)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("https://%s/app/admin?tab=integrations&success=1", os.Getenv("FE_HOST_DOMAIN")), http.StatusFound)
}

// HandleCallback exchanges the OAuth code for tokens and stores the integration.
func HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var input struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Code == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "code is required"})
		return
	}

	err := business.ExchangeCodeAndSave(ctx, input.Code, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleCallback Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "GitHub connected successfully"})
}

// HandleGetStatus returns the current GitHub integration status.
func HandleGetStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	status, err := business.GetGitHubStatus(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": status})
}

// HandleDisconnect fully disconnects GitHub integration.
func HandleDisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	err := business.DisconnectGitHub(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleDisconnect Failed err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "GitHub disconnected successfully"})
}

// HandleListRepos lists accessible repositories from the connected GitHub account.
func HandleListRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	repos, err := business.FetchRepositories(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"repos": repos})
}

// HandleLinkRepo links a GitHub repo to an OneCamp project.
func HandleLinkRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var input business.LinkRepoInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	link, err := business.LinkRepositoryToProject(ctx, input, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{"link": link})
}

// HandleUnlinkRepo unlinks a repo from a project and cleans up task metadata.
func HandleUnlinkRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}

	err = business.UnlinkRepository(ctx, linkId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Repository unlinked successfully"})
}

// HandleGetLinkedRepos returns all repos linked to a project.
func HandleGetLinkedRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	projectIdStr := chi.URLParam(r, "projectId")
	projectId, err := uuid.Parse(projectIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid project ID"})
		return
	}

	links, err := business.GetLinkedRepos(ctx, projectId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"links": links})
}

// HandleImportIssues enqueues a background import of GitHub issues
// and returns the job id immediately. Progress is queryable via
// HandleGetImportJob and updates land in github_import_jobs.
//
// Older clients that called this synchronously and expected a
// `count` field can read job.items_imported once status is
// 'completed'.
func HandleImportIssues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}

	job, err := business.EnqueueImportIssues(ctx, linkId, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{
		"msg":    "Import scheduled",
		"job":    job,
		"job_id": job.Id.String(),
	})
}

// HandleImportPRs enqueues a background import of GitHub PRs.
// Returns the job id immediately; see HandleImportIssues.
func HandleImportPRs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(userModel.UserInfo)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}

	job, err := business.EnqueueImportPRs(ctx, linkId, userInfo.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{
		"msg":    "Import scheduled",
		"job":    job,
		"job_id": job.Id.String(),
	})
}

// HandleGetImportJob returns the current state of an import job for
// the admin UI to poll.
func HandleGetImportJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	jobIdStr := chi.URLParam(r, "jobId")
	jobId, err := uuid.Parse(jobIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid job ID"})
		return
	}
	job, err := business.GetImportJob(ctx, jobId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	if job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Job not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"job": job})
}

// HandleListImportJobs returns the most recent jobs for a link so
// the admin UI can show import history.
func HandleListImportJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}
	jobs, err := business.ListImportJobs(ctx, linkId, 20)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"jobs": jobs})
}

// HandleUpdateAutomationRules updates automation rules for a linked repo.
func HandleUpdateAutomationRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}

	var input struct {
		AutomationRules map[string]string `json:"automation_rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	link, err := business.GetGitHubLinkById(ctx, linkId)
	if err != nil || link == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "Link not found"})
		return
	}

	// Drops "No change", and stores each status as a built-in key or a
	// custom status's id, so renaming the status cannot break the rule.
	rules, err := business.NormalizeAutomationRules(ctx, link.ProjectId, input.AutomationRules)
	if errors.Is(err, taskStatusBusiness.ErrUnknownStatus) {
		// Server-written, for the reader: which status was not found and which
		// ones the project has.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "unknown status", "msg": err.Error()})
		return
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "Could not check the statuses. Try again."})
		return
	}
	rulesJSON, marshalErr := json.Marshal(rules)
	if marshalErr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid automation rules"})
		return
	}
	rulesStr := string(rulesJSON)

	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := business.UpdateAutomationRulesAndInvalidate(ctx2, linkId, rulesStr); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Automation rules updated"})
}

// HandleUpdateBranchFormat updates the branch name format for a linked repo.
func HandleUpdateBranchFormat(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	linkIdStr := chi.URLParam(r, "linkId")
	linkId, err := uuid.Parse(linkIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid link ID"})
		return
	}

	var input struct {
		BranchFormat string `json:"branch_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := business.UpdateBranchFormatAndInvalidate(ctx2, linkId, input.BranchFormat); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Branch format updated"})
}

// HandleGitHubWebhook processes incoming GitHub webhook events (public, signature-verified).
func HandleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Read body
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10MB limit
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	deliveryId := r.Header.Get("X-GitHub-Delivery")

	// Verify GitHub signature using per-link secrets with global fallback
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		helpers.LogErrorWithContext(ctx, "controllers/HandleGitHubWebhook Missing signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	verified, _ := business.VerifyGitHubWebhookSignature(ctx, body, signature, eventType)
	if !verified {
		helpers.LogErrorWithContext(ctx, "controllers/HandleGitHubWebhook Invalid signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	helpers.MessageLogs.InfoLog.Printf("GitHub webhook: event=%s delivery=%s", eventType, deliveryId)

	// Dedup + claim. Returns shouldProcess=true for new deliveries AND
	// for previously-failed/still-processing rows that GitHub is
	// retrying. Only 'completed' rows short-circuit, so transient
	// failures get a real second chance instead of being silently
	// swallowed by the dedup check.
	shouldProcess, claimErr := githubWebhookDeliveryDomain.ClaimGitHubWebhookDelivery(ctx, deliveryId, eventType)
	if claimErr != nil {
		helpers.LogErrorWithContext(ctx, "controllers/HandleGitHubWebhook Failed to claim delivery: %v", claimErr)
		// Return 5xx so GitHub retries — the dedup row failure means
		// we don't even know if we'll handle it. Better to ask GitHub
		// to send it again.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !shouldProcess {
		helpers.MessageLogs.InfoLog.Printf("Skipping already-completed GitHub webhook delivery=%s", deliveryId)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Process async with detached background context and panic recovery.
	// We've already 200'd to GitHub so retries fire only on terminal
	// failure (the goroutine will mark the row 'failed' and the next
	// GitHub retry will re-claim it).
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.LogErrorWithContext(ctx, "controllers/HandleGitHubWebhook Panic recovered in webhook goroutine: %v", rec)
				_ = githubWebhookDeliveryDomain.MarkGitHubWebhookDeliveryFailed(
					context.Background(), deliveryId, fmt.Sprintf("panic: %v", rec))
			}
		}()

		procCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		if err := business.HandleGitHubWebhookEvent(procCtx, eventType, body); err != nil {
			helpers.LogErrorWithContext(procCtx, "controllers/HandleGitHubWebhook Failed to process event=%s err: %+v", eventType, err)
			_ = githubWebhookDeliveryDomain.MarkGitHubWebhookDeliveryFailed(
				context.Background(), deliveryId, err.Error())
			return
		}
		// Use a fresh background context for the success update so a
		// cancelled procCtx doesn't leave the row in 'processing'.
		_ = githubWebhookDeliveryDomain.MarkGitHubWebhookDeliveryCompleted(
			context.Background(), deliveryId)
	}()

	w.WriteHeader(http.StatusOK)
}

// HandleGetRateLimit returns the current GitHub API rate limit status.
func HandleGetRateLimit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	client, err := business.GitHubHTTPClient(ctx)
	if err != nil {
		// Treat "not connected" as a 200 with connected:false so the
		// FE rate-limit widget renders the disconnected state instead
		// of an error toast.
		if err == business.ErrGitHubNotConnected {
			helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
				"connected": false,
				"msg":       "GitHub not connected",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/rate_limit", nil)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		helpers.WriteJSON(w, resp.StatusCode, helpers.Envolope{"error": string(body)})
		return
	}

	var result struct {
		Resources struct {
			Core struct {
				Limit     int `json:"limit"`
				Remaining int `json:"remaining"`
				Reset     int `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"connected": true,
		"limit":     result.Resources.Core.Limit,
		"remaining": result.Resources.Core.Remaining,
		"reset":     result.Resources.Core.Reset,
		"percent":   float64(result.Resources.Core.Remaining) / float64(result.Resources.Core.Limit) * 100,
	})
}

// HandleGetWebhookHealth returns aggregate inbound webhook stats for
// the last 24h. Wired into the admin panel so an operator can spot a
// flaky webhook (failed deliveries, last error message) without
// reading server logs.
func HandleGetWebhookHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	summary, err := githubWebhookDeliveryDomain.GetGitHubWebhookHealth(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"health": summary})
}

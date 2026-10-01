// Package controllers exposes admin-only HTTP endpoints for the generic
// import pipeline. All routes assume the chi.Mux is wrapped with
// VerifyAuth + VerifyAdminAuthOnlyPostgres (router.go).
//
// URL shape:
//
//	/admin/import/providers                            — list installed
//	/admin/import/{provider}/jobs           POST       — create live-API job
//	/admin/import/{provider}/upload         POST       — multipart ZIP upload
//	/admin/import/{provider}/presign        POST       — presigned PUT
//	/admin/import/{provider}/finalize/{id}  POST       — after presigned PUT
//	/admin/import/{provider}/connect        POST       — token-based auth
//	/admin/import/{provider}/disconnect     POST       — revoke token
//	/admin/import/{provider}/discover       POST       — list source workspaces
//	/admin/import/jobs                                  — list across providers
//	/admin/import/jobs/{id}                             — get
//	/admin/import/jobs/{id}/plan            POST       — run the plan stage
//	/admin/import/jobs/{id}/run             POST       — start workers
//	/admin/import/jobs/{id}/cancel          POST
//	/admin/import/jobs/{id}/rollback        POST
//	/admin/import/jobs/{id}/staged-zip      DELETE
//	/admin/import/jobs/{id}/errors          GET
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	importAdapter "github.com/akashc777/OneCamp/adapter/Import"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// uploadSizeCap defends against denial-of-disk by requiring operators
// to set IMPORT_MAX_BYTES if they need >5 GB.
const defaultUploadCap int64 = 5 * 1024 * 1024 * 1024

// ─── Rate limiting ─────────────────────────────────────────────────

// checkImportRateLimit is a thin wrapper around the registry-backed
// rate limiter. The action key encodes the operation name and any
// per-provider qualifier (e.g. "discover:trello") so the registry
// surfaces tractable per-action buckets.
func checkImportRateLimit(ctx context.Context, action string, userID uuid.UUID, max int) redisStore.RateLimitResult {
	return redisStore.AllowFixedWindow(ctx,
		registry.ImportRate,
		[]string{action, userID.String()},
		max,
	)
}

func writeRateLimitExceeded(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"error": "rate limit exceeded"})
}

// ─── Auth helper ───────────────────────────────────────────────────

func requireAdmin(w http.ResponseWriter, r *http.Request) (*userModel.UserInfo, bool) {
	u := mustUser(r)
	if u == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return nil, false
	}
	if !u.UserPostgresInfo.IsAdmin {
		helpers.LogWarnWithContext(r.Context(),
			"Import %s blocked for non-admin user=%s path=%s",
			r.Method, u.UserPostgresInfo.Id, r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return nil, false
	}
	return u, true
}

func mustUser(r *http.Request) *userModel.UserInfo {
	v := r.Context().Value(helpers.UserInfoContextKey)
	if v == nil {
		return nil
	}
	if u, ok := v.(userModel.UserInfo); ok {
		return &u
	}
	if u, ok := v.(*userModel.UserInfo); ok {
		return u
	}
	return nil
}

// ─── Providers list ────────────────────────────────────────────────

// HandleListProviders returns metadata about every registered provider.
// Public to admins so the FE knows which providers are installed and
// what capabilities each has.
func HandleListProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	out := make([]importAdapter.ProviderInfo, 0, 8)
	for _, name := range importProvider.Names() {
		p := importProvider.Get(name)
		if p == nil {
			continue
		}
		out = append(out, importAdapter.ProviderInfo{
			Name:             name,
			Sources:          p.SupportedSources(),
			Capabilities:     capabilityNames(p.Capabilities()),
			DefaultStatusMap: p.DefaultStatusMap(),
			DefaultPriority:  p.DefaultPriorityMap(),
		})
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"providers": out})
}

func capabilityNames(c importProvider.Capability) []string {
	out := []string{}
	for bit, name := range capabilityNameMap {
		if c&bit != 0 {
			out = append(out, name)
		}
	}
	return out
}

var capabilityNameMap = map[importProvider.Capability]string{
	importProvider.CapTeams:        "teams",
	importProvider.CapProjects:     "projects",
	importProvider.CapTasks:        "tasks",
	importProvider.CapSubtasks:     "subtasks",
	importProvider.CapTaskComments: "task_comments",
	importProvider.CapAttachments:  "attachments",
	importProvider.CapChannels:     "channels",
	importProvider.CapDMs:          "dms",
	importProvider.CapMessages:     "messages",
	importProvider.CapReactions:    "reactions",
}

// ─── CreateJob (live-API) ──────────────────────────────────────────

// HandleCreateJob opens a new import_jobs row for a live-API source.
// No file upload is involved; the worker uses the per-user OAuth token
// stored via /connect or the OAuth callback.
func HandleCreateJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	if importProvider.Get(provName) == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "unknown provider"})
		return
	}

	if res := checkImportRateLimit(ctx, "create:"+provName, user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	var body importAdapter.CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid body"})
		return
	}
	body.SourceWorkspaceName = strings.TrimSpace(body.SourceWorkspaceName)
	if body.SourceWorkspaceName == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "source_workspace_name required"})
		return
	}
	source := body.Source
	if source == "" {
		source = importModels.SourceAPI
	}

	jobId := uuid.New()
	optionsJSON, _ := json.Marshal(body.Options)
	job := &importModels.Job{
		Id:                  jobId,
		Provider:            provName,
		SourceWorkspaceName: body.SourceWorkspaceName,
		Source:              source,
		Status:              importModels.StatusValidating,
		Options:             optionsJSON,
		TriggeredBy:         &user.UserPostgresInfo.Id,
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this provider/workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	// Eager Validate: ping the provider with the saved token so a stale
	// or missing connection surfaces immediately rather than at Plan time.
	// Failure flips the job to 'failed' with a clear message; the FE
	// shows it next to the connection card.
	prov := importProvider.Get(provName)
	if prov != nil {
		opts := importProvider.JobOptions{}
		_ = json.Unmarshal(optionsJSON, &opts)
		if err := prov.Validate(ctx, job, opts); err != nil {
			msg := err.Error()
			_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
				strPtr("failed"), &msg)
			helpers.LogWarnWithContext(ctx,
				"Import.HandleCreateJob validate failed provider=%s job=%s err=%+v",
				provName, jobId, err)
			helpers.WriteJSON(w, http.StatusBadGateway, helpers.Envolope{
				"error":  err.Error(),
				"code":   "validate_failed",
				"job_id": jobId,
			})
			return
		}
	}
	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{
		"job_id":                jobId,
		"provider":              provName,
		"source_workspace_name": body.SourceWorkspaceName,
	})
}

// ─── Upload + Presign + Finalize ───────────────────────────────────

// HandlePresignUpload returns a presigned PUT URL for ZIP-shaped
// providers (e.g., Trello board JSON, Notion ZIP). The client PUTs to
// MinIO directly, then calls /finalize.
func HandlePresignUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	if importProvider.Get(provName) == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "unknown provider"})
		return
	}
	if res := checkImportRateLimit(ctx, "presign:"+provName, user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	var body importAdapter.PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid body"})
		return
	}
	body.SourceWorkspaceName = strings.TrimSpace(body.SourceWorkspaceName)
	if body.SourceWorkspaceName == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "source_workspace_name required"})
		return
	}
	if body.FileSize <= 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "file_size must be positive"})
		return
	}
	maxBytes := uploadCap()
	if body.FileSize > maxBytes {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
			"error":     fmt.Sprintf("file exceeds IMPORT_MAX_BYTES (%d)", maxBytes),
			"max_bytes": maxBytes,
		})
		return
	}

	source := body.Source
	if source == "" {
		source = importModels.SourceExportZip
	}

	jobId := uuid.New()
	bucket := stagedZipBucket()
	objectKey := fmt.Sprintf("imports/%s/%s/raw", provName, jobId)

	presigned, err := minioInit.MinioClient.PresignedPutObject(ctx, bucket, objectKey, time.Hour)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Import presign failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "presign failed"})
		return
	}

	rawKey := objectKey
	job := &importModels.Job{
		Id:                  jobId,
		Provider:            provName,
		SourceWorkspaceName: body.SourceWorkspaceName,
		Source:              source,
		RawObjectKey:        &rawKey,
		Status:              importModels.StatusPending,
		TriggeredBy:         &user.UserPostgresInfo.Id,
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this provider/workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusCreated, importAdapter.PresignResponse{
		JobId:               jobId,
		Provider:            provName,
		SourceWorkspaceName: body.SourceWorkspaceName,
		RawObjectKey:        rawKey,
		UploadURL:           presigned.String(),
		ExpiresIn:           int64((time.Hour).Seconds()),
		Method:              http.MethodPut,
		Headers: map[string]string{
			"Content-Type": "application/octet-stream",
		},
	})
}

// HandleFinalizeUpload verifies the staged object exists and advances
// the job to 'validating' so the standard /plan endpoint can run.
func HandleFinalizeUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	if importProvider.Get(provName) == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "unknown provider"})
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	if job.Provider != provName {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "provider mismatch"})
		return
	}
	if job.TriggeredBy == nil || *job.TriggeredBy != user.UserPostgresInfo.Id {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"error": "not your upload"})
		return
	}
	if job.Status != importModels.StatusPending {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "finalize requires a pending job",
			"code":  "invalid_status",
		})
		return
	}
	if job.RawObjectKey == nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "missing raw_object_key"})
		return
	}
	bucket := stagedZipBucket()
	stat, err := minioInit.MinioClient.StatObject(ctx, bucket, *job.RawObjectKey, minio.StatObjectOptions{})
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "uploaded object not found; presigned PUT did not succeed",
			"code":  "no_object",
		})
		return
	}
	if stat.Size <= 0 {
		_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr("uploaded object is empty"))
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "uploaded object is empty"})
		return
	}
	if stat.Size > uploadCap() {
		_ = minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey, minio.RemoveObjectOptions{})
		_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
			strPtr("failed"),
			strPtr(fmt.Sprintf("uploaded object %d bytes exceeds IMPORT_MAX_BYTES (%d)",
				stat.Size, uploadCap())))
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
			"error":     fmt.Sprintf("uploaded file exceeds IMPORT_MAX_BYTES (%d)", uploadCap()),
			"max_bytes": uploadCap(),
			"actual":    stat.Size,
		})
		return
	}

	// Eager Validate before flipping to 'validating'. For ZIP-shaped
	// providers this is a cheap structural check (e.g. board JSON
	// decodes, has an id). Surfacing this synchronously gives the FE
	// a clean error instead of waiting for Plan to fail deep in the
	// pipeline.
	prov := importProvider.Get(provName)
	if prov != nil {
		opts := importProvider.JobOptions{}
		_ = json.Unmarshal(job.Options, &opts)
		if err := prov.Validate(ctx, job, opts); err != nil {
			msg := err.Error()
			_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
				strPtr("failed"), &msg)
			helpers.LogWarnWithContext(ctx,
				"Import.HandleFinalizeUpload validate failed provider=%s job=%s err=%+v",
				provName, jobId, err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"error":  err.Error(),
				"code":   "validate_failed",
				"job_id": jobId,
			})
			return
		}
	}

	if err := importModels.UpdateStatus(ctx, jobId, importModels.StatusValidating,
		strPtr("validating"), nil); err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this provider/workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"job_id": jobId,
		"size":   stat.Size,
	})
}

// ─── Plan / Run / Cancel / Rollback / Get ──────────────────────────

// HandlePlan delegates to importBusiness.BuildPlan and persists any
// operator-confirmed status / priority mappings.
func HandlePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	if job.Status != importModels.StatusValidating && job.Status != importModels.StatusPlanned {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "plan can only be run on a validating or planned job",
			"code":  "invalid_status",
		})
		return
	}
	var body importAdapter.PlanRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Options != nil {
		if optsJSON, err := json.Marshal(body.Options); err == nil {
			_ = importModels.UpdateOptions(ctx, jobId, optsJSON)
		}
	}
	if body.StatusMappings != nil {
		_ = importModels.SetStatusMappings(ctx, jobId, body.StatusMappings)
	}
	if body.PriorityMappings != nil {
		_ = importModels.SetPriorityMappings(ctx, jobId, body.PriorityMappings)
	}
	plan, err := importBusiness.BuildPlan(ctx, jobId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, plan)
}

// HandleRun starts the orchestrator for a planned (or failed-and-retry) job.
func HandleRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkImportRateLimit(ctx, "run", user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	if job.Status != importModels.StatusPlanned && job.Status != importModels.StatusFailed {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "run requires a planned or failed job",
			"code":  "invalid_status",
		})
		return
	}
	var body importAdapter.RunRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Options != nil {
		if optsJSON, err := json.Marshal(*body.Options); err == nil {
			_ = importModels.UpdateOptions(ctx, jobId, optsJSON)
		}
	}
	if body.StatusMappings != nil {
		_ = importModels.SetStatusMappings(ctx, jobId, body.StatusMappings)
	}
	if body.PriorityMappings != nil {
		_ = importModels.SetPriorityMappings(ctx, jobId, body.PriorityMappings)
	}
	if err := importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning,
		strPtr("queued"), nil); err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this provider/workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	go importBusiness.RunImport(context.Background(), jobId, user)
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"job_id": jobId})
}

// HandleCancel signals a running import to stop.
func HandleCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkImportRateLimit(ctx, "cancel", user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	if err := importBusiness.CancelImport(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// HandleRollback walks the id_map and soft-deletes everything created
// by this import. Strongly destructive; rate-limited heavily.
func HandleRollback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkImportRateLimit(ctx, "rollback", user.UserPostgresInfo.Id, 2); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	if err := importBusiness.RollbackImport(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// HandleListJobs returns recent jobs across all providers, or a single
// provider if `?provider=…` is set. Supports `?limit` (default 50,
// max 200) and `?offset` for pagination.
func HandleListJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	provider := r.URL.Query().Get("provider")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	jobs, err := importDomain.ListJobs(ctx, provider, limit)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	views := make([]importAdapter.JobView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, jobToView(ctx, j))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"jobs":   views,
		"limit":  limit,
		"offset": offset,
	})
}

// HandleGetJob returns the full view of a single job.
func HandleGetJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	j, err := importModels.GetJob(ctx, jobId)
	if err != nil || j == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, jobToView(ctx, j))
}

// HandleListErrors returns paginated error log entries.
func HandleListErrors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	severity := r.URL.Query().Get("severity")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	errs, err := importModels.ListErrors(ctx, jobId, severity, limit, offset)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"errors": errs})
}

// HandleDeleteStagedZip removes the staged file from MinIO. Idempotent.
func HandleDeleteStagedZip(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkImportRateLimit(ctx, "delete_zip", user.UserPostgresInfo.Id, 10); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	switch job.Status {
	case importModels.StatusRunning, importModels.StatusValidating, importModels.StatusPaused:
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "cannot delete staged zip while the job is active",
			"code":  "job_active",
		})
		return
	}
	if err := importBusiness.DeleteStagedZip(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// ─── Connect / Disconnect (token-based providers) ──────────────────

// HandleConnect stores an encrypted access token for the calling admin.
// Used by Trello (API key + token), Asana PAT, Jira PAT, etc.
func HandleConnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	if provName == importModels.ProviderSlack {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "slack does not use token connect"})
		return
	}
	if importProvider.Get(provName) == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "unknown provider"})
		return
	}
	if res := checkImportRateLimit(ctx, "connect:"+provName, user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	var body importAdapter.ConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid body"})
		return
	}
	body.AccessToken = strings.TrimSpace(body.AccessToken)
	if body.AccessToken == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "access_token required"})
		return
	}
	tok := &importModels.Token{
		Provider:     provName,
		OwnerUserId:  user.UserPostgresInfo.Id,
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
	}
	if body.SourceAccountId != "" {
		tok.SourceAccountId = &body.SourceAccountId
	}
	if body.SourceAccountName != "" {
		tok.SourceAccountName = &body.SourceAccountName
	}
	if body.Scopes != "" {
		tok.Scopes = &body.Scopes
	}
	if body.ExpiresAtUnix > 0 {
		t := time.Unix(body.ExpiresAtUnix, 0)
		tok.ExpiresAt = &t
	}
	if len(body.Metadata) > 0 {
		md, _ := json.Marshal(body.Metadata)
		tok.Metadata = md
	}
	if err := importModels.SaveToken(ctx, tok); err != nil {
		helpers.LogErrorWithContext(ctx, "Import.HandleConnect SaveToken err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "could not save token"})
		return
	}
	// New token = potentially different scope/account, so the cached
	// discovery list is stale. Drop it.
	_ = redisStore.Delete(ctx, registry.ImportDiscover, []string{provName, user.UserPostgresInfo.Id.String()})
	importProvider.InvalidateImportTokenSource(provName, user.UserPostgresInfo.Id)
	helpers.WriteJSON(w, http.StatusOK, importAdapter.ConnectResponse{
		Provider:          provName,
		SourceAccountName: body.SourceAccountName,
		ExpiresAt:         tok.ExpiresAt,
		Scopes:            tok.Scopes,
	})
}

// HandleDisconnect deletes the per-provider token row.
func HandleDisconnect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	if err := importModels.DeleteToken(ctx, provName, user.UserPostgresInfo.Id); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	// Token gone → discovery cache must go too, otherwise a re-connect
	// to a different account would surface the previous user's data.
	_ = redisStore.Delete(ctx, registry.ImportDiscover, []string{provName, user.UserPostgresInfo.Id.String()})
	importProvider.InvalidateImportTokenSource(provName, user.UserPostgresInfo.Id)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// HandleListConnections returns the connections this admin has.
func HandleListConnections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	tokens, err := importModels.ListTokens(ctx, user.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"connections": tokens})
}

// ─── helpers ───────────────────────────────────────────────────────

func jobToView(ctx context.Context, j *importModels.Job) importAdapter.JobView {
	view := importAdapter.JobView{
		Id:                  j.Id,
		Provider:            j.Provider,
		SourceWorkspaceName: j.SourceWorkspaceName,
		Source:              j.Source,
		Status:              j.Status,
		Stage:               j.Stage,
		StartedAt:           j.StartedAt,
		CompletedAt:         j.CompletedAt,
		ErrorMessage:        j.ErrorMessage,
		TriggeredBy:         j.TriggeredBy,
		CreatedAt:           j.CreatedAt,
		UpdatedAt:           j.UpdatedAt,
		Progress:            j.Progress,
	}
	_ = json.Unmarshal(j.Options, &view.Options)
	if len(j.Plan) > 0 {
		var plan importProvider.Plan
		if err := json.Unmarshal(j.Plan, &plan); err == nil {
			view.Plan = &plan
		}
	}
	view.ChunksTotal, view.ChunksDone, view.ChunksFailed, _ = importModels.CountChunks(ctx, j.Id)
	view.ItemsImported, _ = importModels.SumItemsImported(ctx, j.Id)
	view.ErrorsTotal, _ = importModels.CountErrors(ctx, j.Id)
	view.StatusMappings, _ = importModels.GetStatusMappings(ctx, j.Id)
	view.PriorityMappings, _ = importModels.GetPriorityMappings(ctx, j.Id)
	return view
}

func uploadCap() int64 {
	if v := os.Getenv("IMPORT_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultUploadCap
}

func stagedZipBucket() string {
	return importBusiness.StagedZipBucket()
}

func strPtr(s string) *string { return &s }

// HandleDiscover lists resources visible to the admin's connected
// provider token (Trello boards, Asana workspaces, Jira projects,
// Notion task databases, Todoist projects). Used by the FE to populate
// the "pick a workspace/board" dropdown when creating a new job.
//
// Returns 404 if the provider doesn't implement the optional
// Discoverer interface (e.g., Slack).
//
// Results are cached in Redis for 5 minutes per (provider, owner_user_id)
// to bound outbound API calls. Repeatedly opening the connect dialog
// — a common pattern when the admin is comparing providers — used to
// burn one Asana / Jira / Notion API roundtrip per click. The cache
// is invalidated on connect (token rotation) and disconnect (token
// removal) so a freshly-rotated token never serves stale results.
func HandleDiscover(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	provName := chi.URLParam(r, "provider")
	prov := importProvider.Get(provName)
	if prov == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "unknown provider"})
		return
	}
	disc, ok := prov.(importProvider.Discoverer)
	if !ok {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{
			"error": "provider does not support discovery",
			"code":  "no_discover",
		})
		return
	}
	if res := checkImportRateLimit(ctx, "discover:"+provName, user.UserPostgresInfo.Id, 30); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	// Cache fast path. Hits Redis only — no provider API call.
	var cached []importProvider.DiscoverItem
	if hit, _ := redisStore.GetJSON(ctx, registry.ImportDiscover, []string{provName, user.UserPostgresInfo.Id.String()}, &cached); hit {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"items": cached, "cached": true})
		return
	}

	tok, err := importModels.LoadToken(ctx, provName, user.UserPostgresInfo.Id)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "no token saved for this provider; connect first",
			"code":  "no_token",
		})
		return
	}
	items, err := disc.Discover(ctx, user.UserPostgresInfo.Id.String(), tok)
	if err != nil {
		// Surface auth-flavoured errors as 401 so the FE prompts to reconnect.
		helpers.LogWarnWithContext(ctx, "Import discover failed provider=%s err=%+v", provName, err)
		helpers.WriteJSON(w, http.StatusBadGateway, helpers.Envolope{"error": err.Error()})
		return
	}

	// Best-effort cache write. A Redis hiccup just means the next call
	// repeats the upstream API hit, not a correctness issue.
	_ = redisStore.SetJSON(ctx, registry.ImportDiscover, []string{provName, user.UserPostgresInfo.Id.String()}, items)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"items": items})
}

// HandleRetryFailedChunks resets every failed chunk in a job to
// pending with attempts=0, then re-runs the orchestrator if the job
// is in a terminal state. Used by an admin "retry stuck chunks"
// button when a transient upstream blip burned a chunk's retry budget.
//
// Status precondition: the job must NOT currently be running (we don't
// race the worker pool against the reset). Operators cancel first if
// needed.
func HandleRetryFailedChunks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkImportRateLimit(ctx, "retry", user.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	switch job.Status {
	case importModels.StatusRunning, importModels.StatusValidating, importModels.StatusPaused:
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "cannot retry while the job is active; cancel first",
			"code":  "job_active",
		})
		return
	}
	n, err := importModels.RetryFailedChunks(ctx, jobId)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	if n == 0 {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"reset": 0, "rerun": false})
		return
	}
	// Flip job to running and kick a fresh orchestrator goroutine. The
	// orchestrator picks up only the (now-reset) pending chunks for
	// each stage, so completed work isn't redone.
	if err := importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning,
		strPtr("queued"), nil); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	go importBusiness.RunImport(context.Background(), jobId, user)
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"reset": n, "rerun": true})
}

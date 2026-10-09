// Package controllers exposes admin-only HTTP endpoints for the Slack
// import pipeline. All endpoints assume the chi.Mux is wrapped with
// VerifyAuth + VerifyAdminAuthOnlyPostgres.
package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	importAdapter "github.com/akashc777/OneCamp/adapter/SlackImport"
	importBusiness "github.com/akashc777/OneCamp/business/Import"
	business "github.com/akashc777/OneCamp/business/SlackImport"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	"github.com/akashc777/OneCamp/helpers/zipsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// uploadSizeCap defends against denial-of-disk by requiring operators to
// set EXPORT_MAX_BYTES if they need >5 GB. Most workspace exports fit.
const defaultUploadCap int64 = 5 * 1024 * 1024 * 1024 // 5 GB

// slackUploadCap is the largest export this server takes: EXPORT_MAX_BYTES,
// or 5 GB.
func slackUploadCap() int64 {
	if v := os.Getenv("EXPORT_MAX_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultUploadCap
}

// tooLarge is the answer for an export over the cap, in sizes people read.
func tooLarge(size, limit int64) string {
	return fmt.Sprintf("That export is %s, and this server takes exports up to %s. "+
		"Whoever runs it can raise the limit (EXPORT_MAX_BYTES).", helpers.ReadableBytes(size), helpers.ReadableBytes(limit))
}

// HandleLimits returns the largest export this server takes, so the upload
// dialog says the real limit instead of one of its own.
func HandleLimits(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"max_bytes": slackUploadCap()})
}

// checkSlackImportRateLimit is a thin wrapper around the registry-
// backed rate limiter so each call site stays a single line.
func checkSlackImportRateLimit(ctx context.Context, action string, userID uuid.UUID, max int) redisStore.RateLimitResult {
	return redisStore.AllowFixedWindow(ctx,
		registry.SlackImportRate,
		[]string{action, userID.String()},
		max,
	)
}

func writeRateLimitExceeded(w http.ResponseWriter, retryAfterSeconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"error": "rate limit exceeded"})
}

// HandleUpload accepts a multipart upload of a Slack workspace export
// ZIP and stages it in MinIO under slackImport/<jobId>/raw.zip.
//
// Request: multipart/form-data with fields:
//   - slack_workspace_name (string, required) — the SOURCE Slack workspace
//     name (e.g. "Acme Inc."). OneCamp itself is single-tenant; this
//     value labels which Slack workspace the export came from so re-imports
//     of the same workspace can dedup against earlier ones.
//   - source         (string, optional; defaults to export_zip)
//   - file           (file, required)
//
// Response: 201 Created { job_id, slack_workspace_name, raw_object_key }
func HandleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}

	// Rate limit: 3 uploads per minute per user. Uploads are heavy.
	if res := checkSlackImportRateLimit(ctx, "upload", userInfo.UserPostgresInfo.Id, 3); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	cap := slackUploadCap()
	r.Body = http.MaxBytesReader(w, r.Body, cap)

	if err := r.ParseMultipartForm(64 * 1024 * 1024); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{"error": tooLarge(r.ContentLength, cap), "max_bytes": cap})
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "could not parse multipart: " + err.Error()})
		return
	}

	workspace := strings.TrimSpace(r.FormValue("slack_workspace_name"))
	if workspace == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "slack_workspace_name is required"})
		return
	}
	source := r.FormValue("source")
	if source == "" {
		source = importModels.SourceExportZip
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "file field is required"})
		return
	}
	defer file.Close()

	// Magic-byte verification: peek the leading bytes of the upload
	// before we send a single byte to MinIO. The Go service is the
	// only place we can enforce this on the multipart path; the
	// presigned-PUT path verifies after the fact (see HandleFinalizeUpload).
	//
	// uploadsafe.PeekAndIsZip returns a "rewound" reader with the
	// peeked prefix re-prepended so PutObject still uploads the full
	// file. If the prefix isn't a PKZIP signature we refuse the
	// upload entirely — it cannot be a Slack export.
	isZip, body, err := uploadsafe.PeekAndIsZip(file)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "empty or unreadable upload"})
		return
	}
	if !isZip {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "uploaded file is not a ZIP archive (PKZIP signature missing)",
			"code":  "not_a_zip",
		})
		return
	}

	jobId := uuid.New()
	bucket := helpers.UserUploadBucket()
	objectKey := fmt.Sprintf("slackImport/%s/raw.zip", jobId)

	if _, err := minioInit.MinioClient.PutObject(ctx, bucket, objectKey, body, hdr.Size,
		minio.PutObjectOptions{ContentType: "application/zip"}); err != nil {
		helpers.LogErrorWithContext(ctx, "SlackImport upload failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "minio upload failed"})
		return
	}

	rawKey := objectKey
	job := &importModels.Job{
		Id:                 jobId,
		SlackWorkspaceName: workspace,
		Source:             source,
		RawObjectKey:       &rawKey,
		Status:             importModels.StatusValidating,
		TriggeredBy:        &userInfo.UserPostgresInfo.Id,
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		_ = minioInit.MinioClient.RemoveObject(ctx, bucket, objectKey, minio.RemoveObjectOptions{})
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "could not create job"})
		return
	}

	// Hash + dedup check. Identical to the presigned-PUT finalize path,
	// kept inline here so a small upload that goes through this endpoint
	// doesn't bypass duplicate detection.
	if hash, err := business.HashStagedZip(ctx, rawKey); err == nil && hash != "" {
		if dup, err := importModels.FindCompletedJobByHash(ctx, workspace, hash); err == nil && dup != nil && dup.Id != jobId {
			_ = minioInit.MinioClient.RemoveObject(ctx, bucket, objectKey, minio.RemoveObjectOptions{})
			_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
				strPtr("failed"),
				strPtr(fmt.Sprintf("duplicate upload: identical file already imported as job %s", dup.Id)))
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error":           "this exact export was already uploaded",
				"code":            "duplicate_upload",
				"existing_job_id": dup.Id,
				"existing_status": dup.Status,
			})
			return
		}
		_ = importModels.SetContentHash(ctx, jobId, hash)
	}

	helpers.WriteJSON(w, http.StatusCreated, importAdapter.UploadResponse{
		JobId:              jobId,
		SlackWorkspaceName: workspace,
		RawObjectKey:       rawKey,
	})
}

// verifyStagedZipMagic reads the leading 4 bytes of a staged MinIO
// object and returns nil only when the bytes match a PKZIP signature.
// One ranged GET, no full download, no disk I/O.
//
// Used by the presigned-upload finalize path because the Go service
// never sees the bytes during the actual PUT — we have to confirm
// after the fact that the client uploaded what it promised.
func verifyStagedZipMagic(ctx context.Context, bucket, objectKey string) error {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(0, 3); err != nil {
		return fmt.Errorf("set range: %w", err)
	}
	obj, err := minioInit.MinioClient.GetObject(ctx, bucket, objectKey, opts)
	if err != nil {
		return fmt.Errorf("range get: %w", err)
	}
	defer obj.Close()
	var head [4]byte
	if _, err := io.ReadFull(obj, head[:]); err != nil {
		return fmt.Errorf("read prefix: %w", err)
	}
	if !zipsafe.IsZipMagic(head[:]) {
		return fmt.Errorf("uploaded blob is not a PKZIP archive (got %x)", head)
	}
	return nil
}

// keep bytes import live for future diagnostic helpers.
var _ = bytes.Equal

// HandlePlan parses the staged ZIP, computes counts/conflicts, and stores
// the plan on the job row. Returns the plan.
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

	var body importAdapter.PlanRequest
	_ = json.NewDecoder(r.Body).Decode(&body)

	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": "job not found"})
		return
	}
	switch job.Status {
	case importModels.StatusValidating, importModels.StatusPlanned:
	case importModels.StatusFailed:
		// Planned again. The export has to still be there, and the job takes
		// its workspace's label back, which another import may hold by now.
		if job.RawObjectKey == nil || *job.RawObjectKey == "" {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "The uploaded export is gone (uploads are cleared a week after an import ends). Upload it again to start a new import.",
				"code":  "file_gone",
			})
			return
		}
		// From failed only, decided where it's written: Run may have started
		// it since it was read, and a running import put back to waiting was
		// started a second time.
		reopened, err := importModels.UpdateStatusFrom(ctx, jobId, []string{importModels.StatusFailed},
			importModels.StatusValidating, strPtr("validating"), nil)
		switch {
		case errors.Is(err, importModels.ErrConflictActiveJob):
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "Another import of this Slack workspace is waiting or running. Finish or discard it first.",
				"code":  "active_job",
			})
			return
		case err != nil:
			helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"error": "Couldn't reopen this import. Try again."})
			return
		case !reopened:
			writeJobChanged(w)
			return
		}
	default:
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "Only an uploaded export waiting to be planned, or an import that failed, can be planned.",
			"code":  "invalid_status",
		})
		return
	}

	// Persist updated options if supplied.
	if optionsJSON, err := json.Marshal(body.Options); err == nil {
		_ = importModels.UpdateOptions(ctx, jobId, optionsJSON)
	}

	arc, cleanup, err := openMinioZipForRead(ctx, *job.RawObjectKey)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "could not open zip: " + err.Error()})
		return
	}
	defer cleanup()

	plan, err := business.BuildPlan(ctx, jobId, arc)
	if errors.Is(err, importModels.ErrJobChanged) {
		writeJobChanged(w)
		return
	}
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, plan)
}

// writeJobChanged answers a request for an import that moved on in the
// meantime (Run started it from another tab, or it was discarded while its
// export uploaded), as the other imports' do.
func writeJobChanged(w http.ResponseWriter) {
	p := importBusiness.JobChanged
	helpers.WriteJSON(w, p.Status, helpers.Envolope{"error": p.Msg, "code": p.Code})
}

// HandleRun starts the import pipeline asynchronously. Returns 202 with
// the job id; the FE subscribes to MQTT for live progress.
func HandleRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}

	if res := checkSlackImportRateLimit(ctx, "run", userInfo.UserPostgresInfo.Id, 5); !res.Allowed {
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
		if optionsJSON, err := json.Marshal(*body.Options); err == nil {
			_ = importModels.UpdateOptions(ctx, jobId, optionsJSON)
		}
	}

	// Started in one statement, from planned or failed: the status read above
	// can be stale by now, and two clicks used to start two runs of the same
	// import (every channel made twice, and Cancel stopping one of them).
	started, err := importModels.StartRunning(ctx, jobId, []string{importModels.StatusPlanned, importModels.StatusFailed})
	if err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	if !started {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "this import has already been started",
			"code":  "invalid_status",
		})
		return
	}

	go business.RunImport(context.Background(), jobId, userInfo)
	helpers.WriteJSON(w, http.StatusAccepted, helpers.Envolope{"job_id": jobId})
}

// HandleCancel signals a running job to stop.
func HandleCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkSlackImportRateLimit(ctx, "cancel", userInfo.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	if err := business.CancelImport(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// HandleRollback walks the id_map and soft-deletes every entity created
// by this import. Strongly destructive; rate-limited heavily.
func HandleRollback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkSlackImportRateLimit(ctx, "rollback", userInfo.UserPostgresInfo.Id, 2); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}
	jobId, err := uuid.Parse(chi.URLParam(r, "jobId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid jobId"})
		return
	}
	if err := business.RollbackImport(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

// HandleListJobs returns up to 50 recent jobs with their summary counts.
func HandleListJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	jobs, err := importModels.ListJobs(ctx, 50)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	views := make([]importAdapter.JobView, 0, len(jobs))
	for _, j := range jobs {
		views = append(views, jobToView(ctx, j))
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"jobs": views})
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

// --- helpers --------------------------------------------------------------

func jobToView(ctx context.Context, j *importModels.Job) importAdapter.JobView {
	view := importAdapter.JobView{
		Id:                 j.Id,
		SlackWorkspaceName: j.SlackWorkspaceName,
		Source:             j.Source,
		Status:             j.Status,
		Stage:              j.Stage,
		StartedAt:          j.StartedAt,
		CompletedAt:        j.CompletedAt,
		ErrorMessage:       j.ErrorMessage,
		Digest:             j.Digest,
		TriggeredBy:        j.TriggeredBy,
		CreatedAt:          j.CreatedAt,
		UpdatedAt:          j.UpdatedAt,
		Progress:           j.Progress,
	}
	_ = json.Unmarshal(j.Options, &view.Options)
	if len(j.Plan) > 0 {
		var plan importAdapter.PlanResponse
		if err := json.Unmarshal(j.Plan, &plan); err == nil {
			view.Plan = &plan
		}
	}
	view.ChunksTotal, view.ChunksDone, view.ChunksFailed, _ = importModels.CountChunks(ctx, j.Id)
	view.ItemsImported, _ = importModels.SumItemsImported(ctx, j.Id)
	view.ErrorsTotal, _ = importModels.CountErrors(ctx, j.Id)
	return view
}

// openMinioZipForRead exposes the staged ZIP as a *business.Archive. We use
// HTTP range requests directly against MinIO so imports of any size
// (including >10 GB Corporate exports) work without staging the file
// to local disk. See business/SlackImport/minio_reader.go for details.
//
// The cleanup callback is a no-op now that there's no temp file, but
// the signature is kept so callers can defer it the same way regardless
// of which backend the staged zip lives on.
func openMinioZipForRead(ctx context.Context, objectKey string) (*business.Archive, func(), error) {
	arc, cleanup, err := business.OpenStagedZipFromMinIO(ctx, objectKey)
	if err != nil {
		return nil, nil, err
	}
	return arc, cleanup, nil
}

// mustUser extracts the UserInfo set by VerifyAuth (and required to be
// admin by VerifyAdminAuthOnlyPostgres before this handler ever runs).
//
// Returns nil only if the middleware chain didn't run as expected — a
// real defence-in-depth check rather than a typical code path. The
// admin gate at the router level (`VerifyAdminAuthOnlyPostgres`) is
// what enforces "only org admins can use Slack import". A non-admin
// user gets a 403 before reaching this controller.
// requireAdmin is a defence-in-depth check on top of the router-level
// VerifyAdminAuthOnlyPostgres gate. It rejects any request whose
// authenticated user does not have IsAdmin=true on the postgres
// users row. The router already enforces this, but pinning the same
// check at the handler level means a future refactor that moves a
// route out of the admin group still fails closed instead of silently
// granting non-admin access to a destructive operation.
//
// Returns true when the user is allowed; writes 401/403 and returns
// false otherwise.
func requireAdmin(w http.ResponseWriter, r *http.Request) (*userModel.UserInfo, bool) {
	userInfo := mustUser(r)
	if userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return nil, false
	}
	if !userInfo.UserPostgresInfo.IsAdmin {
		helpers.LogWarnWithContext(r.Context(),
			"SlackImport %s blocked for non-admin user=%s path=%s",
			r.Method, userInfo.UserPostgresInfo.Id, r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return nil, false
	}
	return userInfo, true
}

func mustUser(r *http.Request) *userModel.UserInfo {
	v := r.Context().Value(helpers.UserInfoContextKey)
	if v == nil {
		return nil
	}
	// Other controllers across the codebase store UserInfo as a value;
	// accept both shapes so a future middleware refactor doesn't silently
	// break this controller.
	if u, ok := v.(userModel.UserInfo); ok {
		return &u
	}
	if u, ok := v.(*userModel.UserInfo); ok {
		return u
	}
	return nil
}

func strPtr(s string) *string { return &s }

// HandlePresignUpload returns a presigned PUT URL to MinIO so the
// browser uploads the export ZIP directly. This bypasses the Go
// service entirely, which means:
//
//  1. No size cap from http.MaxBytesReader on the API edge.
//  2. The Go service never holds GBs of upload bytes in memory.
//  3. Multi-GB uploads get MinIO's native multipart resumption.
//
// Use case: any export over a few GB. The standard /upload endpoint
// is still fine for smaller files and a simpler client flow; it
// remains the default in the FE upload dialog.
//
// Flow:
//
//  1. Client → POST /admin/import/slack/presign  (slack_workspace_name, file_size)
//  2. Server validates size, allocates a job id, returns a presigned
//     PUT URL plus the object_key the client should report back.
//  3. Client → PUT <presigned URL> (uploads file directly to MinIO)
//  4. Client → POST /admin/import/slack/finalize-upload (job_id)
//  5. Server marks the job as validating and resumes the standard
//     plan/run pipeline.
//
// Security: the presigned URL is bound to a single object_key and
// expires in 1 hour. The client cannot use it to overwrite arbitrary
// objects.
func HandlePresignUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}

	if res := checkSlackImportRateLimit(ctx, "presign", userInfo.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	var body struct {
		SlackWorkspaceName string `json:"slack_workspace_name"`
		Source             string `json:"source"`
		FileSize           int64  `json:"file_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "invalid body"})
		return
	}
	body.SlackWorkspaceName = strings.TrimSpace(body.SlackWorkspaceName)
	if body.SlackWorkspaceName == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "slack_workspace_name is required"})
		return
	}
	if body.FileSize <= 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "file_size must be positive"})
		return
	}

	// Honour EXPORT_MAX_BYTES even on the presigned path. Operators who
	// want huge uploads bump this env var explicitly.
	cap := slackUploadCap()
	if body.FileSize > cap {
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
			"error":     tooLarge(body.FileSize, cap),
			"max_bytes": cap,
		})
		return
	}

	source := body.Source
	if source == "" {
		source = importModels.SourceExportZip
	}

	jobId := uuid.New()
	bucket := helpers.UserUploadBucket()
	objectKey := fmt.Sprintf("slackImport/%s/raw.zip", jobId)

	// 1 hour is enough for even a transcontinental 50 GB upload over a
	// 100 Mbps link; a slow client with worse bandwidth should request
	// a fresh presign by re-calling this endpoint and the previous job
	// will be left in 'validating' (the client can call cancel/cleanup).
	presigned, err := minioInit.MinioClient.PresignedPutObject(ctx, bucket, objectKey, time.Hour)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackImport presign failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "presign failed"})
		return
	}

	rawKey := objectKey
	job := &importModels.Job{
		Id:                 jobId,
		SlackWorkspaceName: body.SlackWorkspaceName,
		Source:             source,
		RawObjectKey:       &rawKey,
		Status:             importModels.StatusPending, // becomes 'validating' after finalize
		TriggeredBy:        &userInfo.UserPostgresInfo.Id,
	}
	if err := importModels.CreateJob(ctx, job); err != nil {
		if errors.Is(err, importModels.ErrConflictActiveJob) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"error": "another import is already active for this workspace",
				"code":  "active_job",
			})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": "could not create job"})
		return
	}

	helpers.WriteJSON(w, http.StatusCreated, helpers.Envolope{
		"job_id":               jobId,
		"slack_workspace_name": body.SlackWorkspaceName,
		"raw_object_key":       rawKey,
		"upload_url":           presigned.String(),
		"expires_in":           int64((time.Hour).Seconds()),
		"method":               http.MethodPut,
		"headers": map[string]string{
			// MinIO requires the Content-Type be set on the PUT and match
			// the presign signature. The presign itself doesn't pin a
			// Content-Type, so any value works as long as the client uses
			// the same one consistently — application/zip is the natural
			// fit for an export archive.
			"Content-Type": "application/zip",
		},
	})
}

// HandleFinalizeUpload is called by the client after a successful
// presigned PUT. It verifies the object exists at the expected key
// and matches the size declared at presign time, then advances the job
// from 'pending' to 'validating' so the standard /plan endpoint can be
// called.
func HandleFinalizeUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
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
	if job.TriggeredBy == nil || *job.TriggeredBy != userInfo.UserPostgresInfo.Id {
		// Cross-user finalise is forbidden so a malicious admin can't
		// hijack another admin's presign window. The endpoint is
		// already admin-only; this is defence in depth.
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

	bucket := helpers.UserUploadBucket()

	stat, err := minioInit.MinioClient.StatObject(ctx, bucket, *job.RawObjectKey, minio.StatObjectOptions{})
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "uploaded object not found; presigned PUT did not succeed",
			"code":  "no_object",
		})
		return
	}
	if stat.Size <= 0 {
		// Roll the job out of pending so the workspace can take new uploads.
		_, _ = leavePending(ctx, jobId, importModels.StatusFailed, strPtr("uploaded object is empty"))
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "uploaded object is empty",
		})
		return
	}

	// Enforce the size cap against the actual uploaded object. The presign
	// endpoint validates the declared file_size, but a client could PUT a
	// larger file to the same presigned URL (MinIO does not enforce a
	// per-object size cap on presigned PUT). Reject + delete here so a
	// rogue or buggy client cannot DOS the bucket.
	uploadCap := slackUploadCap()
	if stat.Size > uploadCap {
		_ = minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey, minio.RemoveObjectOptions{})
		_, _ = leavePending(ctx, jobId, importModels.StatusFailed, strPtr(tooLarge(stat.Size, uploadCap)))
		helpers.WriteJSON(w, http.StatusRequestEntityTooLarge, helpers.Envolope{
			"error":     tooLarge(stat.Size, uploadCap),
			"max_bytes": uploadCap,
			"actual":    stat.Size,
		})
		return
	}

	// Magic-byte verification — defence against a client that
	// presigns a slot for a "Slack export" and then PUTs garbage to
	// fill our bucket. One ranged GET, no full download.
	if err := verifyStagedZipMagic(ctx, bucket, *job.RawObjectKey); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport finalize: magic-byte check failed for job=%s: %+v", jobId, err)
		_ = minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey, minio.RemoveObjectOptions{})
		_, _ = leavePending(ctx, jobId, importModels.StatusFailed, strPtr("uploaded blob is not a PKZIP archive"))
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"error": "uploaded file is not a ZIP archive (PKZIP signature missing)",
			"code":  "not_a_zip",
		})
		return
	}

	// Hash the staged ZIP so we can detect duplicate uploads. Streaming
	// SHA-256 against MinIO; doesn't touch disk. For multi-GB files this
	// can take a few seconds — the operator already waited through the
	// upload, so the additional latency here is acceptable.
	hash, err := business.HashStagedZip(ctx, *job.RawObjectKey)
	if err != nil {
		// Hashing failure is non-fatal. We log and proceed without dedup;
		// content-level dedup at the workspace map will still catch most
		// re-imports. Better to let the import run than to block on a
		// transient MinIO read hiccup.
		helpers.LogWarnWithContext(ctx,
			"SlackImport finalize: hashing failed (proceeding without file-level dedup): %+v", err)
	} else if dup, err := importModels.FindCompletedJobByHash(ctx, job.SlackWorkspaceName, hash); err == nil && dup != nil && dup.Id != jobId {
		// Same workspace + same content_hash + a non-failed prior job.
		// Fail this job fast and tell the operator about the existing one.
		// We delete the staged duplicate ZIP from MinIO so storage doesn't bloat.
		_ = minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey, minio.RemoveObjectOptions{})
		_, _ = leavePending(ctx, jobId, importModels.StatusFailed,
			strPtr(fmt.Sprintf("duplicate upload: identical file already imported as job %s", dup.Id)))
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error":           "this exact export was already uploaded",
			"code":            "duplicate_upload",
			"existing_job_id": dup.Id,
			"existing_status": dup.Status,
		})
		return
	} else if hash != "" {
		// Persist the hash so subsequent uploads can dedup against this one.
		if err := importModels.SetContentHash(ctx, jobId, hash); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport finalize: could not persist content_hash: %+v", err)
		}
	}

	moved, err := leavePending(ctx, jobId, importModels.StatusValidating, nil)
	switch {
	case errors.Is(err, importModels.ErrConflictActiveJob):
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"error": "another import is already active for this workspace",
			"code":  "active_job",
		})
		return
	case err != nil:
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	case !moved:
		writeJobChanged(w)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"job_id": jobId,
		"size":   stat.Size,
	})
}

// leavePending moves a job waiting for its export on to status (its stage
// named the same), and reports whether it did. From pending only: an import
// discarded while its export uploaded was brought back to waiting, or failed,
// by the upload finishing.
func leavePending(ctx context.Context, jobId uuid.UUID, status string, errMsg *string) (bool, error) {
	return importModels.UpdateStatusFrom(ctx, jobId, []string{importModels.StatusPending}, status, strPtr(status), errMsg)
}

// HandleDeleteStagedZip removes the staged Slack export ZIP from MinIO
// before the cleanup loop's retention window expires. Operators reach
// for this when they're tight on storage and don't plan to retry.
//
// Idempotent: calling it twice on the same job is a no-op.
//
// Status guard: only allowed on terminal jobs. Deleting the source
// while a job is still running would brick the workers.
func HandleDeleteStagedZip(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo, ok := requireAdmin(w, r)
	if !ok {
		return
	}
	if res := checkSlackImportRateLimit(ctx, "delete_zip", userInfo.UserPostgresInfo.Id, 10); !res.Allowed {
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
	if err := business.DeleteStagedZip(ctx, jobId); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"ok": true})
}

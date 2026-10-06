package business

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// Worker pool sizes are env-tunable so operators can dial throughput per
// hardware. The defaults are conservative for a 4-vCPU box.
var (
	messageWorkers = envIntDefault("SLACK_IMPORT_MSG_WORKERS", 4)
	fileWorkers    = envIntDefault("SLACK_IMPORT_FILE_WORKERS", 2)
)

// activeJobs tracks the cancellation channel for each running import,
// keyed by job id. Used by Cancel and Pause endpoints. Reads/writes are
// protected by activeJobsMu.
var (
	activeJobsMu sync.Mutex
	activeJobs   = make(map[uuid.UUID]chan struct{})
)

// errReaperOnly is used when a worker can't make progress but we don't
// want to bump the chunk's attempts counter (e.g., transient DB hiccup).
var errReaperOnly = errors.New("transient; let the reaper retry")

// RunImport is the orchestration entry point. It transitions the job
// through the staged pipeline:
//
//  1. validate (already done by upload+plan)
//  2. users     — resolve every Slack user id to a OneCamp user id
//  3. channels  — create channels (with collision resolution)
//  4. messages  — top-level pass: posts in channels (multi-worker)
//  5. threads   — second pass: thread replies as comments
//  6. files     — separate pool downloading + uploading attachments
//  7. reactions — final pass with the complete id_map
//  8. finalize  — mark complete, emit MQTT. The staged ZIP is kept on
//     MinIO until IMPORT_RAW_RETENTION_DAYS so the
//     operator can rollback + retry without re-upload;
//     the cleanup loop deletes it after retention.
//
// RunImport runs in its own goroutine; the caller (controller) returns
// the job id immediately. The status field is the contract with the FE.
func RunImport(parentCtx context.Context, jobId uuid.UUID, importingUser *userModels.UserInfo) {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(parentCtx,
				"SlackImport.RunImport recovered panic job=%s err=%v", jobId, r)
			_ = importModels.UpdateStatus(parentCtx, jobId, importModels.StatusFailed,
				strPtr("failed"), strPtr(fmt.Sprintf("panic: %v", r)))
		}
		removeActiveJob(jobId)
	}()

	// Dedicated context that survives the HTTP request. The cancellation
	// channel lets Cancel() interrupt any stage cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Propagate "this is a historical bulk import" so the post/comment
	// business layer skips the FCM/MQTT/AI fan-out for every imported
	// message. Without this flag, importing 100k messages would push
	// 100k notifications to channel members. See helpers.IsBulkImport.
	ctx = context.WithValue(ctx, helpers.BulkImportContextKey, true)

	cancelCh := make(chan struct{})
	registerActiveJob(jobId, cancelCh)

	go func() {
		select {
		case <-cancelCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return
	}
	if job.RawObjectKey == nil {
		_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr("missing raw_object_key"))
		return
	}

	// Pull the staged ZIP from MinIO. We previously copied it into /tmp
	// so archive/zip could random-access it, but that capped imports at
	// the host's free disk. minioReaderAt issues HTTP range requests
	// directly against MinIO, so this path scales to arbitrarily large
	// exports without disk staging. See business/SlackImport/minio_reader.go.
	//
	// Kill-switch: SLACK_IMPORT_STAGE_TO_DISK=1 falls back to the legacy
	// download-to-/tmp path. We keep this for environments where the S3
	// layer's range-request semantics misbehave (rare; some self-hosted
	// gateways).
	var (
		arc     *Archive
		cleanup func()
	)
	if os.Getenv("SLACK_IMPORT_STAGE_TO_DISK") == "1" {
		var zr *zip.Reader
		zr, cleanup, err = openStagedZipDiskFallback(ctx, *job.RawObjectKey)
		if zr != nil {
			arc = diskArchive(zr)
		}
	} else {
		arc, cleanup, err = openMinioZip(ctx, *job.RawObjectKey)
	}
	if err != nil {
		_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr(fmt.Sprintf("open staged zip: %v", err)))
		return
	}
	defer cleanup()

	parsed, err := ParseManifests(arc.Reader)
	if err != nil {
		_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr(fmt.Sprintf("parse manifests: %v", err)))
		return
	}

	// Decode per-job options.
	options := decodeOptions(job.Options)

	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("users"), nil)
	publishProgress(ctx, jobId, job.SlackWorkspaceName, "running", "users", "")

	// Stage 2: users
	if _, err := resolveUsers(ctx, jobId, job.SlackWorkspaceName, parsed.Users, importingUser.UserPostgresInfo.Id); err != nil {
		failJob(ctx, jobId, job.SlackWorkspaceName, fmt.Errorf("users: %w", err))
		return
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 3: channels (public + private). DMs/MPIMs come later.
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("channels"), nil)
	publishProgress(ctx, jobId, job.SlackWorkspaceName, "running", "channels", "")
	if _, err := resolveChannels(ctx, jobId, job.SlackWorkspaceName,
		parsed.Channels, parsed.Groups,
		importingUser, options.ChannelPrefix); err != nil {
		failJob(ctx, jobId, job.SlackWorkspaceName, fmt.Errorf("channels: %w", err))
		return
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 3b: DM/MPIM groupings. We don't write any chat rows here —
	// just hash and persist the OneCamp grouping_id for each Slack DM
	// id so the message worker can drop messages into the right grouping.
	// Source filename only; private/public channels live in stages 4-5.
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("dms"), nil)
	publishProgress(ctx, jobId, job.SlackWorkspaceName, "running", "dms", "")
	if _, err := resolveDMs(ctx, jobId, job.SlackWorkspaceName,
		parsed.DMs, parsed.MPIMs, importingUser); err != nil {
		// Non-fatal: a corrupt DM manifest shouldn't break the whole
		// import. Log and proceed; channel imports remain valuable.
		helpers.LogWarnWithContext(ctx,
			"SlackImport DM resolution had issues job=%s err=%+v", jobId, err)
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 4: messages (top-level)
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("messages"), nil)
	if err := runWorkerPool(ctx, jobId, job.SlackWorkspaceName, "messages", messageWorkers,
		[]string{importModels.ChunkChannelMessages},
		func(workerCtx context.Context, c *importModels.Chunk) error {
			return processMessageChunk(workerCtx, arc, c, isTrue(options.SkipSubtypes), importingUser, job.SlackWorkspaceName)
		}); err != nil {
		failJob(ctx, jobId, job.SlackWorkspaceName, err)
		return
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 5: threads (second pass; depends on message id_map being complete)
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("threads"), nil)
	if err := runWorkerPool(ctx, jobId, job.SlackWorkspaceName, "threads", messageWorkers,
		[]string{importModels.ChunkChannelThreads},
		func(workerCtx context.Context, c *importModels.Chunk) error {
			return processMessageChunk(workerCtx, arc, c, isTrue(options.SkipSubtypes), importingUser, job.SlackWorkspaceName)
		}); err != nil {
		failJob(ctx, jobId, job.SlackWorkspaceName, err)
		return
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 5b: DM / MPIM messages. Workers branch on the chunk's
	// channel_slack_id metadata (DM vs MPIM) and route to the right
	// chat-business call. Threads inside DMs are processed in the same
	// pass as top-level since DMs rarely use threading and the volume
	// doesn't justify a separate stage.
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("dm_messages"), nil)
	if err := runWorkerPool(ctx, jobId, job.SlackWorkspaceName, "dm_messages", messageWorkers,
		[]string{importModels.ChunkDMMessages},
		func(workerCtx context.Context, c *importModels.Chunk) error {
			return processDMChunk(workerCtx, arc, c, isTrue(options.SkipSubtypes), importingUser, job.SlackWorkspaceName)
		}); err != nil {
		// Non-fatal: DM failures should not abort the whole import.
		// The error log surfaces specific issues; channels remain useful.
		helpers.LogWarnWithContext(ctx,
			"SlackImport DM stage encountered errors job=%s err=%+v", jobId, err)
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 6: files. Files are scheduled by the message workers, so we
	// only enter this stage AFTER messages+threads have settled.
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("files"), nil)
	if err := runWorkerPool(ctx, jobId, job.SlackWorkspaceName, "files", fileWorkers,
		[]string{importModels.ChunkFile},
		func(workerCtx context.Context, c *importModels.Chunk) error {
			return processFileChunk(workerCtx, c, importingUser.UserPostgresInfo.Id, options.MaxFileBytes)
		}); err != nil {
		// Files are best-effort: a single failed file should not fail
		// the whole import. We log and proceed to reactions.
		helpers.LogWarnWithContext(ctx,
			"SlackImport file stage encountered errors job=%s err=%+v", jobId, err)
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 7: reactions
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusRunning, strPtr("reactions"), nil)
	if err := runWorkerPool(ctx, jobId, job.SlackWorkspaceName, "reactions", 1,
		[]string{importModels.ChunkReactionPass},
		func(workerCtx context.Context, c *importModels.Chunk) error {
			return processReactionPass(workerCtx, arc, c, parsed)
		}); err != nil {
		// Reactions are also best-effort.
		helpers.LogWarnWithContext(ctx,
			"SlackImport reactions stage errors job=%s err=%+v", jobId, err)
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// Stage 8: finalize
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusCompleted, strPtr("finalize"), nil)
	publishProgress(ctx, jobId, job.SlackWorkspaceName, "completed", "finalize", "")

	// Stage 9: the digest, strictly AFTER the customer has been told the import
	// finished. It is a bonus on top of a completed job, so it must never delay
	// the completed status, fail the job, or run before the panel says done.
	//
	// ctx rather than a fresh background context: an operator who cancelled, or a
	// server shutting down, has said they want this to stop, and a summary is the
	// first thing that should be given up.
	imported, _ := importModels.SumItemsImported(ctx, jobId)
	writeDigest(ctx, DigestRequest{
		JobID:            jobId,
		WorkspaceName:    job.SlackWorkspaceName,
		ChannelUUIDs:     importedChannelUUIDs(ctx, jobId),
		ImportingUser:    importingUser,
		MessagesImported: imported,
	})
	// Republish so a panel sitting on the completed event picks the digest up
	// without the customer reloading. Same status, now with prose behind it.
	publishProgress(ctx, jobId, job.SlackWorkspaceName, "completed", "finalize", "")
}

// cancelledOrCtxDone is the orchestrator's "should I keep going?" guard.
// Returns true if the operator cancelled (status went to cancelled),
// the parent context was cancelled, or the job row got nuked.
//
// CRITICAL: without this guard, a Cancel during stage N returns nil
// from runWorkerPool (workers exited cleanly via ctx.Done), the
// orchestrator marches to stage N+1, eventually reaches finalize, and
// overwrites status='cancelled' with status='completed'. Customer-
// reported "I clicked Cancel and it ran to completion anyway" bug.
func cancelledOrCtxDone(ctx context.Context, jobId uuid.UUID) bool {
	if ctx.Err() != nil {
		return true
	}
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil || job == nil {
		return true
	}
	switch job.Status {
	case importModels.StatusCancelled, importModels.StatusFailed,
		importModels.StatusRolledBack:
		return true
	}
	return false
}

// runWorkerPool spins up N goroutines that each pull chunks of the
// requested types from PG and process them via fn. Returns when there's
// no more pending work for those types.
//
// Throttled progress: a separate goroutine emits MQTT updates at most
// once per second. Workers update items_done on the chunk via
// HeartbeatChunk; the publisher reads the aggregate counts from PG.
func runWorkerPool(ctx context.Context, jobId uuid.UUID, workspaceName, stage string, n int,
	chunkTypes []string, fn func(context.Context, *importModels.Chunk) error) error {

	publishCtx, stopPublish := context.WithCancel(ctx)
	defer stopPublish()

	var processed int64
	go progressPublisher(publishCtx, jobId, workspaceName, stage, &processed)

	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		workerId := fmt.Sprintf("%s-w%d", jobId, i)
		go func(wid string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					helpers.LogErrorWithContext(ctx,
						"SlackImport worker panic %s: %v", wid, r)
					errCh <- fmt.Errorf("worker panic: %v", r)
				}
			}()

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				chunk, err := importModels.ClaimNextChunk(ctx, jobId, chunkTypes, wid)
				if err != nil {
					errCh <- err
					return
				}
				if chunk == nil {
					// No work available right now. Sleep a beat and
					// re-check; another worker might still be working
					// on the chunk that produces work for us. After
					// 5s of consecutive empties we exit.
					if !waitForMoreWork(ctx, jobId, chunkTypes) {
						return
					}
					continue
				}

				if err := fn(ctx, chunk); err != nil {
					if errors.Is(err, errReaperOnly) {
						// Transient condition (rate limit, etc.). The
						// chunk has already been reset to pending by
						// the worker without bumping attempts. Just
						// loop back and grab the next available chunk.
						continue
					}
					reason := err.Error()
					if len(reason) > 1024 {
						reason = reason[:1024]
					}
					_ = importModels.FailChunk(ctx, chunk.Id, chunk.ItemsDone, chunk.LastCursor, reason)
					// We don't return here; one bad chunk should not
					// halt the worker. The plan stage caps attempts,
					// so a permanently broken chunk eventually gives up.
					continue
				}
				atomic.AddInt64(&processed, 1)
			}
		}(workerId)
	}

	wg.Wait()
	close(errCh)

	// Surface the first error if any.
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// waitForMoreWork returns true if the operator should keep looking,
// false if the stage is genuinely drained. We poll PG twice with a
// short sleep so the inevitable race between "I just finished my
// chunk" and "another worker just enqueued a follow-up" doesn't cause
// the pool to exit prematurely.
func waitForMoreWork(ctx context.Context, jobId uuid.UUID, chunkTypes []string) bool {
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
		has, err := importModels.HasPendingWork(ctx, jobId, chunkTypes)
		if err == nil && has {
			return true
		}
	}
	return false
}

// progressPublisher emits MQTT progress events at most once per second.
func progressPublisher(ctx context.Context, jobId uuid.UUID, workspaceName, stage string, processed *int64) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publishProgress(ctx, jobId, workspaceName, "running", stage, "")
		}
	}
}

// publishProgress reads aggregate counts and emits a single MQTT message.
func publishProgress(ctx context.Context, jobId uuid.UUID, workspaceName, status, stage, errMsg string) {
	total, done, failed, _ := importModels.CountChunks(ctx, jobId)
	items, _ := importModels.SumItemsImported(ctx, jobId)
	errCount, _ := importModels.CountErrors(ctx, jobId)

	mqttBusiness.PublishSlackImportProgress(&mqttStruct.MqttSlackImportProgress{
		JobId:              jobId.String(),
		SlackWorkspaceName: workspaceName,
		Status:             status,
		Stage:              stage,
		ChunksTotal:        total,
		ChunksDone:         done,
		ChunksFailed:       failed,
		ItemsImported:      items,
		ErrorsTotal:        errCount,
		ErrorMessage:       errMsg,
	})
}

// failJob is the canonical "this run is over and broken" exit.
func failJob(ctx context.Context, jobId uuid.UUID, workspaceName string, err error) {
	msg := err.Error()
	helpers.LogErrorWithContext(ctx,
		"SlackImport job %s failed: %s", jobId, msg)
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusFailed,
		strPtr("failed"), &msg)
	publishProgress(ctx, jobId, workspaceName, "failed", "failed", msg)
}

// CancelImport signals a running import to stop. Workers see ctx.Done()
// at their next loop iteration and exit; the orchestrator finalises the
// job as cancelled.
func CancelImport(ctx context.Context, jobId uuid.UUID) error {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return err
	}
	if job.Status != importModels.StatusRunning && job.Status != importModels.StatusPaused {
		return fmt.Errorf("job is not running (status=%s)", job.Status)
	}

	if err := importModels.UpdateStatus(ctx, jobId, importModels.StatusCancelled,
		strPtr("cancelled"), strPtr("cancelled by operator")); err != nil {
		return err
	}
	if err := importModels.CancelAllPendingChunks(ctx, jobId); err != nil {
		return err
	}

	// Tell any in-process orchestrator to stop.
	activeJobsMu.Lock()
	if ch, ok := activeJobs[jobId]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
		delete(activeJobs, jobId)
	}
	activeJobsMu.Unlock()

	publishProgress(ctx, jobId, job.SlackWorkspaceName, "cancelled", "cancelled", "")
	return nil
}

// --- Helpers --------------------------------------------------------------

// openStagedZip is replaced by openMinioZip in minio_reader.go. The new
// implementation issues HTTP range requests against MinIO instead of
// downloading the full archive to local disk, which means imports are
// no longer capped by /tmp free space. The function below is kept as
// a fallback for environments where MinIO range support is broken
// (very rare; some self-hosted S3 gateways).
//
// Operators can opt into the legacy path by setting
// SLACK_IMPORT_STAGE_TO_DISK=1 in the env. Keeping it gives us a
// kill-switch if the range-request path misbehaves on a customer's
// S3 implementation.
func openStagedZipDiskFallback(ctx context.Context, objectKey string) (*zip.Reader, func(), error) {
	bucket := helpers.UserUploadBucket()

	obj, err := minioInit.MinioClient.GetObject(ctx, bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}

	// Use a temp file rather than RAM so 50 GB imports don't OOM.
	tmp, err := os.CreateTemp("", "slack-import-*.zip")
	if err != nil {
		_ = obj.Close()
		return nil, nil, err
	}
	if _, err := io.Copy(tmp, obj); err != nil {
		_ = obj.Close()
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, nil, err
	}
	_ = obj.Close()

	stat, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, nil, err
	}

	zr, err := zip.NewReader(tmp, stat.Size())
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, nil, err
	}

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	return zr, cleanup, nil
}

func decodeOptions(raw json.RawMessage) struct {
	SkipSubtypes  *bool
	ChannelPrefix string
	MaxFileBytes  int64
} {
	out := struct {
		SkipSubtypes  *bool
		ChannelPrefix string
		MaxFileBytes  int64
	}{}
	if len(raw) == 0 {
		return out
	}
	var holder struct {
		SkipSubtypes  *bool  `json:"skip_subtypes,omitempty"`
		ChannelPrefix string `json:"channel_prefix,omitempty"`
		MaxFileBytes  int64  `json:"max_file_bytes,omitempty"`
	}
	if err := json.Unmarshal(raw, &holder); err != nil {
		return out
	}
	out.SkipSubtypes = holder.SkipSubtypes
	out.ChannelPrefix = holder.ChannelPrefix
	out.MaxFileBytes = holder.MaxFileBytes
	return out
}

func registerActiveJob(jobId uuid.UUID, ch chan struct{}) {
	activeJobsMu.Lock()
	defer activeJobsMu.Unlock()
	activeJobs[jobId] = ch
}

func removeActiveJob(jobId uuid.UUID) {
	activeJobsMu.Lock()
	defer activeJobsMu.Unlock()
	delete(activeJobs, jobId)
}

func envIntDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

func strPtr(s string) *string { return &s }

func isTrue(b *bool) bool {
	if b == nil {
		// Default true for skip_subtypes; we don't import "X joined" noise
		// unless the operator explicitly turns the option off.
		return true
	}
	return *b
}

// StartReaperLoop runs every 60s and resets stuck claims. Mirrors the
// GitHub-sync reaper. Wire from main.go.
func StartReaperLoop() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			ctx := context.Background()
			n, err := importModels.ReapStuckChunks(ctx, 5*time.Minute)
			if err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport reaper failed: %+v", err)
				continue
			}
			if n > 0 {
				helpers.LogInfoWithContext(ctx,
					"SlackImport reaper reset %d stuck chunk(s)", n)
			}
		}
	}()
}

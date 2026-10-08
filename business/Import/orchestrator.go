// Package business is the provider-agnostic import pipeline.
//
// Lifecycle:
//
//	upload (file path) | presign+finalize | oauth-connect+discover  → row in import_jobs
//	plan(jobId)                                                     → counts + chunks + status mapping
//	run(jobId)                                                       → orchestrator drives stages
//	cancel(jobId) | rollback(jobId) | delete-staged-zip(jobId)
//
// Stages run in fixed order per provider capability:
//
//	users → teams → projects → tasks → subtasks → task_comments → attachments → finalize
//
// (For the legacy Slack provider the orchestrator delegates to the
// existing SlackImport orchestrator so this package stays free of Slack
// semantics.)
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	slackImportBusiness "github.com/akashc777/OneCamp/business/SlackImport"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Worker pool sizes. Env-tunable per stage so operators can throttle
// task imports independently of attachment downloads.
var (
	taskWorkers       = envIntDefault("IMPORT_TASK_WORKERS", 4)
	commentWorkers    = envIntDefault("IMPORT_COMMENT_WORKERS", 4)
	attachmentWorkers = envIntDefault("IMPORT_ATTACHMENT_WORKERS", 2)
)

// activeJobs tracks the cancellation channel for each running import.
var (
	activeJobsMu sync.Mutex
	activeJobs   = make(map[uuid.UUID]chan struct{})
)

// errReaperOnly is used when a worker can't make progress but doesn't
// want to bump the chunk's attempts counter.
var errReaperOnly = errors.New("transient; let the reaper retry")

// RunImport is the orchestration entry point. The controller starts it
// in a goroutine; this function survives the request context.
//
// For provider="slack" we delegate to the legacy SlackImport orchestrator
// so the existing message/channel/DM workers keep running. New providers
// (asana/jira/trello/notion/todoist) take the generic path below.
func RunImport(parentCtx context.Context, jobId uuid.UUID, importingUser *userModels.UserInfo) {
	// Defence-in-depth: a nil importingUser would cause every
	// downstream Dgraph/PG write to panic on UserDgraphInfo.Uid. The
	// controller's requireAdmin middleware guarantees non-nil, but a
	// future refactor could break that — fail loud rather than crash.
	if importingUser == nil {
		_ = importModels.UpdateStatus(parentCtx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr("internal: importingUser is nil"))
		return
	}
	if importingUser.UserPostgresInfo.Id == uuid.Nil {
		_ = importModels.UpdateStatus(parentCtx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr("internal: importingUser has zero uuid"))
		return
	}

	job, err := importModels.GetJob(parentCtx, jobId)
	if err != nil || job == nil {
		helpers.LogErrorWithContext(parentCtx,
			"Import.RunImport: job not found id=%s err=%v", jobId, err)
		return
	}

	if job.Provider == importModels.ProviderSlack {
		// Delegate to the existing Slack orchestrator. The Slack stages
		// (channels, messages, DMs, reactions, files) live there and
		// don't fit the generic task-shaped pipeline below.
		slackImportBusiness.RunImport(parentCtx, jobId, importingUser)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(parentCtx,
				"Import.RunImport recovered panic provider=%s job=%s err=%v",
				job.Provider, jobId, r)
			_ = importModels.UpdateStatus(parentCtx, jobId, importModels.StatusFailed,
				strPtr("failed"), strPtr(fmt.Sprintf("panic: %v", r)))
		}
		removeActiveJob(jobId)
	}()

	prov := importProvider.Get(job.Provider)
	if prov == nil {
		_ = importModels.UpdateStatus(parentCtx, jobId, importModels.StatusFailed,
			strPtr("failed"), strPtr("unknown provider: "+job.Provider))
		return
	}

	// Detached context surviving the HTTP request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bulk-import flag suppresses MQTT/AI/webhook fan-out for every
	// downstream business call. Without this the import would publish
	// thousands of "task_created" / "comment_created" events, blast
	// FCM, and queue thousands of webhook deliveries.
	ctx = context.WithValue(ctx, helpers.BulkImportContextKey, true)

	// Cancellation registration so CancelImport can interrupt mid-stage.
	cancelCh := make(chan struct{})
	registerActiveJob(jobId, cancelCh)
	go func() {
		select {
		case <-cancelCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	opts := decodeOptions(job.Options)

	// ── Stage 1: users ──────────────────────────────────────────────
	if err := setStage(ctx, jobId, "users"); err != nil {
		return
	}
	if err := runUsersStage(ctx, prov, job, opts); err != nil {
		failJob(ctx, job, fmt.Errorf("users: %w", err))
		return
	}
	if cancelledOrCtxDone(ctx, jobId) {
		return
	}

	// ── Stage 2: teams ──────────────────────────────────────────────
	if prov.Capabilities()&importProvider.CapTeams != 0 {
		if err := setStage(ctx, jobId, "teams"); err != nil {
			return
		}
		if err := runTeamsStage(ctx, prov, job, opts, importingUser); err != nil {
			failJob(ctx, job, fmt.Errorf("teams: %w", err))
			return
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 3: projects ───────────────────────────────────────────
	if prov.Capabilities()&importProvider.CapProjects != 0 {
		if err := setStage(ctx, jobId, "projects"); err != nil {
			return
		}
		if err := runProjectsStage(ctx, prov, job, opts, importingUser); err != nil {
			failJob(ctx, job, fmt.Errorf("projects: %w", err))
			return
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 4: tasks (per-project chunks) ─────────────────────────
	if prov.Capabilities()&importProvider.CapTasks != 0 {
		if err := setStage(ctx, jobId, "tasks"); err != nil {
			return
		}
		if err := runWorkerPool(ctx, jobId, "tasks", taskWorkers,
			[]string{importModels.ChunkProjectTasks},
			func(workerCtx context.Context, c *importModels.Chunk) error {
				return processTaskChunk(workerCtx, prov, job, opts, c, importingUser)
			}); err != nil {
			failJob(ctx, job, fmt.Errorf("tasks: %w", err))
			return
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 5: subtasks (depends on tasks id_map) ─────────────────
	if prov.Capabilities()&importProvider.CapSubtasks != 0 {
		if err := setStage(ctx, jobId, "subtasks"); err != nil {
			return
		}
		if err := runWorkerPool(ctx, jobId, "subtasks", taskWorkers,
			[]string{importModels.ChunkTaskSubtasks},
			func(workerCtx context.Context, c *importModels.Chunk) error {
				return processSubtaskChunk(workerCtx, prov, job, opts, c, importingUser)
			}); err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import subtasks stage errors job=%s err=%+v", jobId, err)
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 6: task comments ──────────────────────────────────────
	if prov.Capabilities()&importProvider.CapTaskComments != 0 {
		if err := setStage(ctx, jobId, "task_comments"); err != nil {
			return
		}
		if err := runWorkerPool(ctx, jobId, "task_comments", commentWorkers,
			[]string{importModels.ChunkTaskComments},
			func(workerCtx context.Context, c *importModels.Chunk) error {
				return processTaskCommentChunk(workerCtx, prov, job, opts, c, importingUser)
			}); err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import task_comments stage errors job=%s err=%+v", jobId, err)
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 7: attachments ────────────────────────────────────────
	if prov.Capabilities()&importProvider.CapAttachments != 0 {
		if err := setStage(ctx, jobId, "attachments"); err != nil {
			return
		}
		if err := runWorkerPool(ctx, jobId, "attachments", attachmentWorkers,
			[]string{importModels.ChunkAttachment},
			func(workerCtx context.Context, c *importModels.Chunk) error {
				return processAttachmentChunk(workerCtx, prov, job, opts, c, importingUser)
			}); err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import attachments stage errors job=%s err=%+v", jobId, err)
		}
		if cancelledOrCtxDone(ctx, jobId) {
			return
		}
	}

	// ── Stage 8: finalize ───────────────────────────────────────────
	_ = importModels.UpdateStatus(ctx, jobId, importModels.StatusCompleted,
		strPtr("finalize"), nil)
	publishProgress(ctx, job, "completed", "finalize", "")

	// Per-provider cache eviction. Providers with per-job state
	// (Trello board snapshot, Notion database list, etc.) clean up
	// here so memory doesn't grow unbounded across long-running deploys.
	if cleaner, ok := prov.(importProvider.JobCleaner); ok {
		cleaner.CleanupJob(jobId.String())
	}
	forgetFieldWarnings(jobId)
}

// CancelImport signals a running import to stop. Generic across providers.
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
	publishProgress(ctx, job, "cancelled", "cancelled", "")
	if prov := importProvider.Get(job.Provider); prov != nil {
		if cleaner, ok := prov.(importProvider.JobCleaner); ok {
			cleaner.CleanupJob(job.Id.String())
		}
	}
	forgetFieldWarnings(job.Id)
	return nil
}

// StartReaperLoop runs every 60s and resets stuck claims.
func StartReaperLoop() {
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			ctx := context.Background()
			n, err := importModels.ReapStuckChunks(ctx, 5*time.Minute)
			if err != nil {
				helpers.LogWarnWithContext(ctx, "Import reaper failed: %+v", err)
				continue
			}
			if n > 0 {
				helpers.LogInfoWithContext(ctx,
					"Import reaper reset %d stuck chunk(s)", n)
			}
		}
	}()
}

// runWorkerPool spins up N goroutines that each pull chunks of the
// requested types from PG and process them via fn. Returns when there's
// no more pending work for those types.
func runWorkerPool(ctx context.Context, jobId uuid.UUID, stage string, n int,
	chunkTypes []string, fn func(context.Context, *importModels.Chunk) error) error {

	publishCtx, stopPublish := context.WithCancel(ctx)
	defer stopPublish()

	var processed int64
	job, _ := importModels.GetJob(ctx, jobId)
	if job == nil {
		return errors.New("job vanished mid-stage")
	}
	go progressPublisher(publishCtx, job, stage, &processed)

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
						"Import worker panic %s: %v", wid, r)
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
					if !waitForMoreWork(ctx, jobId, chunkTypes) {
						return
					}
					continue
				}
				if err := fn(ctx, chunk); err != nil {
					if errors.Is(err, errReaperOnly) {
						continue
					}
					reason := err.Error()
					if len(reason) > 1024 {
						reason = reason[:1024]
					}
					_ = importModels.FailChunk(ctx, chunk.Id, chunk.ItemsDone, chunk.LastCursor, reason)
					continue
				}
				atomic.AddInt64(&processed, 1)
			}
		}(workerId)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func waitForMoreWork(ctx context.Context, jobId uuid.UUID, chunkTypes []string) bool {
	for i := 0; i < 5; i++ {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
		has, err := importDomain.HasPendingWork(ctx, jobId, chunkTypes)
		if err == nil && has {
			return true
		}
	}
	return false
}

// progressPublisher emits an MQTT progress event every second a stage
// is making progress, plus one final tick when the stage drains. We
// snapshot `processed` at each tick and skip the publish when nothing
// has changed since the previous tick — long-idle waits between
// chunks (the worker pool sleeping in waitForMoreWork) don't spam the
// broker with identical "running" payloads.
func progressPublisher(ctx context.Context, job *importModels.Job, stage string, processed *int64) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	var lastSeen int64 = -1
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := atomic.LoadInt64(processed)
			if cur == lastSeen {
				continue
			}
			lastSeen = cur
			publishProgress(ctx, job, "running", stage, "")
		}
	}
}

func publishProgress(ctx context.Context, job *importModels.Job, status, stage, errMsg string) {
	total, done, failed, _ := importModels.CountChunks(ctx, job.Id)
	items, _ := importModels.SumItemsImported(ctx, job.Id)
	errCount, _ := importModels.CountErrors(ctx, job.Id)

	mqttBusiness.PublishSlackImportProgress(&mqttStruct.MqttSlackImportProgress{
		JobId:              job.Id.String(),
		SlackWorkspaceName: job.SourceWorkspaceName, // shared MQTT struct; field is reused
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

// failJob is the canonical "this run is over and broken" exit. Also
// triggers per-provider cache eviction so a failed job doesn't leak
// per-job state.
func failJob(ctx context.Context, job *importModels.Job, err error) {
	msg := err.Error()
	helpers.LogErrorWithContext(ctx,
		"Import job %s failed: %s", job.Id, msg)
	_ = importModels.UpdateStatus(ctx, job.Id, importModels.StatusFailed,
		strPtr("failed"), &msg)
	publishProgress(ctx, job, "failed", "failed", msg)
	if prov := importProvider.Get(job.Provider); prov != nil {
		if cleaner, ok := prov.(importProvider.JobCleaner); ok {
			cleaner.CleanupJob(job.Id.String())
		}
	}
	forgetFieldWarnings(job.Id)
}

// setStage flips a job's stage label without forcing the status back to
// 'running'. Earlier versions used UpdateStatus(jobId, 'running', stage)
// which raced with CancelImport: a concurrent operator-initiated cancel
// could land between two stages, and the next setStage would overwrite
// status='cancelled' with status='running'. We now only update stage,
// and refuse to advance if the job has already left the running state.
func setStage(ctx context.Context, jobId uuid.UUID, stage string) error {
	res, err := importModels.SetStageIfRunning(ctx, jobId, stage)
	if err != nil {
		return err
	}
	if !res {
		return errJobNotRunning
	}
	return nil
}

// errJobNotRunning is returned by setStage when the job is no longer in
// a running state (cancelled / failed / rolled_back). Callers treat it
// as a soft exit, not an error to log.
var errJobNotRunning = errors.New("job no longer running")

// cancelledOrCtxDone is the inter-stage guard. Without this a Cancel
// during stage N could finish stage N's workers cleanly via ctx.Done,
// then the orchestrator marches to stage N+1 and eventually overwrites
// status='cancelled' with status='completed'.
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

// ─── Helpers ─────────────────────────────────────────────────────────

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

func decodeOptions(raw json.RawMessage) importProvider.JobOptions {
	out := importProvider.JobOptions{}
	if len(raw) == 0 {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func mustMarshal(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func strPtr(s string) *string { return &s }

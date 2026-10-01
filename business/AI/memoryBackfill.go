package business

// Admin "rebuild memory" backfill.
//
// WHY
// ---
// When an admin enables the memory layer on an existing workspace, all the
// durable knowledge already sitting in months of channels/DMs/projects is
// invisible to it — the live worker only extracts content created AFTER a
// scope's watermark. This backfill closes that gap: a one-shot, admin-
// triggered sweep that walks the FULL history of every active scope through
// the same chunked extractor the live worker uses, then advances each
// scope's watermark to "now" so the live worker seamlessly takes over
// without reprocessing.
//
// PRODUCTION PROPERTIES
//   - Single-flight: a global Redis lock means only one backfill runs at a
//     time (it is heavy). A second request returns "already running".
//   - Self-gating: no-op unless AI + memory layer are enabled.
//   - Bounded & cooperative: per-scope history is paginated in capped
//     pages; the whole job respects context cancellation (server shutdown)
//     and a hard wall-clock budget so it can never run unbounded.
//   - Idempotent: extraction dedups by (kind+content+scope) hash, so
//     running backfill twice doesn't duplicate items. Safe to re-run.
//   - Observable: progress + final result are published to Redis as JSON
//     for the admin panel to poll.
//   - Reuses the live worker's generic helpers (DiscoverActiveScopes,
//     FetchContentSince, extractWindowChunked, memoryScopeFor, watermark
//     writers) — no parallel extraction path to drift out of sync.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// How far back a backfill reaches. Generous (covers most workspaces'
	// useful history) but bounded so a multi-year archive can't make one
	// job run forever. Tunable via AI_MEMORY_BACKFILL_DAYS.
	backfillDefaultLookbackDays = 180

	// Per-scope history pagination page size.
	backfillPageSize = 400

	// Safety cap on pages per scope so a pathological scope can't dominate
	// the whole job (≈ 400 * 50 = 20k items/scope max).
	backfillMaxPagesPerScope = 50

	// Hard wall-clock budget for one backfill job. Generous for local
	// LLMs but guarantees termination and lock release.
	backfillMaxDuration = 90 * time.Minute

	// Scope cardinality cap per dimension for discovery.
	backfillMaxScopes = 5000

	// Heartbeat cadence + staleness threshold for liveness detection. The
	// worker refreshes the status heartbeat every backfillHeartbeatInterval
	// from a dedicated goroutine (independent of per-scope progress, so a
	// slow LLM call doesn't look dead). A "running" status whose heartbeat is
	// older than backfillStaleAfter is treated as a crashed/restarted worker
	// and reported as failed — preventing the UI from sticking on "Rebuilding…"
	// forever after a server restart.
	backfillHeartbeatInterval = 30 * time.Second
	backfillStaleAfter        = 2 * time.Minute
)

// MemoryBackfillStatus is the pollable progress/result document.
type MemoryBackfillStatus struct {
	State          string `json:"state"` // "running" | "completed" | "failed"
	StartedAt      int64  `json:"started_at"`
	FinishedAt     int64  `json:"finished_at,omitempty"`
	ScopesTotal    int    `json:"scopes_total"`
	ScopesDone     int    `json:"scopes_done"`
	ItemsExtracted int    `json:"items_extracted"`
	Error          string `json:"error,omitempty"`
	// HeartbeatAt is refreshed periodically by the live worker. A "running"
	// status whose heartbeat is stale means the worker died (e.g. server
	// restart) — the reader downgrades it to "failed" so the UI recovers.
	HeartbeatAt int64 `json:"heartbeat_at,omitempty"`
}

// RunMemoryBackfillAsync starts a backfill in the background and returns
// immediately. Returns (false, reason) when it can't start (feature off, or
// a job already running) so the controller can surface the right HTTP code.
func RunMemoryBackfillAsync(ctx context.Context) (started bool, reason string) {
	// Gate on settings first so a disabled workspace gives a clear message
	// rather than silently acquiring a lock.
	settings, err := getAISettingsForMemory(ctx)
	if err != nil {
		return false, "failed to load AI settings"
	}
	if !settings.enabled || !settings.memoryEnabled {
		return false, "memory layer is not enabled"
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return false, "AI service is not enabled"
	}

	// Single-flight global lock.
	if !acquireBackfillLock(ctx) {
		return false, "a memory backfill is already running"
	}

	go runMemoryBackfill()
	return true, ""
}

// runMemoryBackfill is the worker body. It owns the backfill lock for its
// lifetime via the lock TTL; we publish status throughout. Uses its own
// background context (decoupled from the request) with a hard timeout.
func runMemoryBackfill() {
	ctx, cancel := context.WithTimeout(context.Background(), backfillMaxDuration)
	defer cancel()

	// Single source of truth for this job's status, guarded so the progress
	// loop and the heartbeat ticker can't race / regress each other.
	var (
		mu     sync.Mutex
		status = MemoryBackfillStatus{State: "running", StartedAt: time.Now().Unix()}
	)
	publish := func() {
		mu.Lock()
		snapshot := status
		mu.Unlock()
		publishBackfillStatus(ctx, snapshot)
	}

	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("panic in memory backfill: %v", r)
			mu.Lock()
			status.State = "failed"
			status.Error = fmt.Sprintf("panic: %v", r)
			status.FinishedAt = time.Now().Unix()
			mu.Unlock()
			publish()
		}
		releaseBackfillLock(ctx)
	}()

	publish()

	// Liveness heartbeat: re-publish the current status on a fixed cadence so
	// its HeartbeatAt stays fresh even during a long LLM call on a big scope.
	// Goes through the same mutex+snapshot, so it never regresses loop
	// progress. Stops when the job ends or the context is cancelled.
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(backfillHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				publish()
			}
		}
	}()
	defer close(hbStop)

	lookback := time.Duration(backfillLookbackDays()) * 24 * time.Hour
	since := time.Now().Add(-lookback).Unix()

	scopes, err := ai.DiscoverActiveScopes(ctx, since, backfillMaxScopes)
	if err != nil {
		mu.Lock()
		status.State = "failed"
		status.Error = "discover scopes failed: " + err.Error()
		status.FinishedAt = time.Now().Unix()
		mu.Unlock()
		publish()
		helpers.LogErrorWithContext(ctx, "memory backfill: discover scopes failed: %v", err)
		return
	}

	// Largest scopes first so the most knowledge-dense history is captured
	// even if the wall-clock budget is hit.
	sort.SliceStable(scopes, func(i, j int) bool {
		return scopes[i].DocCount > scopes[j].DocCount
	})

	mu.Lock()
	status.ScopesTotal = len(scopes)
	mu.Unlock()
	publish()

	for _, sc := range scopes {
		if ctx.Err() != nil {
			break // budget exhausted or shutdown — stop cleanly
		}
		n, err := backfillScope(ctx, sc, since)
		if err != nil {
			// Log and continue; one bad scope must not abort the whole job.
			helpers.LogErrorWithContext(ctx, "memory backfill: scope %s:%s failed: %v", sc.FilterType, sc.FilterValue, err)
		}
		mu.Lock()
		status.ScopesDone++
		status.ItemsExtracted += n
		mu.Unlock()
		publish()
	}

	mu.Lock()
	status.State = "completed"
	if ctx.Err() != nil {
		status.Error = "stopped early (time budget or shutdown); re-run to continue"
	}
	status.FinishedAt = time.Now().Unix()
	scopesDone, scopesTotal, items := status.ScopesDone, status.ScopesTotal, status.ItemsExtracted
	mu.Unlock()
	publish()
	helpers.LogInfoWithContext(ctx, "memory backfill done: scopes=%d/%d items=%d",
		scopesDone, scopesTotal, items)
}

// backfillScope walks one scope's full history (from `since`) in pages,
// running the shared chunked extractor on each page, then advances the
// scope's watermark to the newest item processed so the live worker takes
// over without reprocessing. Returns total items persisted for the scope.
func backfillScope(ctx context.Context, sc ai.ActiveScope, since int64) (int, error) {
	scopeKey := sc.FilterType + ":" + sc.FilterValue
	scope := memoryScopeFor(sc.FilterType, sc.FilterValue, scopeKey)

	// Start from max(existing watermark, since) so we never re-extract a
	// window the live worker already covered, and never go further back
	// than the configured lookback.
	cursor := since
	if wm := readMemoryWatermark(ctx, scopeKey); wm > cursor {
		cursor = wm
	}

	total := 0
	newestOverall := cursor
	for page := 0; page < backfillMaxPagesPerScope; page++ {
		if ctx.Err() != nil {
			break
		}
		items, err := ai.FetchContentSince(ctx, sc.FilterType, sc.FilterValue, cursor, backfillPageSize)
		if err != nil {
			return total, err
		}
		if len(items) == 0 {
			break // history exhausted for this scope
		}
		newest := items[len(items)-1].CreatedDate

		n, _, exErr := extractWindowChunked(ctx, items, scope)
		total += n
		if exErr != nil {
			// Don't advance the watermark past content we failed to
			// process; surface the error so the job logs it and moves on.
			return total, exErr
		}

		if newest > newestOverall {
			newestOverall = newest
		}
		// Advance watermark incrementally so a mid-scope failure/shutdown
		// still records progress and a re-run resumes from here.
		writeMemoryWatermark(ctx, scopeKey, newest)

		if len(items) < backfillPageSize {
			break // last page
		}
		// Advance the cursor strictly past this page (FetchContentSince is
		// gt:cursor; using newest avoids missing same-second items because
		// the page is full and ordered ascending).
		cursor = newest
	}
	return total, nil
}

// --- status + lock helpers ---

// GetMemoryBackfillStatus returns the last published status, or a zero-value
// "idle" status when none exists.
//
// Liveness: a "running" status whose heartbeat is older than backfillStaleAfter
// means the worker died (server restart, crash, OOM) without writing a terminal
// status. We report it as "failed" so the UI recovers instead of showing
// "Rebuilding…" forever, and best-effort release the stale lock so a re-run
// works immediately rather than waiting out the lock TTL.
func GetMemoryBackfillStatus(ctx context.Context) MemoryBackfillStatus {
	val, ok, err := redisStore.GetString(ctx, registry.AIMemoryBackfillStatus, nil)
	if err != nil || !ok || val == "" {
		return MemoryBackfillStatus{State: "idle"}
	}
	var st MemoryBackfillStatus
	if json.Unmarshal([]byte(val), &st) != nil {
		return MemoryBackfillStatus{State: "idle"}
	}

	if st.State == "running" && isBackfillStale(st) {
		st.State = "failed"
		if st.Error == "" {
			st.Error = "the rebuild stopped unexpectedly (server restart or crash); click Rebuild to retry"
		}
		st.FinishedAt = time.Now().Unix()
		// Persist the downgrade so the next poll is consistent, and free the
		// lock the dead worker never released.
		publishBackfillStatus(ctx, st)
		releaseBackfillLock(ctx)
	}
	return st
}

// isBackfillStale reports whether a running status's heartbeat is too old to
// belong to a live worker. Falls back to StartedAt for statuses written before
// the first heartbeat tick (so a worker that died in its first 30s is still
// detected once enough time passes).
func isBackfillStale(st MemoryBackfillStatus) bool {
	last := st.HeartbeatAt
	if last == 0 {
		last = st.StartedAt
	}
	if last == 0 {
		return false // no timing info; don't second-guess
	}
	return time.Since(time.Unix(last, 0)) > backfillStaleAfter
}

func publishBackfillStatus(ctx context.Context, st MemoryBackfillStatus) {
	// Stamp a fresh heartbeat for in-progress statuses so the reader can tell
	// a live worker from a crashed one. Terminal states don't need it.
	if st.State == "running" {
		st.HeartbeatAt = time.Now().Unix()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := redisStore.SetString(ctx, registry.AIMemoryBackfillStatus, nil, string(data)); err != nil {
		helpers.LogErrorWithContext(ctx, "memory backfill: publish status failed: %v", err)
	}
}

func acquireBackfillLock(ctx context.Context) bool {
	res := redisStore.AllowFixedWindow(ctx, registry.AIMemoryBackfillLock, nil, 1)
	return res.Allowed
}

func releaseBackfillLock(ctx context.Context) {
	if err := redisStore.Delete(ctx, registry.AIMemoryBackfillLock, nil); err != nil {
		helpers.LogErrorWithContext(ctx, "memory backfill: release lock failed: %v", err)
	}
}

// backfillLookbackDays reads AI_MEMORY_BACKFILL_DAYS (days), defaulting to
// backfillDefaultLookbackDays, floored at 1 and capped at 1095 (3y).
func backfillLookbackDays() int {
	if v := envInt("AI_MEMORY_BACKFILL_DAYS"); v > 0 {
		if v > 1095 {
			return 1095
		}
		return v
	}
	return backfillDefaultLookbackDays
}

package business

// Batched workspace-memory extraction worker.
//
// WHY THIS EXISTS
// ---------------
// The meeting-recap agent compounds knowledge from CALLS, but the bulk of
// a workspace's durable decisions/commitments/open-questions live in
// streaming text: channel posts, DMs/group chats, project task threads.
// We deliberately do NOT extract per message — that would fire an LLM call
// on every keystroke-sized event (catastrophic cost/latency on a local
// Ollama, and most single messages contain nothing durable). Instead this
// worker periodically sweeps the scopes that actually changed and extracts
// from a BATCHED window, so a busy channel yields one extraction pass over
// many messages rather than hundreds of tiny ones.
//
// HOW IT STAYS CHEAP AND CORRECT
//   - Discovery is a single OpenSearch terms aggregation that returns only
//     scopes (channels/groups/projects) with NEW content since a cutoff —
//     so a quiet workspace costs one query and zero LLM calls.
//   - Each scope carries a Redis WATERMARK (last-processed timestamp); a
//     pass only reads content created after it, so work is incremental and
//     never re-extracts the same window.
//   - A per-scope Redis LOCK makes a pass idempotent across overlapping
//     ticks and multiple instances.
//   - Extraction reuses MaybeExtractMemory: same opt-in gate, same circuit
//     breaker, same strict-JSON contract, same dedup-by-hash upsert, same
//     permission-scoped persistence. Items are scoped to the
//     channel/group/project they came from, so retrieval (structured List
//     + intent-gated injection) stays permission-correct for every member.
//   - Soft-deleted and memory-type docs are excluded from the source
//     window, so extraction never feeds on deleted content or its own
//     output.
//
// It is fully gated on ai_settings (AI enabled + memory_layer_enabled); a
// customer who never turns the layer on pays nothing.

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	// Initial stagger after boot so the worker doesn't contend with the
	// auto-archiver (5m) and warm caches. Memory extraction is never
	// time-critical.
	memoryWorkerInitialDelay = 10 * time.Minute

	// Default cadence between sweeps. Tunable via AI_MEMORY_INTERVAL_MIN.
	memoryWorkerDefaultInterval = 1 * time.Hour

	// How far back discovery looks for "active" scopes. A scope with no new
	// content in this window drops off the work list. Per-scope watermarks
	// (not this window) bound what actually gets extracted. Tunable via
	// AI_MEMORY_LOOKBACK_HRS.
	memoryWorkerDefaultLookback = 7 * 24 * time.Hour

	// On a scope's first-ever pass (no watermark yet) we start the window
	// this far back so initial extraction captures recent history without
	// scanning the entire history of a long-lived channel.
	memoryWorkerColdStartWindow = 14 * 24 * time.Hour

	// Bounds per tick so a huge or very busy workspace can't fan out into
	// an unbounded LLM storm in a single pass; the remainder is picked up
	// next tick (their watermarks haven't advanced).
	maxScopesPerTick     = 200
	maxAggBucketsPerDim  = 1000
	maxBatchFetchItems   = 400
	maxBatchPromptChars  = 18000
	minBatchExtractChars = minExtractChars // reuse extractor's floor (200)

	// If a scope's newest content is older than this and still below the
	// extraction floor, advance its watermark anyway so we stop rescanning
	// a permanently-tiny conversation every tick.
	memoryWorkerStaleAfter = 48 * time.Hour
)

// StartMemoryExtractionLoop launches the batched extraction worker. Safe
// to call once at startup; it self-gates on settings every tick, so it is
// inert until an admin enables the memory layer. ctx drives graceful
// shutdown.
func StartMemoryExtractionLoop(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(memoryWorkerInitialDelay):
		}

		ticker := time.NewTicker(memoryWorkerInterval())
		defer ticker.Stop()

		// Run once promptly after the initial delay, then on each tick.
		runMemoryExtractionPass(ctx)
		for {
			select {
			case <-ctx.Done():
				helpers.MessageLogs.InfoLog.Println("Memory extraction worker shutting down")
				return
			case <-ticker.C:
				runMemoryExtractionPass(ctx)
			}
		}
	}()
}

// runMemoryExtractionPass performs a single sweep: gate → discover active
// scopes → extract per scope (lock + watermark bounded). Best-effort; every
// error path logs and continues so one bad scope never stalls the sweep.
func runMemoryExtractionPass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			helpers.MessageLogs.ErrorLog.Printf("panic in memory extraction pass: %v", r)
		}
	}()

	// Gate on settings + service readiness every pass (config can hot-swap).
	settings, err := getAISettingsForMemory(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory worker: load settings failed: %v", err)
		return
	}
	if !settings.enabled || !settings.memoryEnabled {
		return // layer off — silent no-op
	}
	if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
		return
	}

	sinceUnix := time.Now().Add(-memoryWorkerLookback()).Unix()
	scopes, err := ai.DiscoverActiveScopes(ctx, sinceUnix, maxAggBucketsPerDim)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory worker: discover scopes failed: %v", err)
		return
	}
	if len(scopes) == 0 {
		return
	}

	// Busiest scopes first so the per-tick cap spends the LLM budget where
	// the most new content (and thus the most durable knowledge) is.
	sort.SliceStable(scopes, func(i, j int) bool {
		return scopes[i].DocCount > scopes[j].DocCount
	})
	if len(scopes) > maxScopesPerTick {
		scopes = scopes[:maxScopesPerTick]
	}

	processed, extracted := 0, 0
	for _, sc := range scopes {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, ok := extractScope(ctx, sc)
		if ok {
			processed++
			extracted += n
		}
	}
	helpers.LogInfoWithContext(ctx,
		"memory worker: swept %d scope(s), extracted %d item(s)", processed, extracted)
}

// extractScope runs one bounded extraction for a single scope. Returns the
// number of items persisted and whether the scope was actually processed
// (false when skipped due to lock contention or an empty window). All
// errors are logged, never propagated.
func extractScope(ctx context.Context, sc ai.ActiveScope) (int, bool) {
	scopeKey := sc.FilterType + ":" + sc.FilterValue

	// Idempotency lock: first ticker/instance wins this scope's window.
	if !acquireMemoryExtractLock(ctx, scopeKey) {
		return 0, false
	}

	watermark := readMemoryWatermark(ctx, scopeKey)
	if watermark == 0 {
		watermark = time.Now().Add(-memoryWorkerColdStartWindow).Unix()
	}

	items, err := ai.FetchContentSince(ctx, sc.FilterType, sc.FilterValue, watermark, maxBatchFetchItems)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "memory worker: fetch content for %s failed: %v", scopeKey, err)
		return 0, false
	}
	if len(items) == 0 {
		return 0, false
	}

	newest := items[len(items)-1].CreatedDate

	// Too little to be worth an LLM call. Advance the watermark only if the
	// conversation has gone quiet (newest item is old), so we don't rescan
	// a permanently-tiny scope forever, but DON'T skip past content that is
	// still actively accumulating toward the threshold.
	if len(strings.TrimSpace(formatBatchWindow(items))) < minBatchExtractChars {
		if time.Since(time.Unix(newest, 0)) > memoryWorkerStaleAfter {
			writeMemoryWatermark(ctx, scopeKey, newest)
		}
		return 0, false
	}

	scope := memoryScopeFor(sc.FilterType, sc.FilterValue, scopeKey)

	// Chunked extraction handles a window larger than one prompt (a very
	// busy scope) without dropping content past the cap.
	n, _, err := extractWindowChunked(ctx, items, scope)
	if err != nil {
		// Extraction failed (e.g. circuit open) or context cancelled. Do
		// NOT advance the watermark so the window is retried next tick.
		helpers.LogErrorWithContext(ctx, "memory worker: extract for %s failed: %v", scopeKey, err)
		return 0, false
	}

	// Advance the watermark past the window we just processed.
	writeMemoryWatermark(ctx, scopeKey, newest)
	return n, true
}

// formatBatchWindow renders a content window as "Author: text" lines,
// bounded by maxBatchPromptChars. Mirrors the transcript formatter so the
// extractor sees a consistent shape regardless of source. Kept for the
// live worker's single-window path; the backfill uses the chunked engine
// (extractWindowChunked) which calls formatChunk directly.
func formatBatchWindow(items []ai.ScopedContent) string {
	text, _ := formatChunk(items, 0, maxBatchPromptChars)
	return text
}

// formatChunk renders items starting at index `start` into a prompt-sized
// block (≤ maxChars), returning the rendered text and the index of the
// FIRST unconsumed item (== len(items) when everything fit). This lets a
// long window be processed in multiple LLM calls instead of silently
// dropping everything past the cap — essential for backfill over months of
// history. A single item longer than maxChars is rune-safely truncated so
// the loop always makes progress.
func formatChunk(items []ai.ScopedContent, start, maxChars int) (string, int) {
	var sb strings.Builder
	i := start
	for ; i < len(items); i++ {
		text := strings.TrimSpace(items[i].ContentText)
		if text == "" {
			continue
		}
		name := strings.TrimSpace(items[i].AuthorName)
		if name == "" {
			name = "participant"
		}
		line := name + ": " + text + "\n"
		// If a single line alone exceeds the budget, truncate it so the
		// chunk is never empty and the cursor always advances.
		if sb.Len() == 0 && len(line) > maxChars {
			line = runeSafeTruncate(line, maxChars)
			sb.WriteString(line)
			i++
			break
		}
		if sb.Len()+len(line) > maxChars {
			break
		}
		sb.WriteString(line)
	}
	return sb.String(), i
}

// runeSafeTruncate cuts s to at most maxBytes without splitting a UTF-8
// rune.
func runeSafeTruncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// extractWindowChunked runs extraction over an arbitrarily large, already-
// fetched window by splitting it into prompt-sized chunks and calling
// MaybeExtractMemory per chunk. Returns total items persisted and whether
// any LLM call was actually made (false when the whole window was below the
// per-chunk floor). Stops early and returns the error if any chunk fails
// (e.g. circuit open), so the caller can avoid advancing a watermark past
// unprocessed content. Shared by the live worker and the admin backfill.
func extractWindowChunked(ctx context.Context, items []ai.ScopedContent, scope MemoryScope) (persisted int, llmCalled bool, err error) {
	cursor := 0
	for cursor < len(items) {
		select {
		case <-ctx.Done():
			return persisted, llmCalled, ctx.Err()
		default:
		}
		text, next := formatChunk(items, cursor, maxBatchPromptChars)
		if next <= cursor {
			break // safety: no progress possible
		}
		cursor = next
		if len(strings.TrimSpace(text)) < minBatchExtractChars {
			continue // chunk too small to be worth an LLM call
		}
		n, exErr := MaybeExtractMemory(ctx, text, scope)
		if exErr != nil {
			return persisted, llmCalled, exErr
		}
		llmCalled = true
		persisted += n
	}
	return persisted, llmCalled, nil
}

// memoryScopeFor builds a MemoryScope from a scope dimension + value. Shared
// by the batched worker and the admin backfill so both produce identically
// shaped, identically deduplicated items. sourceUUID is the provenance key
// (the scope key) used for traceability; dedup is by content+scope hash, so
// reuse across paths is safe and idempotent.
func memoryScopeFor(filterType, filterValue, sourceUUID string) MemoryScope {
	scope := MemoryScope{
		SourceType: memorySourceTypeForScope(filterType),
		SourceUUID: sourceUUID,
	}
	switch filterType {
	case "channel_uuid":
		scope.ChannelUUID = filterValue
	case "project_uuid":
		scope.ProjectUUID = filterValue
	case "chat_grp_id":
		scope.ChatGrpID = filterValue
	}
	return scope
}

// memorySourceTypeForScope maps a scope dimension to the provenance type
// stored on extracted items. It's metadata only (dedup is by content +
// scope, not source), used by the UI to label where a fact came from.
func memorySourceTypeForScope(filterType string) string {
	switch filterType {
	case "chat_grp_id":
		return memoryModels.SourceChat
	default:
		return memoryModels.SourcePost
	}
}

// --- Redis helpers: per-scope lock + watermark ---

// acquireMemoryExtractLock returns true only for the first caller within
// the lock TTL, so two ticks (or instances) can't double-extract a scope.
func acquireMemoryExtractLock(ctx context.Context, scopeKey string) bool {
	res := redisStore.AllowFixedWindow(ctx, registry.AIMemoryExtractLock, []string{scopeKey}, 1)
	return res.Allowed
}

// readMemoryWatermark returns the last-processed unix-second for a scope,
// or 0 when unset/unavailable (cold start).
func readMemoryWatermark(ctx context.Context, scopeKey string) int64 {
	val, ok, err := redisStore.GetString(ctx, registry.AIMemoryWatermark, []string{scopeKey})
	if err != nil || !ok || val == "" {
		return 0
	}
	ts, perr := strconv.ParseInt(val, 10, 64)
	if perr != nil {
		return 0
	}
	return ts
}

// writeMemoryWatermark records the last-processed unix-second for a scope.
// Best-effort: a failed write only means the next pass re-reads an
// overlapping window, which dedup_hash collapses on upsert.
func writeMemoryWatermark(ctx context.Context, scopeKey string, ts int64) {
	if ts <= 0 {
		return
	}
	if err := redisStore.SetString(ctx, registry.AIMemoryWatermark, []string{scopeKey}, strconv.FormatInt(ts, 10)); err != nil {
		helpers.LogErrorWithContext(ctx, "memory worker: persist watermark for %s failed: %v", scopeKey, err)
	}
}

// --- env-tunable knobs ---

// envInt parses an integer env var, returning 0 when unset/invalid so
// callers can apply their own default + bounds. Shared across the memory
// workers/backfill so env parsing stays in one place.
func envInt(key string) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// memoryWorkerInterval is the sweep cadence (AI_MEMORY_INTERVAL_MIN,
// minutes). Defaults to 1h; floored at 5m to avoid hammering the LLM.
func memoryWorkerInterval() time.Duration {
	if m := envInt("AI_MEMORY_INTERVAL_MIN"); m >= 5 {
		return time.Duration(m) * time.Minute
	}
	return memoryWorkerDefaultInterval
}

// memoryWorkerLookback is the discovery window (AI_MEMORY_LOOKBACK_HRS,
// hours). Defaults to 7d; floored at 1h.
func memoryWorkerLookback() time.Duration {
	if h := envInt("AI_MEMORY_LOOKBACK_HRS"); h >= 1 {
		return time.Duration(h) * time.Hour
	}
	return memoryWorkerDefaultLookback
}

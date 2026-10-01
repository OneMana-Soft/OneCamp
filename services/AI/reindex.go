package ai

// Embedding reindex job.
//
// Changing the embedding model to one with a different vector dimension
// is the single most dangerous AI config change: the OpenSearch k-NN
// index pins its dimension at create time, so a mismatch silently breaks
// every similarity search. This job rebuilds the index safely.
//
// Key insight: the ai_embeddings index already stores `content_text` and
// ALL permission metadata for every embedded item. So a reindex does NOT
// need to re-read Postgres/Dgraph — it can read each document's stored
// text + metadata out of the old index, recreate the index at the new
// dimension, and re-embed the text with the newly-active embedder. The
// index is its own source of truth for "what has been embedded."
//
// Concurrency: only one reindex runs at a time (reindexRunning guard).
// The job is best-effort and logs progress; it is not transactional —
// during the rebuild window AI search returns partial results, which is
// acceptable and clearly preferable to a permanently broken index.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

var reindexRunning atomic.Bool

// IsReindexRunning reports whether an embedding reindex is currently in
// progress. The business layer uses this to reject a second concurrent
// dimension change (which would corrupt the in-flight rebuild).
func IsReindexRunning() bool { return reindexRunning.Load() }

// --- Reindex write buffering ---------------------------------------------
//
// A dimension-changing reindex has an unavoidable window in which the live
// embedder has already been swapped to the new model (producing new-dim
// vectors) while the OpenSearch index is still pinned to the OLD dimension,
// or is mid-rebuild. A live StoreEmbedding/DeleteEmbedding during that window
// would either be rejected (vector/index dimension mismatch) or lost
// (overwritten by the snapshot→re-embed pass). To make a reindex safe under
// live traffic we buffer those writes and replay them LAST, after the bulk
// re-embed, so the final index reflects every concurrent edit/delete.

type reindexWrite struct {
	doc       EmbeddingDoc
	tombstone bool // true = delete
}

const maxReindexBuffer = 100000 // safety valve; far above realistic edit rate

var (
	reindexBufferMu      sync.Mutex
	reindexBuffering     bool
	reindexBuffer        []reindexWrite
	reindexBufferDropped int
)

// bufferReindexWrite captures a live write/delete while a reindex is active.
// Returns true when the write was buffered (caller must NOT touch the index);
// false when no reindex is in progress (caller proceeds normally).
func bufferReindexWrite(doc EmbeddingDoc, tombstone bool) bool {
	reindexBufferMu.Lock()
	defer reindexBufferMu.Unlock()
	if !reindexBuffering {
		return false
	}
	if len(reindexBuffer) >= maxReindexBuffer {
		// Extremely unlikely; refuse to grow unbounded. The dropped item
		// keeps its snapshot state until its next edit. Count for logging.
		reindexBufferDropped++
		return true
	}
	reindexBuffer = append(reindexBuffer, reindexWrite{doc: doc, tombstone: tombstone})
	return true
}

// beginReindexBuffering starts capturing live writes. Called BEFORE the live
// embedder is swapped so no write can slip through at the wrong dimension.
func beginReindexBuffering() {
	reindexBufferMu.Lock()
	defer reindexBufferMu.Unlock()
	reindexBuffering = true
	reindexBuffer = nil
	reindexBufferDropped = 0
}

// BeginReindexBuffering is the exported entry point for the business layer to
// arm buffering at the very start of a dimension change, before it swaps the
// live embedder via ReloadAIService.
func BeginReindexBuffering() { beginReindexBuffering() }

// flushReindexBuffer replays all buffered writes against the rebuilt index,
// then atomically disables buffering. It drains in a loop because replaying
// (which embeds) takes time and more writes may arrive meanwhile; the final
// empty drain flips buffering off under the lock so no write is missed.
func flushReindexBuffer(ctx context.Context) {
	var replayed int
	for {
		reindexBufferMu.Lock()
		batch := reindexBuffer
		reindexBuffer = nil
		if len(batch) == 0 {
			reindexBuffering = false
			dropped := reindexBufferDropped
			reindexBufferMu.Unlock()
			if replayed > 0 || dropped > 0 {
				helpers.LogInfoWithContext(ctx,
					"AI reindex: replayed %d buffered live write(s), dropped %d", replayed, dropped)
			}
			return
		}
		reindexBufferMu.Unlock()

		for _, w := range batch {
			select {
			case <-ctx.Done():
				// Stop replaying; disable buffering so live writes resume
				// going direct to the (rebuilt) index.
				reindexBufferMu.Lock()
				reindexBuffering = false
				reindexBuffer = nil
				reindexBufferMu.Unlock()
				return
			default:
			}
			if w.tombstone {
				_ = deleteEmbeddingDirect(ctx, w.doc.ContentType, w.doc.ContentUUID)
			} else {
				_ = storeEmbeddingDirect(ctx, w.doc)
			}
			replayed++
		}
	}
}

// abortReindexBuffering disables buffering and discards any captured writes.
// Used when a reindex fails before it can replay — live writes then resume
// going direct (the normal error path applies if the index is inconsistent).
func abortReindexBuffering(ctx context.Context) {
	reindexBufferMu.Lock()
	defer reindexBufferMu.Unlock()
	if dropped := len(reindexBuffer); dropped > 0 {
		helpers.LogErrorWithContext(ctx,
			"AI reindex aborted: discarding %d buffered live write(s)", dropped)
	}
	reindexBuffer = nil
	reindexBuffering = false
}

// ReindexState reports the live status of a reindex for the admin panel.
type ReindexState struct {
	Running   bool   `json:"running"`
	Total     int    `json:"total"`
	Processed int    `json:"processed"`
	Failed    int    `json:"failed"`
	Dimension int    `json:"dimension"`
	StartedAt string `json:"started_at,omitempty"`
	Message   string `json:"message,omitempty"`
}

var reindexState atomic.Pointer[ReindexState]

// GetReindexState returns a snapshot of the current/last reindex.
func GetReindexState() ReindexState {
	if s := reindexState.Load(); s != nil {
		return *s
	}
	return ReindexState{}
}

func setReindexState(s ReindexState) { reindexState.Store(&s) }

// StartEmbeddingReindexAsync launches a reindex in the background. Returns
// false (and logs) if a reindex is already running, so the caller can tear
// down any write-buffering it armed in anticipation.
func StartEmbeddingReindexAsync(dimension int) bool {
	if !reindexRunning.CompareAndSwap(false, true) {
		helpers.MessageLogs.ErrorLog.Println("AI reindex already running; ignoring duplicate request")
		return false
	}

	// GoSafeNamed, not a bare goroutine: this runs unattended for up to six
	// hours over OpenSearch responses, and an unrecovered panic in any goroutine
	// takes the whole server down. It also arms write-buffering, so a crash here
	// used to skip the teardown below and leave live writes buffered until a
	// restart. The recover keeps both defers on the normal unwind path.
	helpers.GoSafeNamed("ai.embedding-reindex", func() {
		defer reindexRunning.Store(false)
		// Generous deadline: re-embedding tens of thousands of items on a
		// CPU-bound local model is slow. The job streams in batches so a
		// long total runtime is fine.
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()

		if err := runEmbeddingReindex(ctx, dimension); err != nil {
			// Make sure buffering is torn down on any failure so live
			// writes resume going direct to the index (idempotent if the
			// job already aborted/flushed it).
			abortReindexBuffering(ctx)
			helpers.LogErrorWithContext(ctx, "AI embedding reindex failed: %v", err)
			st := GetReindexState()
			st.Running = false
			st.Message = "failed: " + err.Error()
			setReindexState(st)
		}
	})
	return true
}

// AbortReindexBuffering is the exported teardown for the business layer when
// it armed buffering but the reindex didn't start (lost the start race).
func AbortReindexBuffering() { abortReindexBuffering(context.Background()) }

// storedDoc is the metadata-bearing form read back from the old index.
// It deliberately omits the embedding vector (we regenerate it).
type storedDoc struct {
	EmbeddingDoc
	ContentText string `json:"content_text"`
}

// runEmbeddingReindex performs: snapshot old docs → recreate index at the
// new dimension → re-embed and re-store in batches.
func runEmbeddingReindex(ctx context.Context, dimension int) error {
	start := time.Now()
	setReindexState(ReindexState{Running: true, Dimension: dimension, StartedAt: start.Format(time.RFC3339), Message: "snapshotting existing documents"})

	// 1. Snapshot all existing docs (text + metadata) from the old index
	//    via the scroll API. We hold them in memory; for very large
	//    workspaces this could be chunked to disk, but the metadata-only
	//    footprint (no vectors) is modest.
	docs, err := scrollAllEmbeddingDocs(ctx)
	if err != nil {
		return fmt.Errorf("snapshot existing embeddings: %w", err)
	}

	helpers.LogInfoWithContext(ctx, "AI reindex: snapshotted %d documents", len(docs))
	setReindexState(ReindexState{Running: true, Dimension: dimension, Total: len(docs), StartedAt: start.Format(time.RFC3339), Message: "recreating index"})

	// 2. Recreate the index at the new dimension (DESTROYS old vectors).
	if err := opensearchInit.RecreateAIEmbeddingsIndex(ctx, dimension); err != nil {
		return fmt.Errorf("recreate index: %w", err)
	}

	// 3. Re-embed and re-store with a bounded worker pool. StoreEmbedding
	//    uses the live service's embedder, which ReloadAIService already
	//    swapped to the new model. Concurrency is intentionally modest:
	//    local CPU inference (Ollama) is the bottleneck and over-
	//    subscribing it hurts throughput and starves live AI requests.
	//    Tune via AI_REINDEX_CONCURRENCY (default 4).
	workers := getEnvInt("AI_REINDEX_CONCURRENCY", 4)
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}

	var processed, failed atomic.Int64
	jobs := make(chan storedDoc)
	var wg sync.WaitGroup

	updateProgress := func() {
		p, f := int(processed.Load()), int(failed.Load())
		setReindexState(ReindexState{
			Running: true, Total: len(docs), Processed: p, Failed: f,
			Dimension: dimension, StartedAt: start.Format(time.RFC3339),
			Message: "re-embedding",
		})
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		// Guarded for the same reason as the job above, and because wg.Done()
		// must still fire: a panicking worker that skipped it would leave the
		// parent blocked on wg.Wait() forever instead of finishing the rebuild.
		helpers.GoSafeNamed("ai.embedding-reindex.worker", func() {
			defer wg.Done()
			for d := range jobs {
				// storeEmbeddingDirect regenerates the vector from
				// ContentText and re-applies the upsert doc-id
				// (content_type:content_uuid). We MUST bypass the reindex
				// write buffer here — these are the rebuild writes
				// themselves, not concurrent live traffic.
				doc := d.EmbeddingDoc
				doc.ContentText = d.ContentText
				if err := storeEmbeddingDirect(ctx, doc); err != nil {
					failed.Add(1)
					helpers.LogErrorWithContext(ctx, "AI reindex: re-embed %s:%s failed: %v", doc.ContentType, doc.ContentUUID, err)
				} else {
					processed.Add(1)
				}
				if (processed.Load()+failed.Load())%50 == 0 {
					updateProgress()
				}
			}
		})
	}

	// Feed jobs, honoring cancellation.
feed:
	for _, d := range docs {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- d:
		}
	}
	close(jobs)
	wg.Wait()

	pDone, fDone := int(processed.Load()), int(failed.Load())
	if ctx.Err() != nil {
		// Cancelled mid-rebuild: stop buffering and discard captured writes
		// (the index is partial anyway; live writes resume going direct).
		abortReindexBuffering(ctx)
		setReindexState(ReindexState{
			Running: false, Total: len(docs), Processed: pDone, Failed: fDone,
			Dimension: dimension, StartedAt: start.Format(time.RFC3339),
			Message: "cancelled: " + ctx.Err().Error(),
		})
		return ctx.Err()
	}

	// 4. Replay every live write/delete that arrived during the rebuild so
	//    the new index reflects concurrent edits. This flips buffering off
	//    once the buffer fully drains.
	flushReindexBuffer(ctx)

	setReindexState(ReindexState{
		Running: false, Total: len(docs), Processed: pDone, Failed: fDone,
		Dimension: dimension, StartedAt: start.Format(time.RFC3339),
		Message: fmt.Sprintf("completed in %s", time.Since(start).Round(time.Second)),
	})
	helpers.LogInfoWithContext(ctx, "AI reindex complete: %d ok, %d failed, dim=%d, workers=%d, took %s",
		pDone, fDone, dimension, workers, time.Since(start).Round(time.Second))
	return nil
}

// scrollAllEmbeddingDocs reads every document (text + metadata, no vector)
// from the ai_embeddings index using the scroll API.
func scrollAllEmbeddingDocs(ctx context.Context) ([]storedDoc, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}

	const batch = 500
	// Exclude the (potentially large) embedding vector from _source — we
	// only need the text + metadata to re-embed.
	body := fmt.Sprintf(`{
		"size": %d,
		"_source": { "excludes": ["embedding"] },
		"query": { "match_all": {} }
	}`, batch)

	var out []storedDoc

	type hit struct {
		Source json.RawMessage `json:"_source"`
	}
	type searchResp struct {
		ScrollID string `json:"_scroll_id"`
		Hits     struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}

	var resp searchResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(body),
		Params:  opensearchapi.SearchParams{Scroll: 5 * time.Minute},
	}, &resp)
	if err != nil {
		// A missing index means nothing to reindex — not an error.
		if strings.Contains(err.Error(), "index_not_found") {
			return nil, nil
		}
		return nil, fmt.Errorf("initial scroll: %w", err)
	}

	appendHits := func(hits []hit) {
		for _, h := range hits {
			var d storedDoc
			if err := json.Unmarshal(h.Source, &d); err != nil {
				continue
			}
			if strings.TrimSpace(d.ContentText) == "" {
				continue
			}
			out = append(out, d)
		}
	}
	appendHits(resp.Hits.Hits)
	scrollID := resp.ScrollID

	// Continue scrolling until a page returns no hits.
	for len(resp.Hits.Hits) > 0 && scrollID != "" {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}

		var next searchResp
		_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.ScrollGetReq{
			ScrollID: scrollID,
			Params:   opensearchapi.ScrollGetParams{Scroll: 5 * time.Minute},
		}, &next)
		if err != nil {
			break
		}
		if len(next.Hits.Hits) == 0 {
			break
		}
		appendHits(next.Hits.Hits)
		resp.Hits.Hits = next.Hits.Hits
		scrollID = next.ScrollID
	}

	// Best-effort: clear the scroll context.
	if scrollID != "" {
		_, _ = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.ScrollDeleteReq{
			ScrollIDs: []string{scrollID},
		}, nil)
	}

	return out, nil
}

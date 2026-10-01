package business

// Board snapshots: a recoverable version history that protects boards against
// accidental mass-deletion of the canvas.
//
// Storage is a hybrid by design (see migration 85): the snapshot INDEX lives in
// Postgres (board_snapshots) while the snapshot BLOB - a gzipped copy of the
// base64 Yjs document - lives in MinIO. This keeps the hot databases lean and
// scales regardless of board size. Retention is bounded two ways so storage
// never grows without limit:
//   - a per-board cap (keep the most recent N), pruned on every write
//   - a global age sweep (drop snapshots older than the retention window)
//
// Snapshots are captured opportunistically on the persist path (no extra
// timers per board): periodically while a board is being edited, and - the key
// protection - the PRIOR state is captured whenever the incoming state shrinks
// sharply (the signature of a select-all-delete or a bad import).

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Board"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/BoardSnapshot"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// Tuning. All are conservative defaults; the retention window is env-tunable.
const (
	// Minimum spacing between periodic (interval) snapshots of an active board.
	snapshotMinInterval = 10 * time.Minute
	// Most recent snapshots to keep per board (older ones are pruned).
	snapshotKeepPerBoard = 15
	// Mass-delete heuristic: the incoming state must be below this fraction of
	// the prior state's size, and the prior state must have been at least
	// snapshotMassDeleteMinBytes, to count as a sharp shrink worth preserving.
	snapshotMassDeleteRatio    = 0.4
	snapshotMassDeleteMinBytes = 2048
	// Default age retention (days); override with BOARD_SNAPSHOT_RETENTION_DAYS.
	defaultSnapshotRetentionDays = 30
)

// snapshotBucket returns the object-storage bucket for snapshots, reusing the
// shared user-upload bucket (same as board images and import staging).
func snapshotBucket() string {
	return helpers.UserUploadBucket()
}

func snapshotObjectKey(boardUUID string) string {
	return fmt.Sprintf("boardSnapshot/%s/%s.gz", boardUUID, uuid.New().String())
}

// parseElementCount extracts the leading integer from a collab snippet such as
// "42 elements". Returns 0 when not parseable.
func parseElementCount(snippet string) int {
	fields := strings.Fields(strings.TrimSpace(snippet))
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	// Cap the decompressed size defensively (states are bounded by the board
	// element cap; 64 MB is far above any legitimate board).
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

func putSnapshotObject(ctx context.Context, key string, body []byte) error {
	_, err := minioInit.MinioClient.PutObject(ctx, snapshotBucket(), key,
		bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/gzip"})
	return err
}

func getSnapshotObject(ctx context.Context, key string) ([]byte, error) {
	obj, err := minioInit.MinioClient.GetObject(ctx, snapshotBucket(), key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(io.LimitReader(obj, 64<<20))
}

func removeSnapshotObject(ctx context.Context, key string) {
	if key == "" {
		return
	}
	_ = minioInit.MinioClient.RemoveObject(ctx, snapshotBucket(), key, minio.RemoveObjectOptions{})
}

// liveStateObjectKey is the stable object-storage key for a board's current
// canvas state. It is overwritten on every save (history lives in snapshots),
// so a board never accumulates live-state objects.
func liveStateObjectKey(boardUUID string) string {
	return fmt.Sprintf("boardState/%s.gz", boardUUID)
}

// putLiveBoardState gzips the base64 canvas state and uploads it to object
// storage, returning the stable key. Keeping the blob in MinIO (not dgraph)
// is what makes large boards production-safe: dgraph holds only the pointer.
func putLiveBoardState(ctx context.Context, boardUUID, stateB64 string) (string, error) {
	gz, err := gzipBytes([]byte(stateB64))
	if err != nil {
		return "", err
	}
	key := liveStateObjectKey(boardUUID)
	if err := putSnapshotObject(ctx, key, gz); err != nil {
		return "", err
	}
	return key, nil
}

// resolveBoardState fills b.State from object storage when the board stores its
// canvas via the pointer (board_state_key). Legacy boards keep their inline
// board_state and are left untouched, so reads work during and after migration.
func resolveBoardState(ctx context.Context, b *dgraphStruct.DgraphBoard) {
	if b == nil {
		return
	}
	// Inline board_state, when present, is always the freshest: a successful
	// object-storage save purges it, so a non-empty inline value means either a
	// legacy (un-migrated) board or an object-storage-unavailable fallback write.
	// Only resolve from object storage when there is no inline value.
	if b.State != "" || b.StateKey == "" {
		return
	}
	raw, err := getSnapshotObject(ctx, b.StateKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/resolveBoardState fetch failed key=%s err: %+v", b.StateKey, err)
		return
	}
	plain, err := gunzipBytes(raw)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/resolveBoardState gunzip failed key=%s err: %+v", b.StateKey, err)
		return
	}
	b.State = string(plain)
}

// storeSnapshot gzips and uploads the state blob, records the index row, then
// prunes the board to the per-board cap (removing the orphaned objects).
// contributors is the set of editor uuids attributed to this version.
func storeSnapshot(ctx context.Context, boardUUID, stateB64 string, elementCount int, reason string, contributors []string) error {
	if strings.TrimSpace(stateB64) == "" {
		return nil
	}
	gz, err := gzipBytes([]byte(stateB64))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/storeSnapshot gzip failed err: %+v", err)
		return err
	}

	key := snapshotObjectKey(boardUUID)
	if err := putSnapshotObject(ctx, key, gz); err != nil {
		helpers.LogErrorWithContext(ctx, "business/storeSnapshot upload failed err: %+v", err)
		return err
	}

	if _, err := snapshotModels.CreateSnapshot(ctx, &snapshotModels.BoardSnapshot{
		BoardUUID:        boardUUID,
		ObjectKey:        key,
		ElementCount:     elementCount,
		StateBytes:       len(stateB64),
		Reason:           reason,
		ContributorUUIDs: dedupeNonEmpty(contributors),
	}); err != nil {
		// Don't leave an orphaned object if the index write failed.
		removeSnapshotObject(context.Background(), key)
		return err
	}

	// Enforce the per-board cap; delete the backing objects of pruned rows.
	if keys, perr := snapshotModels.PruneBoardSnapshots(ctx, boardUUID, snapshotKeepPerBoard); perr == nil {
		for _, k := range keys {
			removeSnapshotObject(context.Background(), k)
		}
	}
	return nil
}

// dedupeNonEmpty returns the unique, non-empty values of in, preserving order.
// Always returns a non-nil slice so the DB stores an empty array, not NULL.
func dedupeNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// MaybeSnapshotBoard decides whether to capture a snapshot for a persist event
// and does so. Intended to be called in a goroutine off the persist path.
//
//   - If the incoming state shrank sharply versus the prior state, the PRIOR
//     state is snapshotted (mass-delete protection) and we stop - there is no
//     value in also snapshotting the tiny new state.
//   - Otherwise, the current state is snapshotted at most once per interval.
func MaybeSnapshotBoard(ctx context.Context, boardUUID, oldState, oldSnippet, newState, newSnippet string, contributors []string) {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotBoard recovered from panic: %v", r)
		}
	}()

	oldBytes := len(oldState)
	newBytes := len(newState)

	if oldBytes >= snapshotMassDeleteMinBytes && float64(newBytes) < float64(oldBytes)*snapshotMassDeleteRatio {
		if err := storeSnapshot(ctx, boardUUID, oldState, parseElementCount(oldSnippet), snapshotModels.ReasonMassDelete, contributors); err != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotBoard mass-delete snapshot failed err: %+v", err)
		}
		return
	}

	if newBytes == 0 {
		return
	}

	latest, err := snapshotModels.GetLatestSnapshot(ctx, boardUUID)
	if err != nil {
		return
	}
	if latest == nil || time.Since(latest.CreatedAt) >= snapshotMinInterval {
		if err := storeSnapshot(ctx, boardUUID, newState, parseElementCount(newSnippet), snapshotModels.ReasonInterval, contributors); err != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotBoard interval snapshot failed err: %+v", err)
		}
	}
}

// SnapshotContributor is a resolved editor shown in the version-history UI.
type SnapshotContributor = userDomain.UserDisplay

// BoardSnapshotView is the version-history row returned to the client: snapshot
// metadata plus its resolved contributors (avatars/names).
type BoardSnapshotView struct {
	ID           string                `json:"id"`
	ElementCount int                   `json:"element_count"`
	StateBytes   int                   `json:"state_bytes"`
	Reason       string                `json:"reason"`
	CreatedAt    time.Time             `json:"created_at"`
	Contributors []SnapshotContributor `json:"contributors"`
}

// ListBoardSnapshots returns a board's version history for the UI, with each
// snapshot's contributors resolved to name/avatar. Requires edit access or
// ownership (same gate as managing the board).
func ListBoardSnapshots(ctx context.Context, boardUUID string, userUID string) ([]*BoardSnapshotView, error) {
	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userUID)
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, errors.New("board not found")
	}
	isOwner := board.CreatedBy != nil && board.CreatedBy.Uid == userUID
	if !isOwner && board.HasEditAccess == 0 {
		return nil, errors.New("unauthorized")
	}

	rows, err := snapshotModels.ListSnapshots(ctx, boardUUID, 50)
	if err != nil {
		return nil, err
	}

	// Resolve every distinct contributor in ONE batch query (no N+1 loop).
	uuidSet := make(map[string]struct{})
	for _, r := range rows {
		for _, u := range r.ContributorUUIDs {
			if u != "" {
				uuidSet[u] = struct{}{}
			}
		}
	}
	allUUIDs := make([]string, 0, len(uuidSet))
	for u := range uuidSet {
		allUUIDs = append(allUUIDs, u)
	}
	resolved, err := userDomain.ResolveUserDisplays(ctx, allUUIDs)
	if err != nil {
		// Degrade gracefully: still return snapshots, just without names.
		resolved = map[string]userDomain.UserDisplay{}
	}

	views := make([]*BoardSnapshotView, 0, len(rows))
	for _, r := range rows {
		contributors := make([]SnapshotContributor, 0, len(r.ContributorUUIDs))
		for _, uuidStr := range r.ContributorUUIDs {
			if c, ok := resolved[uuidStr]; ok {
				contributors = append(contributors, c)
			}
		}
		views = append(views, &BoardSnapshotView{
			ID:           r.ID.String(),
			ElementCount: r.ElementCount,
			StateBytes:   r.StateBytes,
			Reason:       r.Reason,
			CreatedAt:    r.CreatedAt,
			Contributors: contributors,
		})
	}
	return views, nil
}

// RestoreBoardSnapshot rewrites the board's persisted state to a chosen
// snapshot. The current state is first captured as a 'manual' snapshot so the
// restore is itself reversible. Requires edit access or ownership.
//
// Note: the restored state takes effect when the board is next opened with no
// active collaborators connected (the live Yjs document, if loaded, keeps its
// in-memory state until all clients disconnect). The controller surfaces this
// to the user.
func RestoreBoardSnapshot(ctx context.Context, boardUUID string, snapshotID uuid.UUID, userUID string, userUUID string) error {
	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userUID)
	if err != nil {
		return err
	}
	if board == nil {
		return errors.New("board not found")
	}
	isOwner := board.CreatedBy != nil && board.CreatedBy.Uid == userUID
	if !isOwner && board.HasEditAccess == 0 {
		return errors.New("unauthorized: edit access required to restore")
	}

	snap, err := snapshotModels.GetSnapshotByID(ctx, snapshotID, boardUUID)
	if err != nil {
		return err
	}
	if snap == nil {
		return errors.New("snapshot not found")
	}

	gz, err := getSnapshotObject(ctx, snap.ObjectKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot fetch blob failed err: %+v", err)
		return errors.New("could not load the snapshot")
	}
	stateBytes, err := gunzipBytes(gz)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot decompress failed err: %+v", err)
		return errors.New("the snapshot is corrupted")
	}
	stateB64 := string(stateBytes)
	if strings.TrimSpace(stateB64) == "" {
		return errors.New("the snapshot is empty")
	}

	// Capture the current (pre-restore) state so the restore can be undone.
	// Attribute it to the user performing the restore. Uses the business read so
	// the current canvas is resolved from object storage when applicable.
	if cur, cerr := GetSystemBoardByUUID(ctx, boardUUID); cerr == nil && cur != nil && cur.State != "" {
		if serr := storeSnapshot(ctx, boardUUID, cur.State, parseElementCount(cur.Snippet), snapshotModels.ReasonManual, []string{userUUID}); serr != nil {
			helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot pre-restore snapshot failed err: %+v", serr)
		}
	}

	now := time.Now()
	snippet := fmt.Sprintf("%d elements", snap.ElementCount)
	// Persist the restored canvas the same way as a live save: blob to object
	// storage, pointer in dgraph. Fall back to an inline write only if object
	// storage is unavailable (inline always wins on read, so this stays correct).
	if key, uerr := putLiveBoardState(ctx, boardUUID, stateB64); uerr == nil {
		if serr := domain.SetBoardStateRef(ctx, boardUUID, key, snippet); serr != nil {
			helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot persist ref failed err: %+v", serr)
			return errors.New("could not restore the board")
		}
	} else {
		helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot object-storage write failed, falling back to inline err: %+v", uerr)
		restored := &dgraphStruct.DgraphBoard{
			Uid:       "uid(board)",
			Uuid:      boardUUID,
			State:     stateB64,
			Snippet:   snippet,
			UpdatedAt: &now,
		}
		if _, err := domain.CreateOrUpdateDgraphBoard(ctx, restored); err != nil {
			helpers.LogErrorWithContext(ctx, "business/RestoreBoardSnapshot persist failed err: %+v", err)
			return errors.New("could not restore the board")
		}
	}
	return nil
}

// snapshotRetentionDays resolves the age retention window from the environment.
func snapshotRetentionDays() int {
	if v := os.Getenv("BOARD_SNAPSHOT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultSnapshotRetentionDays
}

// StartBoardSnapshotCleanupLoop runs daily and deletes snapshots past the age
// retention window (both the MinIO object and the index row), bounded per tick
// so a backlog can't hammer object storage. Wire from the router alongside the
// other background loops.
func StartBoardSnapshotCleanupLoop() {
	go func() {
		// Stagger the first run so it doesn't line up with other startup loops.
		time.Sleep(20 * time.Minute)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			runBoardSnapshotCleanupTick()
			<-t.C
		}
	}()
}

func runBoardSnapshotCleanupTick() {
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "business/board snapshot cleanup recovered from panic: %v", r)
		}
	}()

	cutoff := time.Now().AddDate(0, 0, -snapshotRetentionDays())
	expired, err := snapshotModels.ListExpiredSnapshots(ctx, cutoff, 200)
	if err != nil || len(expired) == 0 {
		return
	}

	ids := make([]uuid.UUID, 0, len(expired))
	for _, e := range expired {
		removeSnapshotObject(ctx, e.ObjectKey)
		ids = append(ids, e.ID)
	}
	if err := snapshotModels.DeleteByIDs(ctx, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/board snapshot cleanup delete rows failed err: %+v", err)
	}
}

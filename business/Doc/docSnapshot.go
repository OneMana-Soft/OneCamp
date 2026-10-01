package businness

// Document version history: a recoverable history of a doc's body that protects
// against accidental mass-deletion (select-all-delete, bad paste, bad import).
//
// Mirrors the board snapshot system: the index lives in Postgres (doc_snapshots)
// while the blob (gzipped HTML body) lives in MinIO. Retention is bounded by a
// per-doc cap (pruned on write) and a daily age sweep. Snapshots are captured on
// the collab persist path: periodically while editing, and the PRIOR body when
// the incoming body shrinks sharply. Each version is attributed to the set of
// users who edited it (see the collaboration service's contributor tracking).

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

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	resourceViewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const (
	docSnapshotMinInterval       = 10 * time.Minute
	docSnapshotKeepPerDoc        = 15
	docSnapshotMassDeleteRatio   = 0.4
	docSnapshotMassDeleteMinByte = 512
	defaultDocSnapshotRetention  = 30
)

func docSnapshotBucket() string {
	return helpers.UserUploadBucket()
}

func docSnapshotObjectKey(docUUID string) string {
	return fmt.Sprintf("docSnapshot/%s/%s.gz", docUUID, uuid.New().String())
}

func docGzip(data []byte) ([]byte, error) {
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

func docGunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

func putDocSnapshotObject(ctx context.Context, key string, body []byte) error {
	_, err := minioInit.MinioClient.PutObject(ctx, docSnapshotBucket(), key,
		bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/gzip"})
	return err
}

func getDocSnapshotObject(ctx context.Context, key string) ([]byte, error) {
	obj, err := minioInit.MinioClient.GetObject(ctx, docSnapshotBucket(), key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(io.LimitReader(obj, 64<<20))
}

func removeDocSnapshotObject(ctx context.Context, key string) {
	if key == "" {
		return
	}
	_ = minioInit.MinioClient.RemoveObject(ctx, docSnapshotBucket(), key, minio.RemoveObjectOptions{})
}

func docDedupeNonEmpty(in []string) []string {
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

// storeDocSnapshot gzips+uploads the body, records the index row, then prunes.
func storeDocSnapshot(ctx context.Context, docUUID, body, reason string, contributors []string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	gz, err := docGzip([]byte(body))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/storeDocSnapshot gzip failed err: %+v", err)
		return err
	}
	key := docSnapshotObjectKey(docUUID)
	if err := putDocSnapshotObject(ctx, key, gz); err != nil {
		helpers.LogErrorWithContext(ctx, "business/storeDocSnapshot upload failed err: %+v", err)
		return err
	}
	if _, err := snapshotModels.CreateSnapshot(ctx, &snapshotModels.DocSnapshot{
		DocUUID:          docUUID,
		ObjectKey:        key,
		BodyBytes:        len(body),
		Reason:           reason,
		ContributorUUIDs: docDedupeNonEmpty(contributors),
	}); err != nil {
		removeDocSnapshotObject(context.Background(), key)
		return err
	}
	if keys, perr := snapshotModels.PruneDocSnapshots(ctx, docUUID, docSnapshotKeepPerDoc); perr == nil {
		for _, k := range keys {
			removeDocSnapshotObject(context.Background(), k)
		}
	}
	return nil
}

// MaybeSnapshotDoc decides whether to capture a snapshot for a persist event.
// Mirrors the board logic: prior body on a sharp shrink (mass-delete), else the
// current body at most once per interval.
func MaybeSnapshotDoc(ctx context.Context, docUUID, oldBody, newBody string, contributors []string) {
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotDoc recovered from panic: %v", r)
		}
	}()

	oldBytes := len(oldBody)
	newBytes := len(newBody)

	if oldBytes >= docSnapshotMassDeleteMinByte && float64(newBytes) < float64(oldBytes)*docSnapshotMassDeleteRatio {
		if err := storeDocSnapshot(ctx, docUUID, oldBody, snapshotModels.ReasonMassDelete, contributors); err != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotDoc mass-delete snapshot failed err: %+v", err)
		}
		return
	}

	if newBytes == 0 {
		return
	}

	latest, err := snapshotModels.GetLatestSnapshot(ctx, docUUID)
	if err != nil {
		return
	}
	if latest == nil || time.Since(latest.CreatedAt) >= docSnapshotMinInterval {
		if err := storeDocSnapshot(ctx, docUUID, newBody, snapshotModels.ReasonInterval, contributors); err != nil {
			helpers.LogErrorWithContext(ctx, "business/MaybeSnapshotDoc interval snapshot failed err: %+v", err)
		}
	}
}

// DocSnapshotContributor / DocSnapshotView mirror the board version-history
// view: snapshot metadata plus resolved contributors.
type DocSnapshotContributor = userDomain.UserDisplay

type DocSnapshotView struct {
	ID           string                   `json:"id"`
	BodyBytes    int                      `json:"body_bytes"`
	Reason       string                   `json:"reason"`
	CreatedAt    time.Time                `json:"created_at"`
	Contributors []DocSnapshotContributor `json:"contributors"`
}

// ListDocSnapshots returns a doc's version history with contributors resolved.
// Requires edit access or ownership.
func ListDocSnapshots(ctx context.Context, docUUID string, userUID string) ([]*DocSnapshotView, error) {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("doc not found")
	}
	isOwner := doc.CreatedBy != nil && doc.CreatedBy.Uid == userUID
	if !isOwner && doc.HasEditAccess == 0 {
		return nil, errors.New("unauthorized")
	}

	rows, err := snapshotModels.ListSnapshots(ctx, docUUID, 50)
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
		resolved = map[string]userDomain.UserDisplay{}
	}

	views := make([]*DocSnapshotView, 0, len(rows))
	for _, r := range rows {
		contributors := make([]DocSnapshotContributor, 0, len(r.ContributorUUIDs))
		for _, uuidStr := range r.ContributorUUIDs {
			if c, ok := resolved[uuidStr]; ok {
				contributors = append(contributors, c)
			}
		}
		views = append(views, &DocSnapshotView{
			ID:           r.ID.String(),
			BodyBytes:    r.BodyBytes,
			Reason:       r.Reason,
			CreatedAt:    r.CreatedAt,
			Contributors: contributors,
		})
	}
	return views, nil
}

// RestoreDocSnapshot restores a doc's body to a chosen snapshot. The current
// body is first snapshotted (attributed to the restoring user) so the restore
// is reversible. Requires edit access or ownership. Takes effect on the next
// fresh load (no connected collaborators).
func RestoreDocSnapshot(ctx context.Context, docUUID string, snapshotID uuid.UUID, userUID string, userUUID string) error {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return err
	}
	if doc == nil {
		return errors.New("doc not found")
	}
	isOwner := doc.CreatedBy != nil && doc.CreatedBy.Uid == userUID
	if !isOwner && doc.HasEditAccess == 0 {
		return errors.New("unauthorized: edit access required to restore")
	}

	snap, err := snapshotModels.GetSnapshotByID(ctx, snapshotID, docUUID)
	if err != nil {
		return err
	}
	if snap == nil {
		return errors.New("snapshot not found")
	}

	gz, err := getDocSnapshotObject(ctx, snap.ObjectKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreDocSnapshot fetch blob failed err: %+v", err)
		return errors.New("could not load the snapshot")
	}
	bodyBytes, err := docGunzip(gz)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreDocSnapshot decompress failed err: %+v", err)
		return errors.New("the snapshot is corrupted")
	}
	body := string(bodyBytes)
	if strings.TrimSpace(body) == "" {
		return errors.New("the snapshot is empty")
	}

	// Snapshot the current body (pre-restore) so the restore can be undone.
	if cur, cerr := domain.GetSystemDocByUUID(ctx, docUUID); cerr == nil && cur != nil && cur.Body != "" {
		if serr := storeDocSnapshot(ctx, docUUID, cur.Body, snapshotModels.ReasonManual, []string{userUUID}); serr != nil {
			helpers.LogErrorWithContext(ctx, "business/RestoreDocSnapshot pre-restore snapshot failed err: %+v", serr)
		}
	}

	// Persist the restored body (reuses UpdateDoc: recomputes snippet, syncs
	// OpenSearch, re-embeds). Effective on next fresh collab load.
	if err := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: docUUID, Body: &body}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreDocSnapshot persist failed err: %+v", err)
		return errors.New("could not restore the document")
	}
	return nil
}

func docSnapshotRetentionDays() int {
	if v := os.Getenv("DOC_SNAPSHOT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultDocSnapshotRetention
}

// StartDocSnapshotCleanupLoop runs daily and age-prunes doc snapshots.
func StartDocSnapshotCleanupLoop() {
	go func() {
		time.Sleep(25 * time.Minute)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			runDocSnapshotCleanupTick()
			<-t.C
		}
	}()
}

func runDocSnapshotCleanupTick() {
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx, "business/doc snapshot cleanup recovered from panic: %v", r)
		}
	}()

	cutoff := time.Now().AddDate(0, 0, -docSnapshotRetentionDays())
	expired, err := snapshotModels.ListExpiredSnapshots(ctx, cutoff, 200)
	if err != nil || len(expired) == 0 {
		return
	}
	ids := make([]uuid.UUID, 0, len(expired))
	for _, e := range expired {
		removeDocSnapshotObject(ctx, e.ObjectKey)
		ids = append(ids, e.ID)
	}
	if err := snapshotModels.DeleteByIDs(ctx, ids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/doc snapshot cleanup delete rows failed err: %+v", err)
	}
}

// UpdateDocFromCollab persists the doc body from the collaboration service and
// evaluates a version snapshot off the persist path. Reads the prior body first
// so a mass-deletion can preserve it. Best-effort snapshotting never blocks the
// save.
func UpdateDocFromCollab(ctx context.Context, docUUID, htmlBody string, contributors []string) error {
	newBytes := len(htmlBody)

	// Only read the prior body when a mass-deletion is plausible (cheap cached
	// size check); a normal edit skips the read. Cache miss / Redis-down falls
	// back to reading so detection is never silently lost.
	needOld := true
	if cached, ok := resourceViewBusiness.CachedStateSize(ctx, resourceViewBusiness.ResourceDoc, docUUID); ok {
		needOld = cached >= docSnapshotMassDeleteMinByte && float64(newBytes) < float64(cached)*docSnapshotMassDeleteRatio
	}

	var oldBody string
	if needOld {
		if prev, perr := domain.GetSystemDocByUUID(ctx, docUUID); perr == nil && prev != nil {
			oldBody = prev.Body
		}
	}

	if err := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: docUUID, Body: &htmlBody}); err != nil {
		return err
	}

	resourceViewBusiness.SetCachedStateSize(ctx, resourceViewBusiness.ResourceDoc, docUUID, newBytes)

	go MaybeSnapshotDoc(context.Background(), docUUID, oldBody, htmlBody, contributors)
	return nil
}

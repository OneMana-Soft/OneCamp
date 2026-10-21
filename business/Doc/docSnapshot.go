package businness

// Document version history, as business/Doc reads it: the index lives in
// Postgres (doc_snapshots) and each version's body, gzipped HTML, in object
// storage. Versions are taken, kept, named and pruned by the Snapshot Service
// (business/Snapshot), which registers its save hook here; this file keeps the
// reads the doc handlers still serve, the live restore, and the rules the save
// path shares (the body hash, the unsafe-body check, a mass deletion).

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

const (
	docSnapshotMassDeleteRatio   = 0.4
	docSnapshotMassDeleteMinByte = 512
)

func docSnapshotBucket() string {
	return helpers.UserUploadBucket()
}

func docGunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 64<<20))
}

// removeDocSnapshotObject deletes a version's body from object storage, for a
// purge that removes a doc's versions with it (A3's trash, docTrash.go).
func removeDocSnapshotObject(ctx context.Context, key string) {
	if key == "" {
		return
	}
	_ = minioInit.MinioClient.RemoveObject(ctx, docSnapshotBucket(), key, minio.RemoveObjectOptions{})
}

func getDocSnapshotObject(ctx context.Context, key string) ([]byte, error) {
	obj, err := minioInit.MinioClient.GetObject(ctx, docSnapshotBucket(), key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(io.LimitReader(obj, 64<<20))
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
		return nil, ErrNotFound
	}
	if IsDeleted(doc) {
		return nil, goneFor(CouldEdit(doc, userUID))
	}
	if !CanEdit(doc, userUID) {
		return nil, errors.New("unauthorized")
	}

	rows, err := snapshotModels.ListSnapshots(ctx, docUUID, time.Time{}, 50)
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

// RestoreDocSnapshot makes a version the doc's body, live: one transaction on
// the copy the collaboration service holds, so everyone with the doc open sees it
// at once and the next save keeps it (it used to write the body underneath the
// live copy, and the next save undid it). The body as it was is kept first, as a
// version credited to the restorer ("manual": before a restore), so the restore
// can itself be undone. Requires edit access or ownership.
//
// The Snapshot Service's Restore does the same through the Access Resolver
// (business/Snapshot); this serves /doc/restoreDocSnapshot until the history
// UI moves to /doc/versions.
func RestoreDocSnapshot(ctx context.Context, docUUID string, snapshotID uuid.UUID, userUID string, userUUID string) error {
	doc, err := domain.GetBasicDgraphDocByUUID(ctx, docUUID, userUID)
	if err != nil {
		return err
	}
	if doc == nil {
		return ErrNotFound
	}
	if IsDeleted(doc) {
		return goneFor(CouldEdit(doc, userUID))
	}
	if !CanEdit(doc, userUID) {
		return errors.New("unauthorized: edit access required to restore")
	}
	body, err := ReadDocVersionBody(ctx, docUUID, snapshotID)
	if err != nil {
		return err
	}
	if _, err := ReplaceBodyLive(ctx, docUUID, body, collab.By{UserUUID: userUUID}, snapshotModels.ReasonManual); err != nil {
		helpers.LogErrorWithContext(ctx, "business/RestoreDocSnapshot live write failed err: %+v", err)
		if errors.Is(err, collab.ErrUnavailable) {
			return errors.New("the document can't be restored just now; try again in a moment")
		}
		if errors.Is(err, collab.ErrDocInTrash) {
			return ErrDocDeleted // deleted while the version was being read
		}
		return errors.New("could not restore the document")
	}
	return nil
}

// ReadDocVersionBody is one version's body (gzipped HTML in object storage).
func ReadDocVersionBody(ctx context.Context, docUUID string, snapshotID uuid.UUID) (string, error) {
	snap, err := snapshotModels.GetSnapshotByID(ctx, snapshotID, docUUID)
	if err != nil {
		return "", err
	}
	if snap == nil {
		return "", errors.New("snapshot not found")
	}
	gz, err := getDocSnapshotObject(ctx, snap.ObjectKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ReadDocVersionBody fetch blob failed err: %+v", err)
		return "", errors.New("could not load the snapshot")
	}
	raw, err := docGunzip(gz)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ReadDocVersionBody decompress failed err: %+v", err)
		return "", errors.New("the snapshot is corrupted")
	}
	body := string(raw)
	if strings.TrimSpace(body) == "" {
		return "", errors.New("the snapshot is empty")
	}
	return body, nil
}

// collabBodyHash is the hash a saved Yjs state is stored with: SHA-256 of the
// body's UTF-8 bytes, in hex, as the collaboration service computes it
// (docState.js bodyHash). The two must agree, or every open rebuilds.
func collabBodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// collabErrorBody is what collaboration services before v2.70.0 wrote over a
// whole doc when they could not write its HTML. Never a real body.
const collabErrorBody = "<p>Error generating HTML</p>"

// ErrUnsafeCollabBody refuses a save that would replace a doc with nothing, or
// with that placeholder.
var ErrUnsafeCollabBody = errors.New("refusing to replace the document with an empty or placeholder body")

// unsafeCollabBody reports whether a body from the collaboration service must
// not be stored: blank, or the old error placeholder.
func unsafeCollabBody(body string) bool {
	b := strings.TrimSpace(body)
	return b == "" || b == collabErrorBody
}

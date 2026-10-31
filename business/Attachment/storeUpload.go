package business

// The one way an upload becomes a stored object: a chat's or a channel's
// file, an imported page's picture, a file in the doc tree (docs F1), and a
// file a visitor sends with a public form. Each wrote this pipeline out by
// hand; now each asks StoreUpload and writes only its own rows.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// uploadKeyPrefix is where the user upload bucket keeps uploads: an
// attachments row's obj_key is the key under it.
const uploadKeyPrefix = "userFileUpload/"

// ObjectName is the name an attachments row's bytes are stored under in the
// user upload bucket: its obj_key, under uploadKeyPrefix. What reads or removes
// a stored upload by name (a form's picture's presigned link, the archive's
// purge) asks for this, never the bare key, so none can drift from the uploads.
func ObjectName(objKey string) string {
	return uploadKeyPrefix + objKey
}

// ErrNoStorage is an upload with no object storage to keep it in.
var ErrNoStorage = errors.New("attachment: object storage is not connected")

// Stored is an upload as it was kept.
type Stored struct {
	ID uuid.UUID
	// ObjKey is what an attachments row names: <uploader>/<id>_<safe name>.
	ObjKey      string
	SafeName    string
	ContentType string
	// Size is how many bytes were stored.
	Size int64
}

// StoreOptions are what an uploader asks of an upload: uploadsafe's options,
// and Accept, which refuses a type it won't keep (an imported page keeps only
// pictures) before anything is stored.
type StoreOptions struct {
	Safe   uploadsafe.Options
	Accept func(contentType string) error
	// Folder keeps the upload in another folder than its uploader's: a
	// visitor has no account, so their files wait in their form's
	// (forms/<form id>).
	Folder string
	// FailClosed keeps the upload only when the scanner says it is clean
	// (SafeUploadOptions.FailClosed): a stranger's file.
	FailClosed bool
}

// uploadObjectKey is an upload's obj_key: in folder, the uploader's id when
// it is "". Pure.
func uploadObjectKey(uploader, id uuid.UUID, folder, safeName string) string {
	if folder == "" {
		folder = uploader.String()
	}
	return fmt.Sprintf("%v/%v_%v", folder, id, safeName)
}

// StoreUpload checks an upload (uploadsafe.PrepareUpload: its real type
// sniffed, a dangerous one made harmless or refused, and a download forced
// except for the safe images opts allow inline), streams it into the user
// upload bucket under userFileUpload/<uploader>/<id>_<safe name> (or opts'
// Folder) with the AV scan beside it (SafeUploadToMinio), and removes it again
// when the scan finds a virus (an *avscan.Infected, naming it). size is the declared
// size, -1 for unknown. Its rows are the caller's to write; RemoveStored takes
// the bytes back when they can't be.
func StoreUpload(ctx context.Context, r io.Reader, size int64, name string, uploader uuid.UUID, opts StoreOptions) (Stored, error) {
	safe, err := uploadsafe.PrepareUpload(r, name, opts.Safe)
	if err != nil {
		return Stored{}, fmt.Errorf("upload validation: %w", err)
	}
	if opts.Accept != nil {
		if err := opts.Accept(safe.ContentType); err != nil {
			return Stored{}, err
		}
	}
	if minioInit.MinioClient == nil {
		return Stored{}, ErrNoStorage
	}
	id := uuid.New()
	objKey := uploadObjectKey(uploader, id, opts.Folder, safe.SafeName)
	bucket := helpers.UserUploadBucket()
	info, scan, err := SafeUploadToMinio(ctx, safe.Reader, SafeUploadOptions{
		Bucket:             bucket,
		ObjectName:         uploadKeyPrefix + objKey,
		ContentType:        safe.ContentType,
		ContentDisposition: safe.ContentDisposition,
		Size:               size,
		FailClosed:         opts.FailClosed,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/StoreUpload %s: %+v", safe.SafeName, err)
		return Stored{}, err
	}
	if scan.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.WithoutCancel(ctx), bucket, uploadKeyPrefix+objKey, minio.RemoveObjectOptions{})
		helpers.LogWarnWithContext(ctx, "business/StoreUpload rejected by AV: %s (%s)", safe.SafeName, scan.Signature)
		return Stored{}, fmt.Errorf("upload blocked: %w", &avscan.Infected{Signature: scan.Signature})
	}
	return Stored{ID: id, ObjKey: objKey, SafeName: safe.SafeName, ContentType: safe.ContentType, Size: info.Size}, nil
}

// What ReadStored refuses: a stored upload larger than its reader takes, one
// a picture's reader asked for that isn't a picture, and an empty one.
var (
	ErrStoredTooLarge = errors.New("attachment: the stored file is larger than its reader takes")
	ErrStoredNotImage = errors.New("attachment: the stored file isn't a picture")
	ErrStoredEmpty    = errors.New("attachment: the stored file is empty")
)

// ReadStored reads a stored upload's bytes and its stored content type, by
// the key its rows name (never one a client sends), up to limit bytes: a
// larger one is ErrStoredTooLarge, and is read no further. With images, one
// whose stored type isn't image/* is ErrStoredNotImage: uploadsafe stores
// SVG and the like as octet-stream, so only real pictures pass. The AI's
// readers of a picture or a document, and a board's export, read through it.
func ReadStored(ctx context.Context, objKey string, limit int64, images bool) ([]byte, string, error) {
	if minioInit.MinioClient == nil {
		return nil, "", ErrNoStorage
	}
	bucket, name := helpers.UserUploadBucket(), uploadKeyPrefix+objKey
	info, err := minioInit.MinioClient.StatObject(ctx, bucket, name, minio.StatObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("stat %s: %w", objKey, err)
	}
	mime := strings.ToLower(strings.TrimSpace(info.ContentType))
	if images && !strings.HasPrefix(mime, "image/") {
		return nil, "", ErrStoredNotImage
	}
	if info.Size > limit {
		return nil, "", ErrStoredTooLarge
	}
	obj, err := minioInit.MinioClient.GetObject(ctx, bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get %s: %w", objKey, err)
	}
	defer obj.Close()
	data, err := io.ReadAll(io.LimitReader(obj, limit+1))
	switch {
	case err != nil:
		return nil, "", fmt.Errorf("read %s: %w", objKey, err)
	case len(data) == 0:
		return nil, "", ErrStoredEmpty
	case int64(len(data)) > limit:
		return nil, "", ErrStoredTooLarge
	}
	return data, mime, nil
}

// RemoveStored removes a stored upload's bytes, by the key its rows name: for
// an upload whose rows couldn't be written. It runs even when ctx has ended,
// since a request that ended is a likely reason they couldn't.
func RemoveStored(ctx context.Context, objKey string) error {
	return RemoveStoredAll(context.WithoutCancel(ctx), []string{objKey})
}

// RemoveStoredAll removes stored uploads by their keys, in one request: the
// files nobody claimed, which a sweep clears. A key already gone is no error;
// the keys that couldn't be removed come back in a *RemoveError (the rest are
// gone), so a sweep forgets what went and moves past what keeps failing.
func RemoveStoredAll(ctx context.Context, objKeys []string) error {
	if len(objKeys) == 0 {
		return nil
	}
	if minioInit.MinioClient == nil {
		return ErrNoStorage
	}
	objects := make(chan minio.ObjectInfo, len(objKeys))
	for _, k := range objKeys {
		objects <- minio.ObjectInfo{Key: uploadKeyPrefix + k}
	}
	close(objects)
	var failed *RemoveError
	for res := range minioInit.MinioClient.RemoveObjects(ctx, helpers.UserUploadBucket(), objects, minio.RemoveObjectsOptions{}) {
		if res.Err == nil {
			continue
		}
		if failed == nil {
			failed = &RemoveError{First: fmt.Errorf("remove %s: %w", res.ObjectName, res.Err)}
		}
		failed.Failed = append(failed.Failed, strings.TrimPrefix(res.ObjectName, uploadKeyPrefix))
	}
	if failed != nil {
		return failed
	}
	return nil
}

// RemoveError is the keys RemoveStoredAll couldn't remove.
type RemoveError struct {
	Failed []string
	First  error
}

func (e *RemoveError) Error() string {
	return fmt.Sprintf("%d files left in storage: %v", len(e.Failed), e.First)
}
func (e *RemoveError) Unwrap() error { return e.First }

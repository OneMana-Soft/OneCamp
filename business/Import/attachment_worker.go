package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// Default per-file size cap (1 GB). Operators bump via env if needed.
const defaultMaxAttachmentBytes int64 = 1 << 30

// processAttachmentChunk downloads one attachment to MinIO and writes
// the attachments row + Dgraph edge + OpenSearch doc.
//
// Failure modes:
//   - 404/410 from provider                  → mark mapped to placeholder, no parent edge
//   - 429 (rate limit)                       → ResetChunkForRetry, wait, return errReaperOnly
//   - file > MaxFileBytes                    → mark done with placeholder + warning
//   - parent never imported                  → upload + log warning, no parent edge
//   - MinIO write failure                    → return err so chunk retries
//
// Provider-agnostic: all provider-specific knowledge (auth headers,
// signed-URL refresh) is encoded in SourceAttachment.Headers and the
// FetchAttachment override. By default we do an HTTP GET ourselves to
// keep things simple.
func processAttachmentChunk(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, chunk *importModels.Chunk,
	importingUser *userModels.UserInfo) error {

	if chunk.ObjectKey == nil || *chunk.ObjectKey == "" {
		return fmt.Errorf("attachment chunk missing object_key meta")
	}
	meta, err := parseAttachmentMeta(*chunk.ObjectKey)
	if err != nil {
		return fmt.Errorf("parse meta: %w", err)
	}

	maxBytes := defaultMaxAttachmentBytes
	if v, _ := opts["max_file_bytes"].(float64); v > 0 {
		maxBytes = int64(v)
	}

	// Idempotent: did we already create an attachment for this file?
	if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityFile, meta.FileID); existing != uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 1, nil)
	}

	if meta.Size > 0 && meta.Size > maxBytes {
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_TOO_LARGE",
			fmt.Sprintf("file %s size %d exceeds cap %d; skipping", meta.Name, meta.Size, maxBytes),
			mustMarshal(map[string]any{"name": meta.Name, "size": meta.Size, "url": meta.URL}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	// Resolve parent first so a fetch failure doesn't write an orphan.
	parentInfo, err := resolveAttachmentParent(ctx, job, chunk, meta)
	if err != nil {
		return fmt.Errorf("resolve parent: %w", err)
	}

	// Build the SourceAttachment for FetchAttachment.
	att := importProvider.SourceAttachment{
		SourceID: meta.FileID,
		Name:     meta.Name,
		URL:      meta.URL,
		Mime:     meta.Mime,
		Size:     meta.Size,
		Headers:  meta.Headers,
		Parent: importProvider.SourceRef{
			Kind:     parentInfo.Kind,
			SourceID: meta.ParentID,
		},
	}

	bucket := helpers.UserUploadBucket()
	attachmentUUID := uuid.New()
	sanitizedName := helpers.SanitizeUploadFileName(meta.Name)
	objKey := fmt.Sprintf("%s/%s_%s",
		importingUser.UserPostgresInfo.Id, attachmentUUID, sanitizedName)
	fullObjName := fmt.Sprintf("userFileUpload/%s", objKey)

	// Download via the provider's FetchAttachment so OAuth-bearer
	// providers can attach Authorization headers.
	// Stream the provider's bytes through an io.Pipe into MinIO. The
	// producer goroutine writes to pw; PutObject reads from pr. If
	// PutObject returns an error mid-stream we MUST drain pr so the
	// producer doesn't block on its next Write — otherwise the
	// goroutine leaks for the lifetime of the process.
	pr, pw := io.Pipe()
	var (
		fetchMime string
		fetchSize int64
		fetchErr  error
	)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		fetchMime, fetchSize, fetchErr = prov.FetchAttachment(ctx, job, opts, att, pw)
		// Always close (or close-with-error) so PutObject's read side
		// returns. Without this the orchestrator can hang forever if
		// the provider implementation forgets to close on the happy path.
		if fetchErr != nil {
			_ = pw.CloseWithError(fetchErr)
		} else {
			_ = pw.Close()
		}
	}()

	contentType := meta.Mime
	if contentType == "" {
		contentType = helpers.GuessContentTypeFromName(meta.Name)
	}
	// Coerce dangerous types and force attachment disposition. This
	// is the same defence applied on the Slack import path: a malicious
	// provider could serve text/html or image/svg+xml that browsers
	// render inline. uploadsafe.SafeContentType strips dangerous types
	// to application/octet-stream; BuildDisposition forces download.
	contentType = uploadsafe.SafeContentType(contentType, filepath.Ext(meta.Name))
	disposition := uploadsafe.BuildDisposition(sanitizedName)

	uploadInfo, scanResult, err := attachmentBusiness.SafeUploadToMinio(ctx,
		io.LimitReader(pr, maxBytes+1),
		attachmentBusiness.SafeUploadOptions{
			Bucket:             bucket,
			ObjectName:         fullObjName,
			ContentType:        contentType,
			ContentDisposition: disposition,
			Size:               -1,
		})

	if err != nil {
		// PutObject failed (network, MinIO error, ctx cancelled). The
		// producer might still be writing; close the read side with an
		// error so its Write returns immediately and the goroutine exits.
		_ = pr.CloseWithError(err)
	}
	<-doneCh
	if fetchErr != nil {
		// Clean up the partial upload (it may or may not exist).
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, fullObjName, minio.RemoveObjectOptions{})
		if errors.Is(fetchErr, importProvider.ErrAttachmentGone) {
			importModels.LogImportError(ctx, job.Id, &chunk.Id,
				importModels.EntityFile, meta.FileID,
				importModels.SeverityWarning, "FILE_GONE",
				"provider reported attachment gone (404/410); placeholder created",
				mustMarshal(map[string]any{"url": meta.URL}))
			ph := uuid.New()
			_ = importModels.UpsertIdMapping(ctx, job.Id, importModels.EntityFile,
				meta.FileID, ph, nil,
				mustMarshal(map[string]any{"placeholder": true, "reason": "gone"}))
			return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
		}
		if rl, isRL := importProvider.IsRateLimited(fetchErr); isRL {
			if rl > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(rl):
				}
			}
			_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
			return errReaperOnly
		}
		return fmt.Errorf("FetchAttachment: %w", fetchErr)
	}
	if err != nil {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, fullObjName, minio.RemoveObjectOptions{})
		return fmt.Errorf("minio put: %w", err)
	}

	// AV verdict handling. Infected → drop the file with a placeholder
	// row in id_map so retries don't re-fetch.
	if scanResult.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, fullObjName, minio.RemoveObjectOptions{})
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityError, "FILE_INFECTED",
			fmt.Sprintf("AV scan detected %s in %s; file dropped", scanResult.Signature, meta.Name),
			mustMarshal(map[string]any{
				"signature": scanResult.Signature, "name": meta.Name, "url": meta.URL,
			}))
		ph := uuid.New()
		_ = importModels.UpsertIdMapping(ctx, job.Id, importModels.EntityFile, meta.FileID,
			ph, nil, mustMarshal(map[string]any{
				"placeholder": true, "reason": "av_infected", "signature": scanResult.Signature,
			}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	if uploadInfo.Size > maxBytes {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, fullObjName, minio.RemoveObjectOptions{})
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_TOO_LARGE_STREAM",
			fmt.Sprintf("file %s exceeded cap during stream", meta.Name), nil)
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}
	_ = fetchSize // size from MinIO is authoritative
	if fetchMime != "" {
		contentType = fetchMime
	}

	// PG attachments row.
	if err := attachmentBusiness.CreateAttachment(ctx, attachmentUUID, objKey,
		importingUser.UserPostgresInfo.Id, parentInfo.SrcKey, parentInfo.SrcValue); err != nil {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, fullObjName, minio.RemoveObjectOptions{})
		return fmt.Errorf("create attachment row: %w", err)
	}

	// Dgraph attachment node + parent edge.
	createdAt := time.Now()
	dgAtt := &dgraphStruct.DgraphAttachment{
		Uuid:      attachmentUUID.String(),
		FileName:  meta.Name,
		ObjectKey: objKey,
		Type:      classifyAttachmentByName(meta.Name),
		RawType:   contentType,
		Size:      int(uploadInfo.Size),
		CreatedAt: &createdAt,
		CreatedBy: &dgraphStruct.DgraphUser{Uuid: importingUser.UserDgraphInfo.Uuid},
		DType:     []string{"Attachment"},
	}
	if _, err := attachmentDomain.BulkAddAttachmentsToDgraph(ctx,
		[]*dgraphStruct.DgraphAttachment{dgAtt}); err != nil {
		helpers.LogWarnWithContext(ctx,
			"Import attachment %s: dgraph node write failed: %+v", attachmentUUID, err)
	}

	// Parent edge + OpenSearch doc.
	if err := retrofitAttachmentToParent(ctx, parentInfo, dgAtt, importingUser); err != nil {
		helpers.LogWarnWithContext(ctx,
			"Import attachment %s: parent retrofit failed: %+v", attachmentUUID, err)
	}

	return importModels.UpsertIdMappingThenFinish(ctx, job.Id, chunk.Id,
		importModels.EntityFile, meta.FileID, attachmentUUID,
		mustMarshal(map[string]any{
			"name":        meta.Name,
			"obj_key":     objKey,
			"size":        uploadInfo.Size,
			"parent_kind": parentInfo.Kind,
			"parent_uuid": parentInfo.ParentUUID.String(),
		}))
}

// attachmentMeta is the parsed form of the chunk's object_key.
type attachmentMeta struct {
	FileID     string            `json:"file_id"`
	URL        string            `json:"url"`
	Name       string            `json:"name"`
	Size       int64             `json:"size"`
	Mime       string            `json:"mime"`
	ParentID   string            `json:"parent_id"`
	ParentKind string            `json:"parent_kind"`
	Headers    map[string]string `json:"headers,omitempty"`
}

func parseAttachmentMeta(s string) (*attachmentMeta, error) {
	out := &attachmentMeta{}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		return nil, err
	}
	if out.FileID == "" || out.URL == "" {
		return nil, fmt.Errorf("invalid attachment meta: missing file_id or url")
	}
	return out, nil
}

// attachmentParentInfo carries the parent context resolved from id_map.
type attachmentParentInfo struct {
	Kind       string // "task" | "comment" | "project"
	SrcKey     string
	SrcValue   string
	ParentUUID uuid.UUID
	// For OpenSearch denorm:
	TaskUUID    uuid.UUID
	ProjectUUID uuid.UUID
	ProjectName string
}

func resolveAttachmentParent(ctx context.Context, job *importModels.Job,
	chunk *importModels.Chunk, meta *attachmentMeta) (*attachmentParentInfo, error) {
	out := &attachmentParentInfo{Kind: meta.ParentKind}
	switch meta.ParentKind {
	case "task":
		// A task or a subtask: both have files.
		mapped := lookupMappedTask(ctx, job.Id, meta.ParentID)
		taskUUID := mapped.UUID
		if taskUUID == uuid.Nil {
			return out, fmt.Errorf("task %s not mapped", meta.ParentID)
		}
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_TASK
		out.SrcValue = taskUUID.String()
		out.ParentUUID = taskUUID
		out.TaskUUID = taskUUID
		// Resolve project for OpenSearch denorm.
		if src := mapped.ProjectSourceID; src != "" {
			if pid, _ := importModels.LookupIdMapping(ctx, job.Id,
				importModels.EntityProject, src); pid != uuid.Nil {
				out.ProjectUUID = pid
				if pmd, _ := importModels.GetIdMapMetadata(ctx, job.Id,
					importModels.EntityProject, src); len(pmd) > 0 {
					var pm struct {
						Name string `json:"name"`
					}
					_ = json.Unmarshal(pmd, &pm)
					out.ProjectName = pm.Name
				}
			}
		}
	case "comment":
		commentUUID, _ := importModels.LookupIdMapping(ctx, job.Id,
			importModels.EntityComment, meta.ParentID)
		if commentUUID == uuid.Nil {
			return out, fmt.Errorf("comment %s not mapped", meta.ParentID)
		}
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_COMMENT
		out.SrcValue = commentUUID.String()
		out.ParentUUID = commentUUID
		// Resolve task uuid through comment's parent_source_id.
		if md, _ := importModels.GetIdMapMetadata(ctx, job.Id,
			importModels.EntityComment, meta.ParentID); len(md) > 0 {
			var m struct {
				TaskSourceId string `json:"task_source_id"`
			}
			_ = json.Unmarshal(md, &m)
			if m.TaskSourceId != "" {
				if tid, _ := importModels.LookupIdMapping(ctx, job.Id,
					importModels.EntityTask, m.TaskSourceId); tid != uuid.Nil {
					out.TaskUUID = tid
				}
			}
		}
	case "project":
		projectUUID, _ := importModels.LookupIdMapping(ctx, job.Id,
			importModels.EntityProject, meta.ParentID)
		if projectUUID == uuid.Nil {
			return out, fmt.Errorf("project %s not mapped", meta.ParentID)
		}
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_PROJECT
		out.SrcValue = projectUUID.String()
		out.ParentUUID = projectUUID
		out.ProjectUUID = projectUUID
	default:
		return out, fmt.Errorf("unknown parent kind: %s", meta.ParentKind)
	}
	return out, nil
}

// retrofitAttachmentToParent stitches the imported attachment to its
// parent in Dgraph and writes the OpenSearch index entry. Best-effort:
// the file is in MinIO and PG already.
func retrofitAttachmentToParent(ctx context.Context, p *attachmentParentInfo,
	dgAtt *dgraphStruct.DgraphAttachment, importingUser *userModels.UserInfo) error {

	switch p.Kind {
	case "task":
		// Add the attachment edge from Task → Attachment via a per-uuid
		// upsert. We don't have a domain helper for this single edge,
		// so use a small Dgraph mutation through the existing
		// CreateOrUpdateDgraphTask.
		dgTask := &dgraphStruct.DgraphTask{
			Uid:         "uid(ta)",
			Uuid:        p.TaskUUID.String(),
			Attachments: []*dgraphStruct.DgraphAttachment{dgAtt},
		}
		_ = dgTask
		// Reuse CreateOrUpdateDgraphTask via the low-level helper that
		// supports query+upsert. We do this by constructing a DgraphTask
		// with the existing uuid and the new attachment, and calling
		// the canonical creator. The mutation appends to the @reverse
		// edge cleanly because edges in dgraph are sets.
		//
		// We use the task domain helper to keep the upsert query cached.
		if _, err := taskDomain.CreateOrUpdateDgraphTask(ctx, dgTask); err != nil {
			helpers.LogWarnWithContext(ctx,
				"retrofit task attachment dgraph: %+v", err)
		}

		osAtt := &openSearchStruct.OpenSearchAttachment{
			Uuid:                     dgAtt.Uuid,
			AttachmentByUserUuid:     importingUser.UserDgraphInfo.Uuid,
			AttachmentByUserFullName: importingUser.UserDgraphInfo.UserFullName,
			AttachmentFileName:       dgAtt.FileName,
			AttachmentObjKey:         dgAtt.ObjectKey,
			AttachmentTaskUuid:       p.TaskUUID.String(),
			AttachmentProjectUuid:    p.ProjectUUID.String(),
			AttachmentCreatedAt:      safeUnix(dgAtt.CreatedAt),
		}
		goSafe(ctx, "attachment_opensearch", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAtt) })

	case "comment":
		dgComment := &dgraphStruct.DgraphComment{
			Uid:         "uid(co)",
			Uuid:        p.ParentUUID.String(),
			Attachments: []*dgraphStruct.DgraphAttachment{dgAtt},
		}
		// Reuse the comment update helper which writes the comment node
		// and resets mentions; the attachment edge is appended cleanly.
		if err := commentDomain.UpdateDgraphCommentAndResetMentions(ctx, dgComment, p.ParentUUID.String()); err != nil {
			helpers.LogWarnWithContext(ctx,
				"retrofit comment attachment dgraph: %+v", err)
		}

		osAtt := &openSearchStruct.OpenSearchAttachment{
			Uuid:                     dgAtt.Uuid,
			AttachmentByUserUuid:     importingUser.UserDgraphInfo.Uuid,
			AttachmentByUserFullName: importingUser.UserDgraphInfo.UserFullName,
			AttachmentFileName:       dgAtt.FileName,
			AttachmentObjKey:         dgAtt.ObjectKey,
			AttachmentCommentUuid:    p.ParentUUID.String(),
			AttachmentTaskUuid:       p.TaskUUID.String(),
			AttachmentCreatedAt:      safeUnix(dgAtt.CreatedAt),
		}
		goSafe(ctx, "attachment_opensearch", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAtt) })

	case "project":
		osAtt := &openSearchStruct.OpenSearchAttachment{
			Uuid:                     dgAtt.Uuid,
			AttachmentByUserUuid:     importingUser.UserDgraphInfo.Uuid,
			AttachmentByUserFullName: importingUser.UserDgraphInfo.UserFullName,
			AttachmentFileName:       dgAtt.FileName,
			AttachmentObjKey:         dgAtt.ObjectKey,
			AttachmentProjectUuid:    p.ProjectUUID.String(),
			AttachmentCreatedAt:      safeUnix(dgAtt.CreatedAt),
		}
		goSafe(ctx, "attachment_opensearch", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAtt) })
	}
	return nil
}

// DefaultFetchAttachment was moved to the provider package
// (business/Import/provider/fetch.go) to avoid an import cycle: the
// per-provider implementations need to call it, but the provider
// package can't import this one. The legacy reference is left here as
// a doc breadcrumb.
//
// classifyAttachmentByName mirrors the slack file_worker logic.
func classifyAttachmentByName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image"
	case ".pdf", ".txt", ".doc", ".docx", ".xls", ".xlsx", ".csv":
		return "document"
	case ".mp3", ".wav", ".ogg", ".m4a":
		return "audio"
	case ".mp4", ".mov", ".webm", ".avi", ".mkv":
		return "video"
	default:
		return "other"
	}
}

func safeUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

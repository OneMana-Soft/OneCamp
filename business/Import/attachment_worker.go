package business

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
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

	// A file the import has recorded is kept already: a chunk runs again
	// only when its link or its finish didn't land, so it is linked again
	// from the record, never fetched and kept twice. One recorded as a
	// placeholder was never kept.
	rec, err := fileRecordOf(ctx, job.Id, meta.FileID)
	if err != nil {
		return err
	}
	if rec != nil && rec.Placeholder {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	if rec == nil && meta.Size > 0 && meta.Size > maxBytes {
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

	if rec == nil {
		if rec, err = keepFile(ctx, prov, job, opts, chunk, meta, parentInfo, importingUser, maxBytes); err != nil || rec == nil {
			return err
		}
	}

	dgAtt := keptFileNode(rec, time.Now(), importingUser.UserDgraphInfo.Uid)
	if err := linkKeptFile(ctx, parentInfo, dgAtt, importingUser); err != nil {
		return fmt.Errorf("link file %s: %w", rec.ID, err)
	}
	return importModels.FinishChunk(ctx, chunk.Id, 1, nil)
}

// keptFileNode is the graph node of a file an import kept, as its record
// describes it, kept at by maker: the maker's own graph node's uid, or ""
// when they have none, and then the node names no maker.
func keptFileNode(rec *fileRecord, at time.Time, maker string) *dgraphStruct.DgraphAttachment {
	node := &dgraphStruct.DgraphAttachment{
		Uuid:      rec.ID.String(),
		FileName:  rec.Name,
		ObjectKey: rec.ObjKey,
		Type:      attachmentBusiness.KindOf(rec.Name),
		RawType:   rec.ContentType,
		Size:      int(rec.Size),
		CreatedAt: &at,
		DType:     []string{"Attachment"},
	}
	if maker != "" {
		node.CreatedBy = &dgraphStruct.DgraphUser{Uid: maker}
	}
	return node
}

// fileRecord is the import's record of a source file (its id map row's
// metadata): one it kept, or a placeholder for one it couldn't.
type fileRecord struct {
	ID          uuid.UUID `json:"-"`
	Name        string    `json:"name,omitempty"`
	ObjKey      string    `json:"obj_key,omitempty"`
	Size        int64     `json:"size,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	ParentKind  string    `json:"parent_kind,omitempty"`
	ParentUUID  string    `json:"parent_uuid,omitempty"`
	Placeholder bool      `json:"placeholder,omitempty"`
}

// fileRecordOf is the import's record of the source file fileID: nil when
// it has none.
func fileRecordOf(ctx context.Context, importID uuid.UUID, fileID string) (*fileRecord, error) {
	id, err := importModels.LookupIdMapping(ctx, importID, importModels.EntityFile, fileID)
	if err != nil || id == uuid.Nil {
		return nil, err
	}
	raw, err := importModels.GetIdMapMetadata(ctx, importID, importModels.EntityFile, fileID)
	if err != nil {
		return nil, err
	}
	rec := &fileRecord{ID: id}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, rec); err != nil {
			return nil, fmt.Errorf("read file %s's record: %w", fileID, err)
		}
	}
	return rec, nil
}

// keepFile fetches the source's file and keeps it as a file of its place
// (storeFile), then records it before anything links it, so a chunk that
// runs again finds it kept. It answers nil, with the chunk finished, for a
// file it doesn't keep: gone, empty or infected (a placeholder is recorded),
// or past the cap.
func keepFile(ctx context.Context, prov importProvider.Provider, job *importModels.Job, opts importProvider.JobOptions,
	chunk *importModels.Chunk, meta *attachmentMeta, parentInfo *attachmentParentInfo, importingUser *userModels.UserInfo,
	maxBytes int64) (*fileRecord, error) {
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
	stored, err := storeFile(ctx, prov, job, opts, att, parentInfo.SrcKey, parentInfo.SrcValue, importingUser.UserPostgresInfo.Id, maxBytes)
	if err != nil {
		if rl, isRL := importProvider.IsRateLimited(err); isRL {
			// Napped, not slept out: a worker asleep past five minutes has
			// its claim on the chunk taken for a stuck one.
			waitOutRateLimit(ctx, job, err, rl)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
			return nil, errReaperOnly
		}
		return nil, fmt.Errorf("FetchAttachment: %w", err)
	}
	switch stored.Outcome {
	case fileGone, fileEmpty:
		code, said := "FILE_GONE", "provider reported attachment gone (404/410); placeholder created"
		if stored.Outcome == fileEmpty {
			code, said = "FILE_EMPTY", fmt.Sprintf("%s is empty, so there's nothing to keep; placeholder created", meta.Name)
		}
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, code, said,
			mustMarshal(map[string]any{"url": meta.URL}))
		if err := importModels.UpsertIdMapping(ctx, job.Id, importModels.EntityFile,
			meta.FileID, uuid.New(), nil,
			mustMarshal(map[string]any{"placeholder": true, "reason": stored.Outcome})); err != nil {
			return nil, err
		}
		return nil, importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	case fileInfected:
		// Infected: dropped, with a placeholder in the id map so retries
		// don't fetch it again.
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityError, "FILE_INFECTED",
			fmt.Sprintf("AV scan detected %s in %s; file dropped", stored.Signature, meta.Name),
			mustMarshal(map[string]any{
				"signature": stored.Signature, "name": meta.Name, "url": meta.URL,
			}))
		if err := importModels.UpsertIdMapping(ctx, job.Id, importModels.EntityFile, meta.FileID,
			uuid.New(), nil, mustMarshal(map[string]any{
				"placeholder": true, "reason": "av_infected", "signature": stored.Signature,
			})); err != nil {
			return nil, err
		}
		return nil, importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	case fileTooLarge:
		importModels.LogImportError(ctx, job.Id, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_TOO_LARGE_STREAM",
			fmt.Sprintf("file %s exceeded cap during stream", meta.Name), nil)
		return nil, importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}
	rec := &fileRecord{ID: stored.ID, Name: meta.Name, ObjKey: stored.ObjKey, Size: stored.Size, ContentType: stored.ContentType,
		ParentKind: parentInfo.Kind, ParentUUID: parentInfo.ParentUUID.String()}
	if err := importModels.UpsertIdMapping(ctx, job.Id, importModels.EntityFile, meta.FileID, rec.ID, nil, mustMarshal(rec)); err != nil {
		return nil, err
	}
	return rec, nil
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

// linkFile is the one link of a file to its task or comment in the graph. A
// seam for tests.
var linkFile = attachmentDomain.LinkFile

// writeFileNode writes a project's kept file's node alone, by its uuid; a seam
// for tests.
var writeFileNode = attachmentDomain.WriteFileNode

// linkKeptFile puts a kept file in the graph as its place shows it: linked
// under its task or comment (linkFile, which writes the file's node too), or,
// a project's, the node alone; then writes its search entry. Each graph write
// can run again, and one that fails is answered, so the chunk runs again.
func linkKeptFile(ctx context.Context, p *attachmentParentInfo,
	dgAtt *dgraphStruct.DgraphAttachment, importingUser *userModels.UserInfo) error {
	osAtt := &openSearchStruct.OpenSearchAttachment{
		Uuid:                     dgAtt.Uuid,
		AttachmentByUserUuid:     importingUser.UserDgraphInfo.Uuid,
		AttachmentByUserFullName: importingUser.UserDgraphInfo.DisplayName(),
		AttachmentFileName:       dgAtt.FileName,
		AttachmentObjKey:         dgAtt.ObjectKey,
		AttachmentCreatedAt:      safeUnix(dgAtt.CreatedAt),
	}
	switch p.Kind {
	case "task":
		if err := linkFile(ctx, "task", p.TaskUUID.String(), dgAtt); err != nil {
			return err
		}
		osAtt.AttachmentTaskUuid, osAtt.AttachmentProjectUuid = p.TaskUUID.String(), p.ProjectUUID.String()
	case "comment":
		if err := linkFile(ctx, "comment", p.ParentUUID.String(), dgAtt); err != nil {
			return err
		}
		osAtt.AttachmentCommentUuid, osAtt.AttachmentTaskUuid = p.ParentUUID.String(), p.TaskUUID.String()
	case "project":
		if err := writeFileNode(ctx, dgAtt); err != nil {
			return err
		}
		osAtt.AttachmentProjectUuid = p.ProjectUUID.String()
	}
	goSafe(ctx, "attachment_opensearch", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAtt) })
	return nil
}

// DefaultFetchAttachment was moved to the provider package
// (business/Import/provider/fetch.go) to avoid an import cycle: the
// per-provider implementations need to call it, but the provider
// package can't import this one. The legacy reference is left here as
// a doc breadcrumb.
//

func safeUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

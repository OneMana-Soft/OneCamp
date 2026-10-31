package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// processTaskCommentChunk imports every comment for a single task. The
// chunk's parent_source_id is the task's source id; we look it up in
// the id_map to get the OneCamp task UUID.
//
// System comments (Asana stories, Jira changelog) are skipped by default
// because they're noisy. Operators opt-in via the
// `keep_system_comments` job option.
//
// Heartbeats every 100 comments.
func processTaskCommentChunk(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, chunk *importModels.Chunk,
	importingUser *userModels.UserInfo) error {

	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("comment chunk missing task source id")
	}
	taskSrcId := *chunk.ParentSourceId

	// A task or a subtask: both have comments.
	taskUUID := lookupMappedTask(ctx, job.Id, taskSrcId).UUID
	if taskUUID == uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}
	dgTask, err := taskDomain.GetDgraphBasicTaskInfoByUUID(ctx,
		taskUUID.String(), importingUser.UserDgraphInfo.Uid)
	if err != nil || dgTask == nil {
		return fmt.Errorf("dgraph task: %w", err)
	}

	keepSystem, _ := opts["keep_system_comments"].(bool)

	who := newPeople(job)
	commentCh, errCh := prov.IterCommentsOfTask(ctx, job, opts, taskSrcId)
	committed := 0
	var kept []string                    // why, for each comment its source keeps from some people
	var notes []importModels.ImportError // the task's report lines, written with its chunks
	pendingChunks := make([]*importModels.Chunk, 0, 8)

	flush := func() {
		if len(pendingChunks) == 0 && len(notes) == 0 {
			return
		}
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		importModels.LogImportErrors(bg, job.Id, notes)
		if len(pendingChunks) > 0 {
			_ = importModels.CreateChunks(bg, pendingChunks)
		}
		pendingChunks, notes = pendingChunks[:0], notes[:0]
	}
	defer flush()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				if d, ok := importProvider.IsRateLimited(err); ok {
					waitOutRateLimit(ctx, job, err, d)
					_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
					return errReaperOnly
				}
				return err
			}
			errCh = nil
		case sc, ok := <-commentCh:
			if !ok {
				notes = append(notes, commentsKeptNote(job, &chunk.Id, taskSrcId, kept)...)
				// A source whose task files come apart from the task lists
				// them here, with the task's comments (TaskFileLister).
				if lister, isLister := prov.(importProvider.TaskFileLister); isLister {
					refs, lerr := lister.ListTaskFiles(ctx, job, opts, taskSrcId)
					if lerr != nil {
						importModels.LogImportError(ctx, job.Id, &chunk.Id,
							importModels.EntityTask, taskSrcId,
							importModels.SeverityWarning, "TASK_FILES_UNLISTED", lerr.Error(), nil)
					}
					pendingChunks = append(pendingChunks, fileChunks(job, taskSrcId, "task", refs)...)
				}
				return importModels.FinishChunk(ctx, chunk.Id, committed, nil)
			}
			// Kept from some of the people who see its task, it stays in its
			// source: here everyone who sees the task would read it.
			if sc.Restricted != "" {
				kept = append(kept, sc.Restricted)
				continue
			}
			if sc.IsSystem && !keepSystem {
				continue
			}
			if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
				importModels.EntityComment, sc.SourceID); existing != uuid.Nil {
				continue
			}
			if existing, _ := importModels.LookupWorkspaceMapping(ctx,
				job.Provider, job.SourceWorkspaceName,
				importModels.EntityComment, sc.SourceID); existing != uuid.Nil {
				if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName,
					importModels.EntityComment, sc.SourceID, existing, &taskSrcId,
					mustMarshal(map[string]any{"matched_by": "workspace_map"}), false); err != nil {
					importModels.LogImportError(ctx, job.Id, &chunk.Id,
						importModels.EntityComment, sc.SourceID,
						importModels.SeverityError, "COMMENT_IMPORT_FAILED",
						err.Error(), nil)
				}
				continue
			}

			author := importingUser
			if sc.AuthorSourceID != "" {
				if _, info := who.resolve(ctx, sc.AuthorSourceID); info != nil {
					author = info
				}
			}

			body := strings.TrimSpace(sc.Body)
			if body == "" && len(sc.AttachmentRefs) == 0 {
				// Empty comment with no attachments: skip entirely.
				// Earlier versions wrote a synthetic id_map entry with a
				// fresh UUID; that polluted the map (later attachment
				// chunks that resolved this id would point at a row that
				// never existed in `comments`). Just skipping is correct
				// because re-running the import is a no-op for empty
				// comments anyway.
				continue
			}
			createdAt := sc.Created
			if createdAt.IsZero() {
				createdAt = time.Now()
			}

			commentUUID, err := writeTaskComment(ctx, dgTask, author, body, createdAt)
			if err != nil {
				importModels.LogImportError(ctx, job.Id, &chunk.Id,
					importModels.EntityComment, sc.SourceID,
					importModels.SeverityError, "COMMENT_IMPORT_FAILED",
					err.Error(), nil)
				continue
			}

			if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName,
				importModels.EntityComment, sc.SourceID, commentUUID, &taskSrcId,
				mustMarshal(map[string]any{
					"task_source_id": taskSrcId,
					"author":         sc.AuthorSourceID,
				}), true); err != nil {
				importModels.LogImportError(ctx, job.Id, &chunk.Id,
					importModels.EntityComment, sc.SourceID,
					importModels.SeverityError, "COMMENT_IMPORT_FAILED",
					err.Error(), nil)
				continue
			}

			pendingChunks = append(pendingChunks, fileChunks(job, sc.SourceID, "comment", sc.AttachmentRefs)...)

			committed++
			if committed%100 == 0 {
				flush()
				_ = importModels.HeartbeatChunk(ctx, chunk.Id, committed, nil)
			}
		}
	}
}

// writeTaskComment is the import-side equivalent of
// taskBusiness.CreateTaskComment minus AI/MQTT/webhook fan-out. It
// persists the comment to PG → Dgraph → OpenSearch with the source's
// created_at preserved.
//
// Dgraph upsert pattern follows the canonical CreatePostComment flow:
// build a DgraphTask containing the embedded Comment node, then call
// commentDomain.CreateDgraphCommentInATask which runs the upsert query.
func writeTaskComment(ctx context.Context, dgTask *dgraphStruct.DgraphTask,
	author *userModels.UserInfo, body string, createdAt time.Time) (uuid.UUID, error) {

	commentUUID := uuid.New()
	zeroUnixTime := time.Time{}

	// 1. Postgres, with its source time as both its times (CreateComment):
	// the import set them in an UPDATE afterwards.
	if err := commentDomain.CreateComment(ctx, commentUUID,
		author.UserPostgresInfo.Id, createdAt); err != nil {
		return uuid.Nil, fmt.Errorf("pg create comment: %w", err)
	}

	// 2. Dgraph: task → embedded comment in one upsert.
	embeddedTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(ta)",
		Uuid: dgTask.Uuid,
		Comments: []*dgraphStruct.DgraphComment{{
			DType: []string{"Comment"},
			Uid:   "uid(co)",
			Uuid:  commentUUID.String(),
			Text:  body,
			Task: &dgraphStruct.DgraphTask{
				Uid: "uid(ta)",
			},
			CommentBy: &dgraphStruct.DgraphUser{
				Uid: author.UserDgraphInfo.Uid,
			},
			CreatedAt: &createdAt,
			UpdatedAt: &createdAt,
			DeletedAt: &zeroUnixTime,
		}},
	}
	if _, err := commentDomain.CreateDgraphCommentInATask(ctx, embeddedTask, commentUUID.String()); err != nil {
		return uuid.Nil, fmt.Errorf("dgraph comment: %w", err)
	}

	// 3. OpenSearch (async, best-effort), the entry a person's comment gets.
	osComment := commentDomain.TaskCommentInSearch(dgTask, commentUUID.String(), &author.UserDgraphInfo, body, createdAt)
	goSafe(ctx, "comment_opensearch", func() {
		commentDomain.CreateTaskCommentWithAttachmentsInOpenSearch(osComment, nil)
	})

	return commentUUID, nil
}

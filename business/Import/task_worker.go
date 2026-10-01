package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// processTaskChunk imports every top-level task in a single project.
// Subtasks are deferred to processSubtaskChunk because they need the
// parent's id_map entry to exist.
//
// For each task we:
//  1. Skip if already in id_map (resume safety).
//  2. Re-use OneCamp UUID from workspace map if a prior import has it.
//  3. Resolve assignee via id_map (best effort; falls back to creator).
//  4. Render description HTML with co-assignees as a leading line.
//  5. Apply status / priority mappings (operator-confirmed → defaults).
//  6. Persist task in PG → Dgraph → OpenSearch with the source created_at.
//  7. Schedule task_comments + per-attachment chunks for this task.
//
// Heartbeat every 25 tasks so the reaper doesn't bump attempts for a
// long-running per-project chunk.
func processTaskChunk(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, chunk *importModels.Chunk,
	importingUser *userModels.UserInfo) error {

	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("task chunk missing project source id")
	}
	projectSrcId := *chunk.ParentSourceId

	projectUUID, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityProject, projectSrcId)
	if projectUUID == uuid.Nil {
		return fmt.Errorf("project not mapped: %s", projectSrcId)
	}
	projectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx,
		projectUUID.String(), importingUser.UserDgraphInfo.Uid)
	if err != nil || projectInfo == nil {
		return fmt.Errorf("dgraph project: %w", err)
	}

	statusMap, _ := importModels.GetStatusMappings(ctx, job.Id)
	priorityMap, _ := importModels.GetPriorityMappings(ctx, job.Id)
	defaultStatus := prov.DefaultStatusMap()
	defaultPriority := prov.DefaultPriorityMap()

	taskCh, errCh := prov.IterTasksOfProject(ctx, job, opts, projectSrcId)

	committed := 0
	pendingChunks := make([]*importModels.Chunk, 0, 64)

	flush := func() {
		if len(pendingChunks) == 0 {
			return
		}
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := importModels.CreateChunks(bg, pendingChunks); err != nil {
			helpers.LogWarnWithContext(ctx,
				"task_worker.flush chunks insert failed batch=%d err=%+v",
				len(pendingChunks), err)
		}
		pendingChunks = pendingChunks[:0]
	}
	defer flush()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				if d, ok := importProvider.IsRateLimited(err); ok {
					sleepUntilRetryAfter(ctx, d)
					_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
					return errReaperOnly
				}
				return err
			}
			errCh = nil
		case st, ok := <-taskCh:
			if !ok {
				flush()
				cursor := ""
				return importModels.FinishChunk(ctx, chunk.Id, committed, &cursor)
			}
			if st.ParentTaskID == "" {
				if err := importOneTask(ctx, job, &st, projectUUID, projectInfo,
					statusMap, priorityMap, defaultStatus, defaultPriority,
					importingUser, &pendingChunks); err != nil {
					importModels.LogImportError(ctx, job.Id, &chunk.Id,
						importModels.EntityTask, st.SourceID,
						importModels.SeverityError, "TASK_IMPORT_FAILED",
						err.Error(), nil)
					continue
				}
				committed++
				if committed%25 == 0 {
					flush()
					_ = importModels.HeartbeatChunk(ctx, chunk.Id, committed, nil)
				}
			} else {
				// Schedule a subtasks chunk for the parent. Idempotent
				// on the chunks unique index so duplicate scheduling
				// (parent has many subtasks) collapses to one row.
				pendingChunks = append(pendingChunks, &importModels.Chunk{
					Id:             uuid.New(),
					ImportId:       job.Id,
					ChunkType:      importModels.ChunkTaskSubtasks,
					ParentSourceId: strPtr(st.ParentTaskID),
					Status:         importModels.ChunkStatusPending,
					MaxAttempts:    5,
				})
			}
		}
	}
}

// importOneTask creates exactly one task in OneCamp and records its
// id_map entry. Schedules a comments chunk if hint > 0 and an
// attachments chunk per attachment ref.
func importOneTask(ctx context.Context, job *importModels.Job,
	st *importProvider.SourceTask,
	projectUUID uuid.UUID, projectInfo *dgraphStruct.DgraphProject,
	statusMap, priorityMap, defaultStatus, defaultPriority map[string]string,
	importingUser *userModels.UserInfo, pendingChunks *[]*importModels.Chunk,
) error {
	if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityTask, st.SourceID); existing != uuid.Nil {
		return nil
	}
	if existing, _ := importModels.LookupWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTask, st.SourceID); existing != uuid.Nil {
		_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
			importModels.EntityTask, st.SourceID, existing, nil,
			mustMarshal(map[string]any{"matched_by": "workspace_map"}), false)
		return nil
	}

	creator := importingUser
	if st.CreatedBy != "" {
		if _, info := resolveUserUUID(ctx, job, st.CreatedBy); info != nil {
			creator = info
		}
	}

	var assigneeDg *dgraphStruct.DgraphUser
	extraAssignees := []string{}
	if len(st.AssigneeIds) > 0 {
		_, info := resolveUserUUID(ctx, job, st.AssigneeIds[0])
		if info != nil {
			a := info.UserDgraphInfo
			assigneeDg = &a
		}
		for _, ex := range st.AssigneeIds[1:] {
			if _, exInfo := resolveUserUUID(ctx, job, ex); exInfo != nil {
				extraAssignees = append(extraAssignees, exInfo.UserDgraphInfo.UserFullName)
			}
		}
	}

	finalStatus := importProvider.ApplyStatusMap(st.Status, statusMap, defaultStatus)
	finalPriority := importProvider.ApplyPriorityMap(st.Priority, priorityMap, defaultPriority)

	descHTML := renderTaskDescription(st, extraAssignees)
	label := ""
	if len(st.Labels) > 0 {
		label = st.Labels[0]
	}

	createdAt := st.Created
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	updatedAt := st.Updated
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	taskUUID, err := writeTask(ctx, job, importTaskInput{
		ProjectUUID: projectUUID,
		Project:     projectInfo,
		Creator:     creator,
		Assignee:    assigneeDg,
		Name:        truncate(st.Name, 256),
		Description: descHTML,
		Status:      finalStatus,
		Priority:    finalPriority,
		Label:       label,
		StartDate:   st.StartDate,
		DueDate:     st.DueDate,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	})
	if err != nil {
		return err
	}

	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
		importModels.EntityTask, st.SourceID, taskUUID, nil,
		mustMarshal(map[string]any{
			"name":              st.Name,
			"project_source_id": st.ProjectSourceID,
			"author":            st.CreatedBy,
			"final_status":      finalStatus,
			"final_priority":    finalPriority,
		}), true); err != nil {
		return err
	}
	_ = importModels.UpsertWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityTask, st.SourceID, taskUUID, job.Id)

	if st.CommentCount > 0 {
		*pendingChunks = append(*pendingChunks, &importModels.Chunk{
			Id:             uuid.New(),
			ImportId:       job.Id,
			ChunkType:      importModels.ChunkTaskComments,
			ParentSourceId: strPtr(st.SourceID),
			Status:         importModels.ChunkStatusPending,
			ItemsTotal:     intPtr(st.CommentCount),
			MaxAttempts:    5,
		})
	}
	for _, ar := range st.AttachmentRefs {
		ck := encodeAttachmentMeta(ar, st.SourceID, "task")
		if ck == "" {
			continue
		}
		*pendingChunks = append(*pendingChunks, &importModels.Chunk{
			Id:             uuid.New(),
			ImportId:       job.Id,
			ChunkType:      importModels.ChunkAttachment,
			ParentSourceId: strPtr(st.SourceID),
			ObjectKey:      &ck,
			Status:         importModels.ChunkStatusPending,
			MaxAttempts:    5,
		})
	}
	return nil
}

// processSubtaskChunk creates each subtask under its parent. Implemented
// as a flat task plus a "parent_task" edge so we don't re-implement
// CreateSubTask's full dgraph mutation. The edge is wired via
// CreateOrUpdateDgraphTask with task_parent_task set.
func processSubtaskChunk(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions, chunk *importModels.Chunk,
	importingUser *userModels.UserInfo) error {

	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("subtask chunk missing parent source id")
	}
	parentSrc := *chunk.ParentSourceId

	parentUUID, _ := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityTask, parentSrc)
	if parentUUID == uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	parentMeta, _ := importModels.GetIdMapMetadata(ctx, job.Id,
		importModels.EntityTask, parentSrc)
	parentProjectSrc := ""
	if len(parentMeta) > 0 {
		var m struct {
			ProjectSourceId string `json:"project_source_id"`
		}
		_ = json.Unmarshal(parentMeta, &m)
		parentProjectSrc = m.ProjectSourceId
	}
	projectUUID := uuid.Nil
	if parentProjectSrc != "" {
		projectUUID, _ = importModels.LookupIdMapping(ctx, job.Id,
			importModels.EntityProject, parentProjectSrc)
	}
	if projectUUID == uuid.Nil {
		return fmt.Errorf("subtask parent's project not mapped: %s", parentProjectSrc)
	}
	projectInfo, err := projectBusiness.GetBasicDgraphProjectInfo(ctx,
		projectUUID.String(), importingUser.UserDgraphInfo.Uid)
	if err != nil || projectInfo == nil {
		return fmt.Errorf("dgraph project: %w", err)
	}
	parentDgraphTask, err := taskDomain.GetDgraphBasicTaskInfoByUUID(ctx,
		parentUUID.String(), importingUser.UserDgraphInfo.Uid)
	if err != nil || parentDgraphTask == nil {
		return fmt.Errorf("dgraph parent task: %w", err)
	}

	statusMap, _ := importModels.GetStatusMappings(ctx, job.Id)
	priorityMap, _ := importModels.GetPriorityMappings(ctx, job.Id)
	defaultStatus := prov.DefaultStatusMap()
	defaultPriority := prov.DefaultPriorityMap()

	subCh, errCh := prov.IterSubtasksOfTask(ctx, job, opts, parentSrc)
	committed := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				if d, ok := importProvider.IsRateLimited(err); ok {
					sleepUntilRetryAfter(ctx, d)
					_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
					return errReaperOnly
				}
				return err
			}
			errCh = nil
		case st, ok := <-subCh:
			if !ok {
				return importModels.FinishChunk(ctx, chunk.Id, committed, nil)
			}
			if existing, _ := importModels.LookupIdMapping(ctx, job.Id,
				importModels.EntitySubtask, st.SourceID); existing != uuid.Nil {
				continue
			}
			creator := importingUser
			if st.CreatedBy != "" {
				if _, info := resolveUserUUID(ctx, job, st.CreatedBy); info != nil {
					creator = info
				}
			}
			var assigneeDg *dgraphStruct.DgraphUser
			if len(st.AssigneeIds) > 0 {
				if _, info := resolveUserUUID(ctx, job, st.AssigneeIds[0]); info != nil {
					a := info.UserDgraphInfo
					assigneeDg = &a
				}
			}
			finalStatus := importProvider.ApplyStatusMap(st.Status, statusMap, defaultStatus)
			finalPriority := importProvider.ApplyPriorityMap(st.Priority, priorityMap, defaultPriority)
			label := ""
			if len(st.Labels) > 0 {
				label = st.Labels[0]
			}
			createdAt := st.Created
			if createdAt.IsZero() {
				createdAt = time.Now()
			}
			subUUID, err := writeTask(ctx, job, importTaskInput{
				ProjectUUID: projectUUID,
				Project:     projectInfo,
				ParentTask:  parentDgraphTask,
				Creator:     creator,
				Assignee:    assigneeDg,
				Name:        truncate(st.Name, 256),
				Description: renderTaskDescription(&st, nil),
				Status:      finalStatus,
				Priority:    finalPriority,
				Label:       label,
				StartDate:   st.StartDate,
				DueDate:     st.DueDate,
				CreatedAt:   createdAt,
				UpdatedAt:   ifZeroNow(st.Updated),
			})
			if err != nil {
				importModels.LogImportError(ctx, job.Id, &chunk.Id,
					importModels.EntitySubtask, st.SourceID,
					importModels.SeverityError, "SUBTASK_IMPORT_FAILED",
					err.Error(), nil)
				continue
			}
			_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
				importModels.EntitySubtask, st.SourceID, subUUID, &parentSrc,
				mustMarshal(map[string]any{
					"parent_task_uuid": parentUUID.String(),
					"name":             st.Name,
				}), true)
			_ = importModels.UpsertWorkspaceMapping(ctx,
				job.Provider, job.SourceWorkspaceName,
				importModels.EntitySubtask, st.SourceID, subUUID, job.Id)
			committed++
		}
	}
}

// renderTaskDescription returns final HTML, listing extra assignees on
// a leading "Co-assignees:" line so the OneCamp task page surfaces them
// despite the single-assignee data model.
func renderTaskDescription(st *importProvider.SourceTask, extraAssignees []string) string {
	body := strings.TrimSpace(st.Description)
	prefix := ""
	if len(extraAssignees) > 0 {
		prefix = "<p><em>Co-assignees: " + strings.Join(extraAssignees, ", ") + "</em></p>"
	}
	if body == "" {
		return prefix
	}
	return prefix + body
}

// sleepUntilRetryAfter blocks for d (capped) before returning, or
// returns early on context cancel. Use d=0 for "no hint, sleep a
// short default" — providers that don't know their reset window
// pass 0 and we still let the chunk cool down rather than re-claiming
// instantly via the reaper.
//
// Cap of 2 minutes prevents a misbehaving provider that returns a
// huge Retry-After (or one with a clock skew bug) from stalling a
// worker indefinitely; the chunk reaper will eventually pick it back
// up regardless.
func sleepUntilRetryAfter(ctx context.Context, d time.Duration) {
	if d <= 0 {
		d = 5 * time.Second
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func intPtr(i int) *int { return &i }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ifZeroNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// encodeAttachmentMeta serialises the attachment ref into the chunk's
// object_key column as JSON. Same encoding shared with the legacy
// Slack file_worker so the attachment_worker can read either.
func encodeAttachmentMeta(ar importProvider.SourceAttachment, parentSrcId, parentKind string) string {
	headersBytes, _ := json.Marshal(ar.Headers)
	b, err := json.Marshal(map[string]any{
		"file_id":     ar.SourceID,
		"url":         ar.URL,
		"name":        ar.Name,
		"size":        ar.Size,
		"mime":        ar.Mime,
		"parent_id":   parentSrcId,
		"parent_kind": parentKind,
		"headers":     json.RawMessage(headersBytes),
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// ─── writeTask: PG → Dgraph → OpenSearch in one place ─────────────────

type importTaskInput struct {
	ProjectUUID uuid.UUID
	Project     *dgraphStruct.DgraphProject
	ParentTask  *dgraphStruct.DgraphTask // nil for top-level
	Creator     *userModels.UserInfo
	Assignee    *dgraphStruct.DgraphUser
	Name        string
	Description string
	Status      string
	Priority    string
	Label       string
	StartDate   *time.Time
	DueDate     *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// writeTask is the import-side equivalent of taskBusiness.CreateTask
// that respects the source's created_at and skips the AI/calendar/MQTT
// fan-out we don't want during a bulk import.
//
// Side-effects intentionally NOT performed (vs CreateTask):
//   - ai.EmbedTaskContent (re-embed once at finalize, not per-task)
//   - integrationBusiness.SyncTaskToGoogleCalendar (no historical sync)
//   - mqtt task_create publish (suppressed via BulkImportContextKey
//     anyway, but we don't even invoke the path)
//
// Stays consistent with native tasks via:
//   - Same PG insert shape (domain.CreateTask)
//   - Same Dgraph node shape (domain.CreateOrUpdateDgraphTask)
//   - Same OpenSearch doc shape (CreateTaskWithAttachmentsInOpensearch)
//
// Returns the new task UUID.
func writeTask(ctx context.Context, job *importModels.Job, in importTaskInput) (uuid.UUID, error) {
	taskUUID := uuid.New()
	zeroUnixTime := time.Time{}

	// 1. Postgres. domain.CreateTask sets both created_at and
	// updated_at to in.CreatedAt (the SQL is "VALUES ($1,$2,$3,$4,$4)")
	// so we don't need a follow-up UPDATE for the common case where
	// the source has a single timestamp. Only when updated_at differs
	// (the source had a real edit history) do we patch it.
	if err := taskDomain.CreateTask(ctx, taskUUID, in.ProjectUUID,
		in.Creator.UserPostgresInfo.Id, in.CreatedAt); err != nil {
		return uuid.Nil, fmt.Errorf("pg create task: %w", err)
	}
	if !in.UpdatedAt.IsZero() && !in.UpdatedAt.Equal(in.CreatedAt) {
		if _, err := importModels.Exec(ctx,
			`UPDATE tasks SET updated_at = $2 WHERE id = $1`,
			taskUUID, in.UpdatedAt); err != nil {
			helpers.LogWarnWithContext(ctx,
				"writeTask backdate updated_at failed: %+v", err)
		}
	}

	// 2. Dgraph node.
	startDate := &zeroUnixTime
	if in.StartDate != nil && !in.StartDate.IsZero() {
		startDate = in.StartDate
	}
	dueDate := &zeroUnixTime
	if in.DueDate != nil && !in.DueDate.IsZero() {
		dueDate = in.DueDate
	}
	createdAt := in.CreatedAt
	updatedAt := in.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	dgraphTask := &dgraphStruct.DgraphTask{
		Uid:  "uid(task)",
		Uuid: taskUUID.String(),
		Project: &dgraphStruct.DgraphProject{
			Uid: in.Project.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		},
		Team: &dgraphStruct.DgraphTeam{
			Uid: in.Project.Team.Uid,
		},
		StartDate:   startDate,
		DueDate:     dueDate,
		Name:        in.Name,
		Description: &in.Description,
		Status:      in.Status,
		Priority:    in.Priority,
		Label:       &in.Label,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: in.Creator.UserDgraphInfo.Uid,
		},
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
		DeletedAt: &zeroUnixTime,
		DType:     []string{"Task"},
	}
	if in.ParentTask != nil {
		dgraphTask.ParentTask = &dgraphStruct.DgraphTask{
			Uid: in.ParentTask.Uid,
			SubTasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		}
	}
	if in.Assignee != nil {
		dgraphTask.Assignee = &dgraphStruct.DgraphUser{
			Uid: in.Assignee.Uid,
			Tasks: []*dgraphStruct.DgraphTask{{
				Uid: "uid(task)",
			}},
		}
	}
	if _, err := taskDomain.CreateOrUpdateDgraphTask(ctx, dgraphTask); err != nil {
		return uuid.Nil, fmt.Errorf("dgraph task: %w", err)
	}

	// 3. OpenSearch (async, best-effort).
	emptyString := ""
	osTask := &openSearchStruct.OpenSearchTask{
		Uuid:                  taskUUID.String(),
		TaskName:              in.Name,
		TaskDescription:       &in.Description,
		TaskAssigneeUuid:      &emptyString,
		TaskCreatedAt:         createdAt.Unix(),
		TaskDeletedAt:         nil,
		TaskProjectUuid:       in.Project.Uuid,
		TaskProjectName:       in.Project.Name,
		TaskStatus:            in.Status,
		TaskPriority:          in.Priority,
		TaskLabel:             &in.Label,
		TaskCreatedByUserUuid: in.Creator.UserDgraphInfo.Uuid,
	}
	if in.Assignee != nil {
		osTask.TaskAssigneeUuid = &in.Assignee.Uuid
		osTask.TaskAssigneeFullName = in.Assignee.UserFullName
	}
	goSafe(ctx, "task_opensearch", func() {
		taskDomain.CreateTaskWithAttachmentsInOpensearch(osTask, nil, &in.Creator.UserDgraphInfo)
	})

	return taskUUID, nil
}

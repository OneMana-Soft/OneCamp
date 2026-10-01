package Domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Attachment"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Attachment"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	models "github.com/akashc777/OneCamp/models/postgres/Attachment"
	"github.com/google/uuid"
)

//CREATE TABLE IF NOT EXISTS attachment(
//"id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
//"project_id" uuid REFERENCES teams(id),
//"obj_key" varchar NOT NULL,
//"created_by" uuid REFERENCES users(id),
//"created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"deleted_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
//);

func CreateAttachment(ctx context.Context, attachmentUUID uuid.UUID, objKey string, createdByUUID uuid.UUID, srcKey string, srcValue string) (err error) {

	query := `
		INSERT INTO attachments (id, obj_key, src_key, src_value, created_by)
		VALUES ($1, $2, $3, $4, $5)
	`
	err = models.CreateAttachment(query, attachmentUUID, objKey, srcKey, srcValue, createdByUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateProject Failed to create new attachment err: %+v",
			err)
		return
	}
	return
}

func CreateAttachmentForChannelsAndChats(ctx context.Context, attachmentUUID uuid.UUID, objKey string, createdByUUID uuid.UUID, channelUUIDs []string, chatUUIDs []string) (err error) {
	if len(channelUUIDs) == 0 && len(chatUUIDs) == 0 {
		return nil // No attachments to create
	}

	query := `INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES `
	var values []interface{}
	var placeholders []string
	paramIndex := 1

	for _, channelUUID := range channelUUIDs {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", paramIndex, paramIndex+1, paramIndex+2, paramIndex+3, paramIndex+4))
		values = append(values, attachmentUUID, objKey, postgressStruct.ATTACHMENT_SRC_CHANNEL, channelUUID, createdByUUID)
		paramIndex += 5
	}

	for _, chatUUID := range chatUUIDs {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", paramIndex, paramIndex+1, paramIndex+2, paramIndex+3, paramIndex+4))
		values = append(values, attachmentUUID, objKey, postgressStruct.ATTACHMENT_SRC_CHAT, chatUUID, createdByUUID)
		paramIndex += 5
	}

	query += strings.Join(placeholders, ", ")

	err = models.CreateAttachmentForChannelsAndChats(query, values...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateAttachmentForChannelsAndChats Failed to create new attachment err: %+v",
			err)
		return err
	}

	return nil
}

func UpdateAttachmentInOpenSearch(openSearchAttachment *openSearchStruct.OpenSearchAttachment) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateAttachmentInOpenSearch(ctx, openSearchAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateAttachmentInOpenSearch Failed to update attachment in open search err: %+v",
			err)
		return
	}
	return
}

func CreateAttachmentInOpenSearch(ctx context.Context, openSearchAttachment *openSearchStruct.OpenSearchAttachment) {
	err := OpenSearchModels.CreateAttachmentInOpenSearch(ctx, openSearchAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateAttachmentInOpenSearch Failed to update attachment in open search err: %+v",
			err)
		return
	}
	return
}

//func CheckIfProjectExistByProjectNameAndTeamID(projectName string, teamUUID uuid.UUID) (exist bool, err error) {
//	query := `
//        SELECT EXISTS (
//            SELECT 1
//            FROM projects
//            WHERE project_name = $1 AND team_id = $2
//        );
//    `
//
//	exist, err = models.CheckIfProjectExistByProjectNameAndTeamUUID(query, projectName, teamUUID)
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"domain/CheckIfProjectExistByProjectNameAndTeamID Failed to check project by project name and teamID err: %+v",
//			err)
//		return
//	}
//
//	return
//}

func GetAttachmentByUUID(ctx context.Context, attachmentUUID uuid.UUID) (attachmentInfo *models.Attachment, err error) {

	query := `
        SELECT id, project_id, obj_key, src_key, src_value, created_by, created_at, deleted_at
        FROM attachments
        WHERE id = $1
    `

	attachmentInfo, err = models.GetAttachmentByUUID(query, attachmentUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAttachmentByUUID Failed to get attachment by uuid err: %+v",
			err)
		return
	}

	return
}

//func UpdateAttachmentByAttachmentUUID(attachmentUUID uuid.UUID, currentTime time.Time) (err error) {
//	query := `
//        UPDATE projects
//        SET project_name = $1, updated_at = $2
//		WHERE id = $3`
//
//	err = models.UpdateProjectNameByProjectUUID(query, newProjectName, currentTime, projectUUID)
//
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"domain/UpdateTeamNameByTeamUUID Failed to update project name by project uuid err: %+v",
//			err)
//		return
//	}
//
//	return
//}

func UpdateAttachmentDeletedTimeByUUID(ctx context.Context, attachmentUUID uuid.UUID, deleteTime *time.Time) (err error) {
	query := `
        UPDATE attachments
        SET deleted_at = $1
		WHERE id = $2`
	err = models.UpdateAttachmentDeletedTimeByUUID(query, deleteTime, attachmentUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateAttachmentDeletedTimeByUUID Failed to update attachment delete time by attachment uuid err: %+v",
			err)
		return
	}

	return
}

func UpdateAttachmentDeletedTimeToNullByUUID(ctx context.Context, attachmentUUID uuid.UUID, updatedTime *time.Time) (err error) {
	query := `
        UPDATE attachments
        SET updated_at = $1, deleted_at = null
		WHERE id = $2`
	err = models.UpdateAttachmentDeletedTimeToNullByUUID(query, updatedTime, attachmentUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateAttachmentDeletedTimeToNullByUUID Failed to attachment delete time to null err: %+v",
			err)
		return
	}
	return
}

func GetAttachmentByObjUUID(ctx context.Context, objUUID string, srcKey string) (attachment *models.Attachment, err error) {
	query := `
		SELECT id, obj_key, src_key, src_value, created_by, created_at, deleted_at
		FROM attachments
		WHERE id = $1
		AND src_key = $2
	`
	attachment, err = models.GetAttachmentByObjUUID(query, objUUID, srcKey)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAttachmentByObjUUID Failed to get attachment from postgres err: %+v",
			err)
		return
	}
	return
}

func GetAttachmentsByObjUUIDs(ctx context.Context, objUUIDS []string) (attachments []*models.Attachment, err error) {
	query := `
		SELECT id, obj_key, created_by, created_at, deleted_at
		FROM attachments
		WHERE id = ANY($1)
		AND deleted_at IS NULL
		ORDER BY array_position($1, id::text)
	`
	attachments, err = models.GetAttachmentsByObjUUIDs(query, objUUIDS)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAttachmentsByObjUUIDs Failed to attachments list for postgres err: %+v",
			err)
		return
	}

	return
}

func CreateOrUpdateDgraphAttachment(ctx context.Context, dgraphAttachment *dgraphStruct.DgraphAttachment) (attachmentUid string, err error) {
	dgraphAttachment.DType = []string{"Attachment"}
	query := fmt.Sprintf(`query {
									  attachment as var(func: eq(attachment_uuid, "%+v"))
								  }`, dgraphAttachment.Uuid)

	attachmentUid, err = dgraphModels.CreateOrUpdateDgraphAttachment(ctx, dgraphAttachment, query)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphAttachment Failed to create/update attachment err: %+v",
			err)
		return
	}
	return
}

func GetDgraphAttachmentInfoByUUID(ctx context.Context, attachmentUUID string, userDgraph string) (dgraphAttachment *dgraphStruct.DgraphAttachment, err error) {

	variables := make(map[string]string)
	variables["$id"] = attachmentUUID
	variables["$userUid"] = userDgraph
	query := `query AttachmentInfo($id: string){
				attachmentInfo(func: eq(attachment_uuid, $id)) {
					uid
					attachment_uuid
					attachment_obj_key
					attachment_file_name
					attachment_project {
						project_is_member: count(project_members @filter(uid($userUid)))
						project_is_admin: count(project_admins @filter(uid($userUid)))
					}
					attachment_task {
						uid
					}
					
				}
			}`

	dgraphAttachment, err = dgraphModels.GetDgraphAttachmentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphAttachmentInfoByUUID Failed to get attachment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func BulkAddAttachmentsToDgraph(ctx context.Context, dgraphAttachments []*dgraphStruct.DgraphAttachment) (dgraphAttachmentUUID []string, err error) {

	for i, _ := range dgraphAttachments {
		dgraphAttachments[i].DType = []string{"Attachment"}
	}

	dgraphAttachmentUUID, err = dgraphModels.BulkAddAttachmentsToDgraph(ctx, dgraphAttachments)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/BulkAddAttachmentsToDgraph Failed to add attachments in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// BulkArchiveAttachmentsInDgraph sets attachment_deleted_at on multiple attachments in a single Dgraph mutation (batched).
func BulkArchiveAttachmentsInDgraph(ctx context.Context, attachmentUUIDs []string) error {
	if len(attachmentUUIDs) == 0 {
		return nil
	}
	now := time.Now()
	const batchSize = 500
	for start := 0; start < len(attachmentUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(attachmentUUIDs) {
			end = len(attachmentUUIDs)
		}
		batch := attachmentUUIDs[start:end]
		query := "query {\n"
		attachments := make([]*dgraphStruct.DgraphAttachment, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  at%d as var(func: eq(attachment_uuid, \"%s\"))\n", i, uuidStr)
			attachments[i] = &dgraphStruct.DgraphAttachment{
				DType:     []string{"Attachment"},
				Uid:       fmt.Sprintf("uid(at%d)", i),
				DeletedAt: &now,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphAttachments(ctx, attachments, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkArchiveAttachmentsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// BulkRestoreAttachmentsInDgraph clears attachment_deleted_at on multiple attachments in a single Dgraph mutation (batched).
func BulkRestoreAttachmentsInDgraph(ctx context.Context, attachmentUUIDs []string) error {
	if len(attachmentUUIDs) == 0 {
		return nil
	}
	zeroTime := time.Time{}
	const batchSize = 500
	for start := 0; start < len(attachmentUUIDs); start += batchSize {
		end := start + batchSize
		if end > len(attachmentUUIDs) {
			end = len(attachmentUUIDs)
		}
		batch := attachmentUUIDs[start:end]
		query := "query {\n"
		attachments := make([]*dgraphStruct.DgraphAttachment, len(batch))
		for i, uuidStr := range batch {
			query += fmt.Sprintf("  at%d as var(func: eq(attachment_uuid, \"%s\"))\n", i, uuidStr)
			attachments[i] = &dgraphStruct.DgraphAttachment{
				DType:     []string{"Attachment"},
				Uid:       fmt.Sprintf("uid(at%d)", i),
				DeletedAt: &zeroTime,
			}
		}
		query += "}"
		err := dgraphModels.BulkSoftDeleteDgraphAttachments(ctx, attachments, query)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/BulkRestoreAttachmentsInDgraph failed for batch %d-%d: %v", start, end, err)
			return err
		}
	}
	return nil
}

// UpsertAttachmentInOpenSearchSafe adds a single OpenSearchAttachment
// to the attachments index. Mirrors the inline logic in
// CreatePostWithAttachmentsInOpenSearch / CreateChatWithAttachmentsInOpenSearch
// but exposed as a stand-alone helper so the Slack-import retrofit pass
// can index a single file without going through the parent's full
// create-with-attachments pipeline.
//
// Side-effect-only: errors are logged, not returned, because callers
// fire-and-forget (the canonical attachment row in Postgres + Dgraph is
// already written; OpenSearch drift is reconcilable).
func UpsertAttachmentInOpenSearchSafe(att *openSearchStruct.OpenSearchAttachment) {
	if att == nil || att.Uuid == "" {
		return
	}
	ctx := context.Background()
	bulk := fmt.Sprintf(`{ "create" : { "_index" : "%s", "_id" : "%s" } }`+"\n", openSearchStruct.ATTACHMENT_INDEX, att.Uuid)
	body, err := json.Marshal(att)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpsertAttachmentInOpenSearchSafe marshal failed att=%s err=%+v",
			att.Uuid, err)
		return
	}
	bulk += string(body) + "\n"

	if err := OpenSearchBulkModels.BulkCreateInOpenSearch(bulk); err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpsertAttachmentInOpenSearchSafe bulk create failed att=%s err=%+v",
			att.Uuid, err)
	}
}

package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Comment"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchBulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/Comment"
	models "github.com/akashc777/OneCamp/models/postgres/Comment"
	"github.com/google/uuid"
)

func CreateComment(ctx context.Context, commentUUID uuid.UUID, userUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
		INSERT INTO comments (id, created_by, created_at)
		VALUES ($1, $2, $3)
	`
	err = models.CreateComment(query, commentUUID, userUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateComment Failed to create new comment err: %+v",
			err)
		return
	}
	return
}

// UpsertComment inserts a comment row, or updates updated_at if the id already
// exists. Used by surfaces (e.g. board comments) that re-sync the same comment
// id on edits, so the create/update path is idempotent and never errors on a
// duplicate primary key.
func UpsertComment(ctx context.Context, commentUUID uuid.UUID, userUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
		INSERT INTO comments (id, created_by, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET updated_at = $3
	`
	err = models.CreateComment(query, commentUUID, userUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpsertComment Failed to upsert comment err: %+v",
			err)
		return
	}
	return
}

func UpdateCommentByUUID(ctx context.Context, commentUUID uuid.UUID, currentTime time.Time) (err error) {
	query := `
		UPDATE comments
        SET updated_at = $1
		WHERE id = $2
	`
	err = models.UpdateCommentByUUID(query, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateCommentByUUID Failed to update comment err: %+v",
			err)
		return
	}
	return
}

func GetCommentByUUID(ctx context.Context, commentUUID uuid.UUID) (commentInfo *models.Comment, err error) {
	query := `
		SELECT id, created_by, created_at, updated_at, deleted_at
        FROM comments
        WHERE id = $1
	`
	commentInfo, err = models.GetCommentByUUID(query, commentUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetCommentByUUID Failed to get comment err: %+v",
			err)
		return
	}
	return
}

func SoftDeletePostByUUIUD(ctx context.Context, commentUUID uuid.UUID, current time.Time) (err error) {
	query := `
		UPDATE comments
        SET deleted_at = $1
		WHERE id = $2
	`
	err = models.UpdateCommentByUUID(query, commentUUID, current)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/SoftDeletePostByUUIUD Failed to delete comment err: %+v",
			err)
		return
	}
	return
}

func HardDeleteCommentByUUID(ctx context.Context, commentUUID uuid.UUID) (err error) {
	query := `
		DELETE FROM comments
        WHERE id = $1
	`
	err = models.HardDeleteCommentByUUID(query, commentUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteCommentByUUID Failed to delete comment err: %+v",
			err)
		return
	}
	return
}

func CreateDgraphCommentInADoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc, commentUUID string) (commentUid string, err error) {

	query := fmt.Sprintf(`query {
									  doc as var(func: eq(doc_uuid, "%+v"))
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphDoc.Uuid, commentUUID)

	commentUid, err = dgraphModels.CreateOrUpdateCommentInDoc(ctx, query, dgraphDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDgraphCommentInADoc Failed to create comment in doc err: %+v",
			err)
		return
	}
	return
}

func CreateDgraphCommentInAPost(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost, commentUUID string) (commentUid string, err error) {

	query := fmt.Sprintf(`query {
									  po as var(func: eq(post_uuid, "%+v"))
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphPost.Uuid, commentUUID)

	commentUid, err = dgraphModels.CreateOrUpdateCommentInPost(ctx, query, dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateCommentInAPost Failed to create comment in post err: %+v",
			err)
		return
	}
	return
}

func CreateDgraphCommentInATask(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask, commentUUID string) (commentUid string, err error) {

	query := fmt.Sprintf(`query {
									  ta as var(func: eq(task_uuid, "%+v"))
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphTask.Uuid, commentUUID)

	commentUid, err = dgraphModels.CreateOrUpdateCommentInTask(ctx, query, dgraphTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDgraphCommentInATask Failed to create comment in post err: %+v dgraph: %+v",
			err, dgraphTask)
		return
	}
	return
}

func CreateOrUpdateCommentInChat(ctx context.Context, dgraphChat *dgraphStruct.DgraphChat, commentUUID string) (commentUid string, err error) {

	query := fmt.Sprintf(`query {
									  cha as var(func: eq(chat_uuid, "%+v"))
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphChat.Uuid, commentUUID)

	commentUid, err = dgraphModels.CreateOrUpdateCommentInChat(ctx, query, dgraphChat)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateCommentInChat Failed to create/update comment in chat err: %+v",
			err)
		return
	}
	return
}

func UpdateDgraphCommentAndResetMentions(ctx context.Context, dgraphPost *dgraphStruct.DgraphComment, commentUUID string) (err error) {

	query := fmt.Sprintf(`query {
									  co as var(func: eq(comment_uuid, "%+v"))
									  me as var(func: eq(mention_comment_id, "%+v"))
								  }`, commentUUID, commentUUID)

	delStringJSON := `{
		"uid": "uid(me)",
		"mention_users": null
	}`
	err = dgraphModels.UpdateComment(ctx, query, dgraphPost, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateComment Failed to update comment err: %+v",
			err)
		return
	}
	return
}

func CreateOrUpdateDgraphCommentReaction(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment) (reactionUid string, err error) {

	query := fmt.Sprintf(`query {
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphComment.Uuid)

	reactionUid, err = dgraphModels.CreateOrUpdateCommentReaction(ctx, query, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphPostCommentReaction Failed to create/update comment reaction in post err: %+v",
			err)
		return
	}
	return
}

func GetDgraphPostCommentsInfoByUUID(ctx context.Context, postUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	variables := make(map[string]string)
	variables["$id"] = postUUID

	query := `query PostInfo($id: string){
				postInfo(func: eq(post_uuid, $id)) {
					post_uuid
					post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
						comment_by {
							user_uuid
						}
					}
				}
			}`

	dgraphPost, err = dgraphModels.GetDgraphPostCommentsInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPostCommentsInfoByUUID Failed to get post from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphTaskCommentUUIDsInfo(ctx context.Context, taskUUID string) (dgraphTask *dgraphStruct.DgraphTask, err error) {

	variables := make(map[string]string)
	variables["$id"] = taskUUID
	query := `query TaskInfo($id: string){
				taskInfo(func: eq(task_uuid, $id)) {
					task_uuid
					task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
						comment_by {
							user_uuid
						}
					}
					
				}
			}`

	dgraphTask, err = dgraphModels.GetDgraphTaskCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphTaskCommentInfo Failed to get task in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphTaskCommentInfoByUUID(ctx context.Context, commentUUID string, userDraphUid string) (dgraphComment *dgraphStruct.DgraphComment, err error) {

	variables := make(map[string]string)
	variables["$id"] = commentUUID
	variables["$userId"] = userDraphUid
	query := `query CommentInfo($id: string, $userId: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_by {
						uid
						user_uuid
					}
					comment_task {
						task_uuid
						task_project {
							project_uuid
							project_name
							project_is_member: count(project_members @filter(uid($userId)))
							project_is_admin: count(project_admins @filter(uid($userId))) 
							project_deleted_at
						}
						task_deleted_at
					}
				}
			}`

	dgraphComment, err = dgraphModels.GetDgraphCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphDocCommentInfoByUUID(ctx context.Context, commentUUID string, userDraphUid string) (dgraphComment *dgraphStruct.DgraphComment, err error) {

	variables := make(map[string]string)
	variables["$id"] = commentUUID
	variables["$userId"] = userDraphUid
	query := `query CommentInfo($id: string, $userId: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_by {
						uid
						user_uuid
					}
					comment_doc {
						doc_uuid
						doc_read_access: count(doc_reading_users @filter(uid($userId)))
						doc_edit_access: count(doc_editing_users @filter(uid($userId)))
						doc_comment_access: count(doc_commenting_users @filter(uid($userId)))
						doc_private
						doc_deleted_at
						doc_public_comment
						doc_title
						doc_created_by {
							user_uuid
						}
					}
				}
			}`

	dgraphComment, err = dgraphModels.GetDgraphCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDocCommentInfoByUUID Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPostCommentInfoByUUID(ctx context.Context, commentUUID string, userDraphUid string) (dgraphComment *dgraphStruct.DgraphComment, err error) {

	variables := make(map[string]string)
	variables["$id"] = commentUUID
	variables["$userId"] = userDraphUid
	query := `query CommentInfo($id: string, $userId: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_by {
						uid
						user_uuid
					}
					comment_post {
						post_uuid
						post_channel {
							uid
							ch_uuid
							ch_is_member: count(ch_members @filter(uid($userId)))
							ch_is_admin: count(ch_moderators @filter(uid($userId)))
							ch_deleted_at
						}
						post_by {
							id
							user_uuid
						}
					}
				}
			}`

	dgraphComment, err = dgraphModels.GetDgraphCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphChatCommentInfoByUUID(ctx context.Context, commentUUID string, userDgraphUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {

	variables := make(map[string]string)
	variables["$id"] = commentUUID
	variables["$userId"] = userDgraphUID

	query := `query CommentInfo($id: string, $userId: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_by {
						uid
						 user_uuid
					}
					comment_chat_grouping_id
					comment_chat {
						 chat_uuid
						chat_from {
							uid
							 user_uuid
							user_profile_object_key
							user_deleted_at
						}
						chat_to {
							uid
							 user_uuid
							user_profile_object_key
							user_deleted_at
						}
						chat_dm {
							dm_grouping_id
							dm_is_member: count(dm_participants @filter(uid($userId)))
						}
					}
				}
			}`

	dgraphComment, err = dgraphModels.GetDgraphCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphChatCommentInfoByUUID Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphCommentInfoByUUID(ctx context.Context, commentUUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {

	variables := make(map[string]string)
	variables["$id"] = commentUUID
	query := `query CommentInfo($id: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_by {
						uid
						 user_uuid
					}
					comment_post {
						 post_uuid
						post_channel {
							uid
							 ch_uuid
							ch_deleted_at
						}
						post_by {
							id
							 user_uuid
						}
					}
					comment_chat {
						 chat_uuid
						chat_from {
							uid
							 user_uuid
							user_profile_object_key
							user_deleted_at
						}
						chat_to {
							uid
							 user_uuid
							user_profile_object_key
							user_deleted_at
						}
					}
					
					comment_task {
						 task_uuid
						task_project {
							 project_uuid
						}
					}
				}
			}`

	dgraphComment, err = dgraphModels.GetDgraphCommentInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get comment in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphProjectMemberInfoForComment(ctx context.Context, projectUUID string) (dgraphProject *dgraphStruct.DgraphProject, err error) {
	variables := make(map[string]string)
	variables["$id"] = projectUUID
	query := `query ProjectInfo($id: string, $userUid: string){
				projectInfo(func: eq(project_uuid, $id)) {
					project_uuid
					project_name
					project_members (orderasc: user_name){
						uid
						user_uuid
						user_name
						user_email
						user_profile_object_key
					}
				}
			}`

	dgraphProject, err = dgraphModels.GetDgraphProjectMembersByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphProjectMemberInfoForComment Failed to get project info from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func UpdateDgraphCommentInAPost(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment) (err error) {

	query := fmt.Sprintf(`query {
									  co as var(func: eq(comment_uuid, "%+v"))
								  }`, dgraphComment.Uuid)

	err = dgraphModels.UpdateComment(ctx, query, dgraphComment, ``)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDgraphCommentInAPost Failed to update comment in post err: %+v",
			err)
		return
	}
	return
}

func DeleteCommentReaction(ctx context.Context, commentDgraphUID string, reactionDgraphUID string) (err error) {
	delStringJSON := fmt.Sprintf(`
		[
			
			{
				"uid": "%s",
				"comment_reactions": [
					{
						"uid": "%s"
					}
				]
			},
			{
				"uid": "%s",
				"reaction_added_by": null,
				"reaction_on_content_added_by": null,
					"reaction_added_at": null,
					"reaction_emoji_id": null
			
			},
			{
				"uid": "%s"
			}
		]
	`, commentDgraphUID, reactionDgraphUID, reactionDgraphUID, reactionDgraphUID)

	err = dgraphModels.DeleteReactionNodeOrEdges(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteCommentReaction Failed to remove channel member in dgraph err: %+v",
			err,
		)
		return
	}
	return
}

func CreateCommentInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment) (err error) {
	ctx := context.Background()
	err = OpenSearchModels.CreateCommentInOpenSearch(ctx, openSearchComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateCommentInOpenSearch Failed to insert to openSearch err: %+v",
			err,
		)
		return
	}

	return
}

func UpdateCommentInOpenSearch(openSearchComment openSearchStruct.OpenSearchComment) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateCommentInOpenSearch(ctx, &openSearchComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateCommentInOpenSearch Failed to update in openSearch err: %+v",
			err,
		)
		return
	}
}

func getCommentWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchComment *openSearchStruct.OpenSearchComment, openSearchAttachments []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"comments\", \"_id\" : \"%+v\" } }\n", openSearchComment.Uuid)
	postJsonData, err := json.Marshal(openSearchComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getCommentWithAttachmentBulkOperationStringForOpenSearch Error mashiling comment struct to json err: %+v",
			err)
		return
	}

	postCreate += string(postJsonData) + "\n"

	bulkActionString += postCreate

	attachmentCreate := ""

	for _, attachment := range openSearchAttachments {
		tempAttachmentCreate := fmt.Sprintf("{ \"create\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentJsonData, jsonErr := json.Marshal(attachment)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getPostWithAttachmentString Error mashiling attachment struct to json err: %+v",
				jsonErr)
			err = jsonErr
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func CreateChatCommentWithAttachmentsInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                           attachment.Uuid,
			AttachmentFileName:             attachment.FileName,
			AttachmentByUserUuid:           openSearchComment.CommentByUserUuid,
			AttachmentByProfile:            openSearchComment.CommentByProfile,
			AttachmentByUserFullName:       openSearchComment.CommentByUserFullName,
			AttachmentObjKey:               attachment.ObjectKey,
			AttachmentChatGrpId:            openSearchComment.CommentChatGrpId,
			AttachmentChatParticipants:     openSearchComment.CommentChatParticipants,
			AttachmentChatFromUserUuid:     openSearchComment.CommentChatFromUserUuid,
			AttachmentChatFromUserFullName: openSearchComment.CommentChatFromUserFullName,
			AttachmentChatUuid:             openSearchComment.CommentChatUuid,
			AttachmentCreatedAt:            openSearchComment.CommentCreatedAt,
			AttachmentDeletedAt:            nil,
		})
	}

	bulkOperationString, err := getCommentWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchComment, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateChatCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's with attachments err: %+v",
			err)
		return
	}
}
func CreateTaskCommentWithAttachmentsInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                     attachment.Uuid,
			AttachmentFileName:       attachment.FileName,
			AttachmentByUserUuid:     openSearchComment.CommentByUserUuid,
			AttachmentByProfile:      openSearchComment.CommentByProfile,
			AttachmentByUserFullName: openSearchComment.CommentByUserFullName,
			AttachmentObjKey:         attachment.ObjectKey,
			AttachmentProjectUuid:    openSearchComment.CommentProjectUuid,
			AttachmentProjectName:    openSearchComment.CommentProjectName,
			AttachmentTaskUuid:       openSearchComment.CommentTaskUuid,
			AttachmentCreatedAt:      openSearchComment.CommentCreatedAt,
			AttachmentDeletedAt:      nil,
		})
	}

	bulkOperationString, err := getCommentWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchComment, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTaskCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateTaskCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's with attachments err: %+v",
			err)
		return
	}
}

func CreatePostCommentWithAttachmentsInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                     attachment.Uuid,
			AttachmentFileName:       attachment.FileName,
			AttachmentByUserUuid:     openSearchComment.CommentByUserUuid,
			AttachmentByProfile:      openSearchComment.CommentByProfile,
			AttachmentByUserFullName: openSearchComment.CommentByUserFullName,
			AttachmentObjKey:         attachment.ObjectKey,
			AttachmentChannelUuid:    openSearchComment.CommentChannelUuid,
			AttachmentChannelName:    openSearchComment.CommentChannelName,
			AttachmentPostUuid:       openSearchComment.CommentPostUuid,
			AttachmentCreatedAt:      openSearchComment.CommentCreatedAt,
			AttachmentDeletedAt:      nil,
		})
	}

	bulkOperationString, err := getCommentWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchComment, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreatePostCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's with attachments err: %+v",
			err)
		return
	}
}

func CreateDocCommentWithAttachmentsInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchAttachments []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchAttachments = append(openSearchAttachments, &openSearchStruct.OpenSearchAttachment{
			Uuid:                           attachment.Uuid,
			AttachmentFileName:             attachment.FileName,
			AttachmentByUserUuid:           openSearchComment.CommentByUserUuid,
			AttachmentByProfile:            openSearchComment.CommentByProfile,
			AttachmentByUserFullName:       openSearchComment.CommentByUserFullName,
			AttachmentObjKey:               attachment.ObjectKey,
			AttachmentDocUuid:              openSearchComment.CommentDocUuid,
			AttachmentDocTitle:             openSearchComment.CommentDocTitle,
			AttachmentDocPrivate:           openSearchComment.CommentDocPrivate,
			AttachmentDocReadingUsers:      openSearchComment.CommentDocReadingUsers,
			AttachmentDocEditingUsers:      openSearchComment.CommentDocEditingUsers,
			AttachmentDocCommentingUsers:   openSearchComment.CommentDocCommentingUsers,
			AttachmentDocCreatedByUserUuid: openSearchComment.CommentDocCreatedByUserUuid,
			AttachmentCreatedAt:            openSearchComment.CommentCreatedAt,
			AttachmentDeletedAt:            nil,
		})
	}

	bulkOperationString, err := getCommentWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchComment, openSearchAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDocCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDocCommentWithAttachmentsInOpenSearch Error getting bulk string for comment's with attachments err: %+v",
			err)
		return
	}
}

func getDeleteCommentWithAttachmentBulkOperationStringForOpenSearch(ctx context.Context, openSearchComment *openSearchStruct.OpenSearchComment, openSearchAttachment []*openSearchStruct.OpenSearchAttachment) (bulkActionString string, err error) {

	postUpdate := fmt.Sprintf("{ \"update\": { \"_index\": \"comments\", \"_id\": \"%+v\" } }\n", openSearchComment.Uuid)
	postUpdateDoc := &openSearchStruct.BulkUpdate{
		Doc: openSearchComment,
	}
	postJsonData, err := json.Marshal(postUpdateDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/getDeleteCommentWithAttachmentBulkOperationStringForOpenSearch Error mashiling post struct to json err: %+v",
			err)
		return
	}

	postUpdate += string(postJsonData) + "\n"

	bulkActionString += postUpdate

	attachmentCreate := ""

	for _, attachment := range openSearchAttachment {
		tempAttachmentCreate := fmt.Sprintf("{ \"update\" : { \"_index\" : \"attachments\", \"_id\" : \"%+v\" } }\n", attachment.Uuid)
		attachmentUpdateDoc := &openSearchStruct.BulkUpdate{
			Doc: attachment,
		}
		attachmentJsonData, _ := json.Marshal(attachmentUpdateDoc)

		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/getDeleteCommentWithAttachmentBulkOperationStringForOpenSearch Error mashiling attachment struct to json err: %+v",
				err)
			return
		}

		tempAttachmentCreate += string(attachmentJsonData) + "\n"
		attachmentCreate += tempAttachmentCreate
	}

	bulkActionString += attachmentCreate

	return
}

func DeleteCommentWithAttachmentsInOpenSearch(openSearchComment *openSearchStruct.OpenSearchComment, dgraphAttachment []*dgraphStruct.DgraphAttachment) {
	ctx := context.Background()
	var openSearchUpdateDocs []*openSearchStruct.OpenSearchAttachment

	for _, attachment := range dgraphAttachment {
		openSearchUpdateDocs = append(openSearchUpdateDocs, &openSearchStruct.OpenSearchAttachment{
			Uuid:                attachment.Uuid,
			AttachmentDeletedAt: openSearchComment.CommentDeletedAt,
		})
	}

	bulkOperationString, err := getDeleteCommentWithAttachmentBulkOperationStringForOpenSearch(ctx, openSearchComment, openSearchUpdateDocs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeletePostWithAttachmentsInOpenSearch Error getting bulk string for post's attachments err: %+v",
			err)
		return
	}

	err = OpenSearchBulkModels.BulkCreateInOpenSearch(bulkOperationString)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeletePostWithAttachmentsInOpenSearch Error getting bulk string for post with attachments err: %+v",
			err)
		return
	}

}

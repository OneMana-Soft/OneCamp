package business

import (
	"context"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Attachment"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/Attachment"
	"github.com/google/uuid"
)

func CreateAttachment(ctx context.Context, attachmentUUID uuid.UUID, objKey string, createdByUUID uuid.UUID, srcKey string, srcValue string) (err error) {
	err = domain.CreateAttachment(ctx, attachmentUUID, objKey, createdByUUID, srcKey, srcValue)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateAttachment Failed to create new attachment in postgres err: %+v",
			err)
		return
	}

	return
}

func CreateAttachmentForChannelsAndChats(ctx context.Context, attachmentUUID uuid.UUID, objKey string, createdByUUID uuid.UUID, channelUUIDs []string, chatUUIDs []string) (err error) {
	err = domain.CreateAttachmentForChannelsAndChats(ctx, attachmentUUID, objKey, createdByUUID, channelUUIDs, chatUUIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateAttachmentForChannelsAndChats Failed to create new attachment in postgres err: %+v",
			err)
		return
	}

	return
}

func ArchiveAttachmentByAttachmentUUID(ctx context.Context, attachmentUUID uuid.UUID, dgraphTaskUID string, userDgraphUID string) (err error) {

	currentTime := time.Now()
	activityUUID := uuid.New()

	err = domain.UpdateAttachmentDeletedTimeByUUID(ctx, attachmentUUID, &currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveAttachmentByAttachmentUUID failed to update delete time in postgres err: %+v", err)

		return
	}

	dgraphAttachment := &dgraphStruct.DgraphAttachment{
		Uid:       "uid(attachment)",
		Uuid:      attachmentUUID.String(),
		DeletedAt: &currentTime,
	}

	if len(dgraphTaskUID) > 0 && len(userDgraphUID) > 0 {
		dgraphAttachment.Task = &dgraphStruct.DgraphTask{
			Uid: dgraphTaskUID,
			Activity: []*dgraphStruct.DgraphTaskActivity{{
				Uuid:    activityUUID.String(),
				Type:    dgraphStruct.ACTIVITY_TYPE_REMOVE_ATACHMENT,
				LogTime: &currentTime,
				CreatedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraphUID,
				},
			}},
		}
	}

	_, err = domain.CreateOrUpdateDgraphAttachment(ctx, dgraphAttachment)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveAttachmentByAttachmentUUID failed to update delete time in dgraph err: %+v", err)

		return
	}

	opensearchAttachment := &openSearchStruct.OpenSearchAttachment{
		Uuid:                attachmentUUID.String(),
		AttachmentDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	go domain.UpdateAttachmentInOpenSearch(opensearchAttachment)

	return

}

func GetAttachmentByObjUUID(ctx context.Context, objUUID string, srcKey string) (attachment *models.Attachment, err error) {
	attachment, err = domain.GetAttachmentByObjUUID(ctx, objUUID, srcKey)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetAttachmentByObjUUID Failed to get attachment from postgres err: %+v", err)

		return
	}

	return
}

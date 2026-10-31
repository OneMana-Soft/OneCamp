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

	_, err = domain.ArchiveRows(ctx, []uuid.UUID{attachmentUUID}, currentTime)

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

	err = domain.UpdateDgraphAttachment(ctx, dgraphAttachment)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/ArchiveAttachmentByAttachmentUUID failed to update delete time in dgraph err: %+v", err)

		return
	}

	opensearchAttachment := &openSearchStruct.OpenSearchAttachment{
		Uuid:                attachmentUUID.String(),
		AttachmentDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	helpers.GoSafeNamed("domain.UpdateAttachmentInOpenSearch", func() { domain.UpdateAttachmentInOpenSearch(opensearchAttachment) })

	return

}

// IsStored is whether a file's row names a file that is there: one with its
// stored object, not taken off its place. Taking a file off a task or a
// project stamps its row deleted (ArchiveAttachmentByAttachmentUUID) and the
// purge removes it later; until then it is no such file to every read by id
// (business/FileAccess, StoredFilesOfPlace).
func IsStored(att *models.Attachment) bool {
	return att != nil && att.ObjKey != "" && !helpers.IsSoftDeleted(&att.DeletedAt)
}

// StoredFilesOfPlace is the files with these ids kept at this place that are
// there (IsStored), by id: an id that isn't a uuid, or isn't such a file, is
// left out. It decides nothing about who is asking: business/FileAccess
// does, and a system read (a board's export) reads through it directly.
func StoredFilesOfPlace(ctx context.Context, ids []string, srcKey, srcValue string) (map[string]*models.Attachment, error) {
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			valid = append(valid, id)
		}
	}
	found, err := AttachmentsOfPlace(ctx, valid, srcKey, srcValue)
	if err != nil {
		return nil, err
	}
	for id, att := range found {
		if !IsStored(att) {
			delete(found, id)
		}
	}
	return found, nil
}

// AttachmentsOfPlace is the files with these ids kept at this place, by id.
func AttachmentsOfPlace(ctx context.Context, ids []string, srcKey, srcValue string) (map[string]*models.Attachment, error) {
	return domain.AttachmentsOfPlace(ctx, ids, srcKey, srcValue)
}

func GetAttachmentByObjUUID(ctx context.Context, objUUID string, srcKey string) (attachment *models.Attachment, err error) {
	attachment, err = domain.GetAttachmentByObjUUID(ctx, objUUID, srcKey)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetAttachmentByObjUUID Failed to get attachment from postgres err: %+v", err)

		return
	}

	return
}

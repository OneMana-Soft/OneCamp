package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// UpdateDgraphAttachment sets dgraphAttachment on the nodes query finds (its
// variable is attachment), and writes nothing when it finds none. The update
// names its file, so writing it on a query that found nothing made a node
// naming the file with no time, and a node with no time holds its file
// (business/Attachment ArchiveFilesOf). An aborted write is tried again.
func UpdateDgraphAttachment(ctx context.Context, dgraphAttachment *dgraphStruct.DgraphAttachment, query string) error {
	if _, err := dgexec.Mutate(ctx, query, nil, dgexec.Mutation{Cond: "@if(gt(len(attachment), 0))", Set: dgraphAttachment}); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateDgraphAttachment failed to update dgraph attachment err: %+v", err)
		return err
	}
	return nil
}

// BulkAddAttachmentsToDgraph sets the attachments in one mutation and returns
// the uids Dgraph gave them.
func BulkAddAttachmentsToDgraph(ctx context.Context, dgraphAttachments []*dgraphStruct.DgraphAttachment) (dgraphAttachmentUUID []string, err error) {
	uids, err := dgexec.Set(ctx, dgraphAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkAddAttachmentsToDgraph failed to bulk create dgraph attachment err: %+v", err)
		return nil, err
	}
	for _, uid := range uids {
		dgraphAttachmentUUID = append(dgraphAttachmentUUID, uid)
	}
	return dgraphAttachmentUUID, nil
}

// BulkSoftDeleteDgraphAttachments sets attachment_deleted_at on multiple attachments in a single Dgraph mutation.
func BulkSoftDeleteDgraphAttachments(ctx context.Context, attachments []*dgraphStruct.DgraphAttachment, query string) error {
	if _, err := dgexec.Mutate(ctx, query, nil, dgexec.Mutation{Set: attachments}); err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphAttachments failed: %v", err)
		return err
	}
	return nil
}

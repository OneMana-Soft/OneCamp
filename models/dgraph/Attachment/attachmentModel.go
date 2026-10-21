package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// CreateOrUpdateDgraphAttachment upserts dgraphAttachment over query (whose
// variable is attachment) and returns the new node's uid, "" when it updated
// one. An aborted write is tried again.
func CreateOrUpdateDgraphAttachment(ctx context.Context, dgraphAttachment *dgraphStruct.DgraphAttachment, query string) (userUid string, err error) {
	uids, err := dgexec.Upsert(ctx, query, nil, dgraphAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateOrUpdateDgraphAttachment failed to create or update dgraph attachment err: %+v", err)
		return "", err
	}
	// will get uid only when new node is created
	return uids[dgexec.Var("attachment")], nil
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

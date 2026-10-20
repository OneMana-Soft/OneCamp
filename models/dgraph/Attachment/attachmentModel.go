package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

func CreateOrUpdateDgraphAttachment(ctx context.Context, dgraphAttachment *dgraphStruct.DgraphAttachment, query string) (userUid string, err error) {
	pb, err := json.Marshal(dgraphAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphAttachment failed to marshal dgraphAttachment struct err: %+v",
			err)
		return
	}

	// An upsert, so a conflicting concurrent write is retried (DoCommitNow).
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: pb}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphAttachment failed to create or update dgraph attachment err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	userUid = res.Uids["uid(attachment)"]
	return
}

func BulkAddAttachmentsToDgraph(ctx context.Context, dgraphAttachments []*dgraphStruct.DgraphAttachment) (dgraphAttachmentUUID []string, err error) {
	pb, err := json.Marshal(dgraphAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddAttachmentsToDgraph failed to marshal dgraphAttachment struct err: %+v",
			err)
		return
	}

	// One all-or-nothing request: an aborted try created nothing, so trying
	// again makes no duplicates (DoCommitNow).
	res, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Mutations: []*api.Mutation{{SetJson: pb}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddAttachmentsToDgraph failed to bulk create dgraph attachment err: %+v",
			err)
		return
	}

	for _, attachmentUid := range res.Uids {
		dgraphAttachmentUUID = append(dgraphAttachmentUUID, attachmentUid)
	}
	return
}

// BulkSoftDeleteDgraphAttachments sets attachment_deleted_at on multiple attachments in a single Dgraph mutation.
func BulkSoftDeleteDgraphAttachments(ctx context.Context, attachments []*dgraphStruct.DgraphAttachment, query string) error {
	pb, err := json.Marshal(attachments)
	if err != nil {
		return err
	}
	_, err = dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: pb}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphAttachments failed: %v", err)
		return err
	}
	return nil
}

func GetDgraphAttachmentInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphAttachment *dgraphStruct.DgraphAttachment, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphAttachmentInfoByUUID failed to get attachment err: %+v",
			err)
		return
	}

	type Attachments struct {
		AttachmentInfo []dgraphStruct.DgraphAttachment `json:"attachmentInfo"`
	}

	var attachmentsInfo Attachments
	err = json.Unmarshal(resp.Json, &attachmentsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphAttachmentInfoByUUID failed to unmarshal response json err: %+v",
			err)
		return
	}

	if len(attachmentsInfo.AttachmentInfo) == 0 {
		err = errors.New("failed to get dgraph attachment")
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphAttachmentInfoByUUID failed to get dgraph attachment")

		return
	}
	dgraphAttachment = &attachmentsInfo.AttachmentInfo[0]

	return
}

func GetDgraphAttachmentList(ctx context.Context, query string, variables map[string]string) (dgraphAttachments []*dgraphStruct.DgraphAttachment, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphAttachmentList failed to get attachment err: %+v",
			err)
		return
	}

	type Attachments struct {
		AttachmentInfo []*dgraphStruct.DgraphAttachment `json:"attachmentInfo"`
	}

	var attachmentsInfo Attachments
	err = json.Unmarshal(resp.Json, &attachmentsInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetDgraphAttachmentList failed to unmarshal response json err: %+v",
			err)
		return
	}
	dgraphAttachments = attachmentsInfo.AttachmentInfo
	return
}

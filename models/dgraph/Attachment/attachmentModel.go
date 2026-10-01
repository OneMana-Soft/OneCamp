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
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphAttachment failed to marshal dgraphAttachment struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateOrUpdateDgraphAttachment failed to create or update dgraph attachment err: %+v",
			err)
		return
	}

	// will get uid only when new node is created
	userUid = res.Uids["uid(attachment)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/CreateOrUpdateDgraphAttachment failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}

func GetDgraphAttachmentInfoByUUID(ctx context.Context, query string, variables map[string]string) (dgraphAttachment *dgraphStruct.DgraphAttachment, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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

func BulkAddAttachmentsToDgraph(ctx context.Context, dgraphAttachments []*dgraphStruct.DgraphAttachment) (dgraphAttachmentUUID []string, err error) {

	// Convert data to JSON
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(dgraphAttachments)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddAttachmentsToDgraph failed to marshal dgraphAttachment struct err: %+v",
			err)
		return
	}

	mu := &api.Mutation{
		SetJson: pb,
	}
	req := &api.Request{
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}

	res, err := txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/BulkAddAttachmentsToDgraph failed to bulk create dgraph attachment err: %+v",
			err)
		return
	}

	for _, attachmentUid := range res.Uids {

		dgraphAttachmentUUID = append(dgraphAttachmentUUID, attachmentUid)
	}

	// userUid = res.Uids["uid(attachment)"]

	defer func() {
		err = txn.Discard(ctx)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/BulkAddAttachmentsToDgraph failed to discard dgraph txn err: %+v",
				err)
		}
	}()
	return
}

func GetDgraphAttachmentList(ctx context.Context, query string, variables map[string]string) (dgraphAttachments []*dgraphStruct.DgraphAttachment, err error) {
	txn := dgraphInit.DgraphClient.NewTxn()

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

// BulkSoftDeleteDgraphAttachments sets attachment_deleted_at on multiple attachments in a single Dgraph mutation.
func BulkSoftDeleteDgraphAttachments(ctx context.Context, attachments []*dgraphStruct.DgraphAttachment, query string) error {
	txn := dgraphInit.DgraphClient.NewTxn()
	pb, err := json.Marshal(attachments)
	if err != nil {
		return err
	}
	mu := &api.Mutation{SetJson: pb}
	req := &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mu},
		CommitNow: true,
	}
	_, err = txn.Do(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BulkSoftDeleteDgraphAttachments failed: %v", err)
		return err
	}
	return nil
}

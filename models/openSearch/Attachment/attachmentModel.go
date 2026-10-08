package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func UpdateAttachmentInOpenSearch(ctx context.Context, openSearchAttachment *openSearchStruct.OpenSearchAttachment) (err error) {

	jsonData, err := json.Marshal(openSearchAttachment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateAttachmentInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchAttachment.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      "attachments",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateAttachmentInOpenSearch failed to update attachment document err: %+v",
			err)
		return
	}
	return
}

// DeleteAttachmentInOpenSearch removes the document outright. The other
// functions here mark documents deleted so search can hide them; this one is
// for a purge, when the file itself is gone and there is nothing to hide.
// A document that is already absent is not an error.
func DeleteAttachmentInOpenSearch(ctx context.Context, attachmentUUID string) error {
	if err := opensearchInit.DeleteDocument(ctx, openSearchStruct.ATTACHMENT_INDEX, attachmentUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteAttachmentInOpenSearch %s: %v", attachmentUUID, err)
		return err
	}
	return nil
}

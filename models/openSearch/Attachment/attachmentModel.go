package models

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateAttachmentInOpenSearch(ctx context.Context, openSearchAttachment *openSearchStruct.OpenSearchAttachment) (err error) {

	jsonData, err := json.Marshal(openSearchAttachment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateAttachmentInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchAttachment.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "attachments",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateAttachmentInOpenSearch failed to create attachment document err: %+v",
			err)
		return
	}
	return
}

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
	_, err := opensearchInit.OpenSearchClient.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{
		Index:      openSearchStruct.ATTACHMENT_INDEX,
		DocumentID: attachmentUUID,
	})
	if err != nil && !strings.Contains(err.Error(), "not_found") && !strings.Contains(err.Error(), "404") {
		helpers.LogErrorWithContext(ctx, "models/DeleteAttachmentInOpenSearch %s: %v", attachmentUUID, err)
		return err
	}
	return nil
}

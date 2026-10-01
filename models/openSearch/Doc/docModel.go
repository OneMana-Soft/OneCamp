package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateDocInOpenSearch(ctx context.Context, openSearchDoc *openSearchStruct.OpenSearchDoc) (err error) {

	jsonData, err := json.Marshal(openSearchDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateDocInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchDoc.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      openSearchStruct.DOC_INDEX,
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateDocInOpenSearch failed to create doc document err: %+v",
			err)
		return
	}
	return
}

func UpdateDocInOpenSearch(ctx context.Context, openSearchDoc *openSearchStruct.OpenSearchDoc) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchDoc,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDocInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchDoc.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      openSearchStruct.DOC_INDEX,
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDocInOpenSearch failed to update doc document err: %+v",
			err)
		return
	}
	return
}

func DeleteDocInOpenSearch(ctx context.Context, docUUID string, deletedAt int64) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: map[string]interface{}{
			"deleted_date": deletedAt,
		},
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteDocInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      openSearchStruct.DOC_INDEX,
			DocumentID: docUUID,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/DeleteDocInOpenSearch failed to update doc document err: %+v",
			err)
		return
	}
	return
}

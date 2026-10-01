package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateCommentInOpenSearch(ctx context.Context, openSearchComment *openSearchStruct.OpenSearchComment) (err error) {

	jsonData, err := json.Marshal(openSearchComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateCommentInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchComment.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "comments",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateCommentInOpenSearch failed to create comment document err: %+v",
			err)
		return
	}

	return
}

func UpdateCommentInOpenSearch(ctx context.Context, openSearchComment *openSearchStruct.OpenSearchComment) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchComment,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateCommentInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchComment.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      "comments",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateCommentInOpenSearch failed to update comment document err: %+v",
			err)
		return
	}
	return
}

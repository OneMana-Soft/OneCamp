package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func UpdatePostInOpenSearch(ctx context.Context, openSearchPost *openSearchStruct.OpenSearchPost) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchPost,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChatInOpenSearch Error mashiling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchPost.Uuid

	err = opensearchInit.UpdateDocument(context.Background(), "posts", docId, document)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdatePostInOpenSearch failed to update post document err: %+v",
			err)
		return
	}

	return
}

func CreatePostInOpenSearch(ctx context.Context, openSearchPost *openSearchStruct.OpenSearchPost) (err error) {

	jsonData, err := json.Marshal(openSearchPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreatePostInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchPost.Uuid
	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "posts",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreatePostInOpenSearch failed to create post document err: %+v",
			err)
		return
	}

	return
}

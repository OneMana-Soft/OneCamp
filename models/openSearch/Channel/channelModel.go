package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateChannelInOpenSearch(ctx context.Context, openSearchChannel *openSearchStruct.OpenSearchChannel) (err error) {

	jsonData, err := json.Marshal(openSearchChannel)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChannelInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchChannel.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "channels",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateChannelInOpenSearch failed to create channel document err: %+v",
			err)
		return
	}
	return
}

func UpdateChannelInOpenSearch(ctx context.Context, openSearchChannel *openSearchStruct.OpenSearchChannel) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchChannel,
	}

	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChannelInOpenSearch Error marshaling struct to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchChannel.Uuid

	err = opensearchInit.UpdateDocument(context.Background(), "channels", docId, document)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateChannelInOpenSearch failed to update channel document err: %+v",
			err)
		return
	}
	return
}

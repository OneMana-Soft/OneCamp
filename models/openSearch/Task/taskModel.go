package models

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateTaskInOpenSearch(ctx context.Context, openSearchTask *openSearchStruct.OpenSearchTask) (err error) {

	jsonData, err := json.Marshal(openSearchTask)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTaskInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchTask.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "tasks",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTaskInOpenSearch failed to create task document err: %+v",
			err)
		return
	}
	return
}

func UpdateTaskInOpenSearch(ctx context.Context, openSearchTask *openSearchStruct.OpenSearchTask) (err error) {
	// Callers run this in a goroutine; with search not connected (a tool, a
	// test) a nil client would panic there and take the process with it.
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchTask,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchTask.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      "tasks",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskInOpenSearch failed to update task document err: %+v",
			err)
		return
	}
	return
}

package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateProjectInOpenSearch(ctx context.Context, openSearchProject *openSearchStruct.OpenSearchProject) (err error) {

	jsonData, err := json.Marshal(openSearchProject)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateProjectInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchProject.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      "projects",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateProjectInOpenSearch failed to create project document err: %+v",
			err)
		return
	}
	return
}

func UpdateProjectInOpenSearch(ctx context.Context, openSearchProject *openSearchStruct.OpenSearchProject) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc: openSearchProject,
	}

	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchProject.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      "projects",
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectInOpenSearch failed to update project document err: %+v",
			err)
		return
	}
	return
}

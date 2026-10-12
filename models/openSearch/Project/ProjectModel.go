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

// UpdateProjectFieldsInOpenSearch changes only the named fields of a
// project's search document. A whole OpenSearchProject always carries
// deleted_date (it has no omitempty, so unarchiving can clear it), and a
// rename sent that way wrote "deleted_date": null, putting an archived
// project back in search.
func UpdateProjectFieldsInOpenSearch(ctx context.Context, projectUUID string, fields map[string]interface{}) (err error) {
	jsonData, err := json.Marshal(openSearchStruct.BulkUpdate{Doc: fields})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectFieldsInOpenSearch Error mashiling fields to json err: %+v",
			err)
		return
	}
	err = opensearchInit.UpdateDocument(context.Background(), "projects", projectUUID, openSearchStruct.IndexReader(jsonData))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateProjectFieldsInOpenSearch failed to update project document err: %+v",
			err)
		return
	}
	return
}

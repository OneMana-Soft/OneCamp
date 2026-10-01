package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateTeamInOpenSearch(ctx context.Context, openSearchTeam *openSearchStruct.OpenSearchTeam) (err error) {

	jsonData, err := json.Marshal(openSearchTeam)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTeamInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchTeam.Uuid

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      openSearchStruct.TEAM_INDEX,
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTeamInOpenSearch failed to create team document err: %+v",
			err)
		return
	}

	return
}

func UpdateTeamInOpenSearch(ctx context.Context, openSearchTeam *openSearchStruct.OpenSearchTeam) (err error) {

	doc := openSearchStruct.BulkUpdate{
		Doc:         openSearchTeam,
		DocAsUpsert: true,
	}
	jsonData, err := json.Marshal(doc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTeamInOpenSearch Error mashiling strut to json err: %+v",
			err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	docId := openSearchTeam.Uuid

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      openSearchStruct.TEAM_INDEX,
			DocumentID: docId,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTeamInOpenSearch failed to update team document err: %+v",
			err)
		return
	}
	return
}

func DeleteTeamInOpenSearch(ctx context.Context, teamUUID string, deletedAt int64) (err error) {
	doc := openSearchStruct.BulkUpdate{
		Doc: map[string]interface{}{
			"deleted_date": deletedAt,
		},
	}
	jsonData, err := json.Marshal(doc)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteTeamInOpenSearch Error marshaling err: %+v", err)
		return
	}

	document := openSearchStruct.IndexReader(jsonData)

	_, err = opensearchInit.OpenSearchClient.Update(
		context.Background(),
		opensearchapi.UpdateReq{
			Index:      openSearchStruct.TEAM_INDEX,
			DocumentID: teamUUID,
			Body:       document,
		},
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteTeamInOpenSearch failed to update team document err: %+v", err)
		return
	}
	return
}

package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

func CreateBoardInOpenSearch(ctx context.Context, openSearchBoard *openSearchStruct.OpenSearchBoard) (err error) {
	jsonData, err := json.Marshal(openSearchBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardInOpenSearch marshal err: %+v", err)
		return
	}

	_, err = opensearchInit.OpenSearchClient.Document.Create(
		context.Background(),
		opensearchapi.DocumentCreateReq{
			Index:      openSearchStruct.BOARD_INDEX,
			DocumentID: openSearchBoard.Uuid,
			Body:       openSearchStruct.IndexReader(jsonData),
		},
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardInOpenSearch create err: %+v", err)
		return
	}
	return
}

// UpdateBoardFieldsInOpenSearch applies a partial field update (used for title,
// privacy, and access changes). A map is used so a `false` boolean is written
// rather than dropped by struct omitempty. doc_as_upsert keeps it resilient if
// the board was never indexed.
func UpdateBoardFieldsInOpenSearch(ctx context.Context, boardUUID string, fields map[string]interface{}) (err error) {
	if len(fields) == 0 {
		return nil
	}
	doc := openSearchStruct.BulkUpdate{
		Doc:         fields,
		DocAsUpsert: true,
	}
	jsonData, err := json.Marshal(doc)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateBoardFieldsInOpenSearch marshal err: %+v", err)
		return
	}

	err = opensearchInit.UpdateDocument(context.Background(), openSearchStruct.BOARD_INDEX, boardUUID, openSearchStruct.IndexReader(jsonData))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateBoardFieldsInOpenSearch update err: %+v", err)
		return
	}
	return
}

func DeleteBoardInOpenSearch(ctx context.Context, boardUUID string, deletedAt int64) (err error) {
	doc := openSearchStruct.BulkUpdate{
		Doc: map[string]interface{}{"deleted_date": deletedAt},
	}
	jsonData, err := json.Marshal(doc)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteBoardInOpenSearch marshal err: %+v", err)
		return
	}

	err = opensearchInit.UpdateDocument(context.Background(), openSearchStruct.BOARD_INDEX, boardUUID, openSearchStruct.IndexReader(jsonData))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteBoardInOpenSearch update err: %+v", err)
		return
	}
	return
}

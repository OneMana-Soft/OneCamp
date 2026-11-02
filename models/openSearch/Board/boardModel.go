package models

import (
	"context"
	"encoding/json"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	bulkModel "github.com/akashc777/OneCamp/models/openSearch/Bulk"
)

// CreateBoardInOpenSearch writes a new board's whole entry, merged into one
// its text already started (SetBoardTextsInOpenSearch): a create would fail
// with a conflict there, and the board would stay in search with no title and
// no access.
func CreateBoardInOpenSearch(ctx context.Context, openSearchBoard *openSearchStruct.OpenSearchBoard) (err error) {
	jsonData, err := json.Marshal(openSearchStruct.BulkUpdate{Doc: openSearchBoard})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardInOpenSearch marshal err: %+v", err)
		return
	}
	err = opensearchInit.UpsertDocument(context.Background(), openSearchStruct.BOARD_INDEX, openSearchBoard.Uuid, openSearchStruct.IndexReader(jsonData))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateBoardInOpenSearch create err: %+v", err)
	}
	return
}

// BoardText is what one board says, for SetBoardTextsInOpenSearch.
type BoardText struct {
	BoardUUID, Text string
}

// SetBoardTextsInOpenSearch writes what each board says onto its entry, in
// one bulk request. An entry not written yet is started with the text alone,
// which no search reaches (it grants no one) until the board's own write
// fills it.
func SetBoardTextsInOpenSearch(ctx context.Context, texts []BoardText) error {
	docs := make([]bulkModel.Upsert, len(texts))
	for i, t := range texts {
		docs[i] = bulkModel.Upsert{ID: t.BoardUUID, Doc: map[string]any{"board_uuid": t.BoardUUID, "board_text": t.Text}}
	}
	return bulkModel.BulkUpsert(ctx, openSearchStruct.BOARD_INDEX, docs)
}

// UpdateBoardFieldsInOpenSearch applies a partial field update (used for title,
// privacy, and access changes). A map is used so a `false` boolean is written
// rather than dropped by struct omitempty. A board the index doesn't have is
// left for its whole document (opensearchInit.UpdateDocument): created from
// these fields alone it held a title and its privacy and none of its access.
func UpdateBoardFieldsInOpenSearch(ctx context.Context, boardUUID string, fields map[string]interface{}) (err error) {
	if len(fields) == 0 {
		return nil
	}
	doc := openSearchStruct.BulkUpdate{
		Doc: fields,
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

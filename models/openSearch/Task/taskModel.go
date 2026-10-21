package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	bulkModels "github.com/akashc777/OneCamp/models/openSearch/Bulk"
)

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

	err = opensearchInit.UpdateDocument(context.Background(), "tasks", docId, document)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskInOpenSearch failed to update task document err: %+v",
			err)
		return
	}
	return
}

// bulkUpdateBody is the NDJSON of a bulk request that updates each task's
// search document with its fields set. Pure.
func bulkUpdateBody(tasks []*openSearchStruct.OpenSearchTask) (string, error) {
	var b strings.Builder
	for _, t := range tasks {
		if t == nil || t.Uuid == "" {
			continue
		}
		head, err := json.Marshal(map[string]map[string]string{"update": {"_index": "tasks", "_id": t.Uuid}})
		if err != nil {
			return "", err
		}
		doc, err := json.Marshal(openSearchStruct.BulkUpdate{Doc: t})
		if err != nil {
			return "", err
		}
		b.Write(head)
		b.WriteByte('\n')
		b.Write(doc)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// UpdateTasksInOpenSearch updates many tasks' search documents in one bulk
// request (PR2): a status renamed across a project, a bulk change.
func UpdateTasksInOpenSearch(ctx context.Context, tasks []*openSearchStruct.OpenSearchTask) error {
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}
	body, err := bulkUpdateBody(tasks)
	if err != nil || body == "" {
		return err
	}
	return bulkModels.BulkCreateInOpenSearch(body)
}

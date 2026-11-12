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
// search document with its fields set, and each field in clear emptied
// (null): a field an update leaves out is kept as it was. Pure.
func bulkUpdateBody(tasks []*openSearchStruct.OpenSearchTask, clear ...string) (string, error) {
	var b strings.Builder
	for _, t := range tasks {
		if t == nil || t.Uuid == "" {
			continue
		}
		head, err := json.Marshal(map[string]map[string]string{"update": {"_index": "tasks", "_id": t.Uuid}})
		if err != nil {
			return "", err
		}
		fields, err := withCleared(t, clear)
		if err != nil {
			return "", err
		}
		doc, err := json.Marshal(openSearchStruct.BulkUpdate{Doc: fields})
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

// withCleared is t's fields with each one in clear set to null, or t itself
// when there's none to clear.
func withCleared(t *openSearchStruct.OpenSearchTask, clear []string) (any, error) {
	if len(clear) == 0 {
		return t, nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	fields := map[string]any{}
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	for _, f := range clear {
		fields[f] = nil
	}
	return fields, nil
}

// UpdateTasksInOpenSearch updates many tasks' search documents in one bulk
// request (PR2): a status renamed across a project, a bulk change. Each field
// in clear is emptied (a priority taken off).
func UpdateTasksInOpenSearch(ctx context.Context, tasks []*openSearchStruct.OpenSearchTask, clear ...string) error {
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}
	body, err := bulkUpdateBody(tasks, clear...)
	if err != nil || body == "" {
		return err
	}
	return bulkModels.BulkCreateInOpenSearch(body)
}

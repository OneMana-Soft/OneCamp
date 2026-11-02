package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// BulkCreateInOpenSearch performs a bulk (NDJSON) write.
//
// The body goes through openSearchStruct.BulkReader, which is the same guarantee
// IndexReader gives single-document writes: no embedded media and no unbounded
// field reaches the cluster. This matters more here than anywhere else, because the
// bulk path carries most of the write volume — every post and comment created with
// attachments, and every cascading rename or delete — and it is assembled by hand
// as NDJSON in the domain layer, so it would otherwise be the one way around the
// guard.
func BulkCreateInOpenSearch(bulkCreateString string) (err error) {
	// Search not connected (a tool, a test): say so, rather than panic in the
	// caller's goroutine, as the single-document writes do.
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}

	_, err = opensearchInit.OpenSearchClient.Bulk(
		context.Background(),
		opensearchapi.BulkReq{
			Body: openSearchStruct.BulkReader(bulkCreateString),
		},
	)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"models/BulkCreateInOpenSearch Error performing bulk operation err: %+v",
			err)
		return
	}

	return
}

// BulkIndexChecked performs a bulk (NDJSON) write, through the same BulkReader
// guard, and answers an error when the cluster refused any item, naming the
// first: the outbox's sinks try again on it, where BulkCreateInOpenSearch's
// callers only ever saw a request that failed whole.
func BulkIndexChecked(ctx context.Context, body string) error {
	resp, err := opensearchInit.OpenSearchClient.Bulk(ctx, opensearchapi.BulkReq{Body: openSearchStruct.BulkReader(body)})
	if err != nil {
		return err
	}
	if resp == nil || !resp.Errors {
		return nil
	}
	for _, item := range resp.Items {
		for op, r := range item {
			if r.Error != nil {
				return fmt.Errorf("%s of %s in %s refused (%d): %s: %s", op, r.ID, r.Index, r.Status, r.Error.Type, r.Error.Reason)
			}
		}
	}
	return errors.New("the bulk write said some items failed, and named none")
}

// Upsert is one entry a BulkUpsert writes: its id, and the fields it sets.
type Upsert struct {
	ID  string
	Doc any
}

// upsertRetries is how often a bulk upsert's item is retried on a version
// conflict, as opensearchInit's single upserts are.
const upsertRetries = 5

// BulkUpsert is opensearchInit.UpsertDocument for several entries of one index
// in one bulk request: each entry's fields are written onto it, and an entry
// that isn't there is made from them. An error names the first entry the
// cluster refused.
func BulkUpsert(ctx context.Context, index string, docs []Upsert) error {
	if len(docs) == 0 {
		return nil
	}
	if opensearchInit.OpenSearchClient == nil {
		return errors.New("search is not connected")
	}
	body, err := upsertBody(index, docs)
	if err != nil {
		return err
	}
	return BulkIndexChecked(ctx, body)
}

// upsertBody is BulkUpsert's NDJSON: an update with doc_as_upsert per entry.
// Pure.
func upsertBody(index string, docs []Upsert) (string, error) {
	var b strings.Builder
	for _, d := range docs {
		head, err := json.Marshal(map[string]map[string]any{"update": {"_index": index, "_id": d.ID, "retry_on_conflict": upsertRetries}})
		if err != nil {
			return "", err
		}
		doc, err := json.Marshal(map[string]any{"doc": d.Doc, "doc_as_upsert": true})
		if err != nil {
			return "", fmt.Errorf("entry %s: %w", d.ID, err)
		}
		b.Write(head)
		b.WriteByte('\n')
		b.Write(doc)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

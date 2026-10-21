package opensearchInit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// updateRetries is how many times OpenSearch tries a partial update again
// when another write to the same document lands in between.
var updateRetries = 5

// UpdateDocument applies a partial update (a body of {"doc": …}) to one
// document. OpenSearch reads the document, merges, and writes it back; when
// another write lands in between, the update fails with a version conflict
// and its change would be lost (a task renamed and moved at once, a seed
// writing a doc twice). It's tried again instead, from the document as it
// then is, so both changes land.
//
// A document that isn't there is left alone: the update is skipped, and said
// at DEBUG. Merging some fields into nothing would write a document holding
// only them (a team with a name and no members, a board with its privacy and
// none of its access) for search to serve. The demo's nightly reset empties
// the index, and every rename after it failed at ERROR with
// document_missing_exception (21 in a week). A body asking for doc_as_upsert
// has it taken out for the same reason: only UpsertDocument, given the whole
// document, creates one.
func UpdateDocument(ctx context.Context, index, id string, body io.Reader) error {
	return updateDocument(ctx, OpenSearchClient, index, id, body, false)
}

// UpsertDocument updates a document from the whole of it ({"doc": …} with
// every field the index keeps for it, as when the thing is created) and
// creates the document when it isn't there, so an index emptied by a reset
// fills again as things are written.
func UpsertDocument(ctx context.Context, index, id string, body io.Reader) error {
	return updateDocument(ctx, OpenSearchClient, index, id, body, true)
}

func updateDocument(ctx context.Context, c *opensearchapi.Client, index, id string, body io.Reader, whole bool) error {
	if c == nil {
		return errors.New("search is not connected")
	}
	body, err := withUpsert(body, whole)
	if err != nil {
		return err
	}
	_, err = c.Update(ctx, opensearchapi.UpdateReq{
		Index:      index,
		DocumentID: id,
		Body:       body,
		Params:     opensearchapi.UpdateParams{RetryOnConflict: &updateRetries},
	})
	if !whole && documentMissing(err) {
		helpers.LogDebugWithContext(ctx, "search: no %s document %s to update; it is written whole when it is next indexed", index, id)
		return nil
	}
	return err
}

// withUpsert is the update body with doc_as_upsert as whole says: set for a
// whole document, taken out of a partial one.
func withUpsert(body io.Reader, whole bool) (io.Reader, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("search update body: %w", err)
	}
	if whole {
		fields["doc_as_upsert"] = json.RawMessage("true")
	} else {
		delete(fields, "doc_as_upsert")
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

// documentMissing reports whether OpenSearch refused an update because there
// was no document (or no index) to update.
func documentMissing(err error) bool {
	var se *opensearch.StructError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		return false
	}
	return se.Err.Type == "document_missing_exception" || se.Err.Type == "index_not_found_exception"
}

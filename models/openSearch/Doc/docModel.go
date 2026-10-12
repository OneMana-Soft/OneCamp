package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// IndexDocEntry writes a doc's whole entry, replacing whatever the index
// held for it: an entry is what the doc says now, never a patch on an old one.
func IndexDocEntry(ctx context.Context, entry *openSearchStruct.OpenSearchDocEntry) error {
	if opensearchInit.OpenSearchClient == nil {
		return fmt.Errorf("search is not connected")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = opensearchInit.OpenSearchClient.Index(ctx, opensearchapi.IndexReq{
		Index:      openSearchStruct.DOC_INDEX,
		DocumentID: entry.Uuid,
		Body:       openSearchStruct.IndexReader(raw),
	})
	return err
}

// docEntriesBulkBytes bounds one bulk request of whole entries: a doc's text
// is in its entry, and a page of long docs in one request is a large heap on
// the search node.
const docEntriesBulkBytes = 4 << 20

// BulkIndexDocEntries writes many docs' whole entries, in requests of a few
// megabytes, and answers how many the index took and the first failure it
// reported, if any.
func BulkIndexDocEntries(ctx context.Context, entries []*openSearchStruct.OpenSearchDocEntry) (written int, err error) {
	if opensearchInit.OpenSearchClient == nil {
		return 0, fmt.Errorf("search is not connected")
	}
	var buf bytes.Buffer
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		resp, err := opensearchInit.OpenSearchClient.Bulk(ctx, opensearchapi.BulkReq{Body: openSearchStruct.BulkReader(buf.String())})
		buf.Reset()
		n := pending
		pending = 0
		if err != nil {
			return err
		}
		failed := 0
		var first string
		for _, item := range resp.Items {
			for _, r := range item {
				if r.Error != nil {
					failed++
					if first == "" {
						first = fmt.Sprintf("%s: %s", r.ID, r.Error.Reason)
					}
				}
			}
		}
		written += n - failed
		if failed > 0 {
			return fmt.Errorf("%d of %d doc entries not written, first: %s", failed, n, first)
		}
		return nil
	}
	for _, e := range entries {
		if e == nil || strings.TrimSpace(e.Uuid) == "" {
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return written, err
		}
		action, _ := json.Marshal(map[string]map[string]string{"index": {"_index": openSearchStruct.DOC_INDEX, "_id": e.Uuid}})
		buf.Write(action)
		buf.WriteByte('\n')
		buf.Write(raw)
		buf.WriteByte('\n')
		pending++
		if buf.Len() >= docEntriesBulkBytes {
			if err := flush(); err != nil {
				return written, err
			}
		}
	}
	return written, flush()
}

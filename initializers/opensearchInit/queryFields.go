package opensearchInit

// The searchable copies the query grammar's filters read (business/GlobalSearch's
// sources, docs C2-1): fields every entry has carried since its index was
// made, kept only to be returned, never to be filtered on. from: needs who
// wrote a post, a reply or a file; updated: needs when anything last changed.
//
// A copy is a sub-field, .q, which a live index takes as it is: new and
// rewritten entries carry it at once, and older entries get it from
// business/GlobalSearch's one-time pass (queryFieldsBackfill.go). An index
// that won't take it (a field mapped differently on an old install) is
// logged, and the engine leaves that filter out there rather than finding
// nothing (QueryFieldReady).

import (
	"context"
	"strings"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// QueryCopy is the sub-field a searchable copy is kept under.
const QueryCopy = ".q"

const (
	keywordCopy = `{ "type": "keyword", "index": false, "fields": { "q": { "type": "keyword" } } }`
	dateCopy    = `{ "type": "date", "format": "epoch_second", "index": false, "fields": { "q": { "type": "date", "format": "epoch_second" } } }`
)

// QueryCopies are, by index, the fields given a searchable copy, each with
// its mapping as the index was made plus the copy.
var QueryCopies = map[string]map[string]string{
	"posts":       {"post_by_user_id": keywordCopy, "updated_date": dateCopy},
	"comments":    {"comment_by_user_id": keywordCopy, "updated_date": dateCopy},
	"attachments": {"attachment_by_user_id": keywordCopy},
	"chats":       {"updated_date": dateCopy},
	"docs":        {"updated_date": dateCopy},
	"boards":      {"updated_date": dateCopy},
	"events":      {"updated_date": dateCopy},
	"tasks":       {"updated_date": dateCopy},
	"projects":    {"updated_date": dateCopy},
	"channels":    {"updated_date": dateCopy},
	"teams":       {"updated_date": dateCopy},
	"users":       {"updated_date": dateCopy},
}

var (
	copiesMu sync.RWMutex
	copied   = map[string]bool{} // "index/field.q"
)

// QueryFieldReady is whether an index has the searchable copy a field names
// (field ends in .q): mapped, so new entries carry it. Any other field is
// ready.
func QueryFieldReady(index, field string) bool {
	if !strings.HasSuffix(field, QueryCopy) {
		return true
	}
	copiesMu.RLock()
	defer copiesMu.RUnlock()
	return copied[index+"/"+field]
}

// ensureQueryCopies maps every searchable copy, index by index.
func ensureQueryCopies(ctx context.Context) {
	if OpenSearchClient == nil {
		return
	}
	for index, fields := range QueryCopies {
		var props []string
		for field, mapping := range fields {
			props = append(props, `"`+field+`": `+mapping)
		}
		_, err := OpenSearchClient.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
			Indices: []string{index},
			Body:    strings.NewReader(`{ "properties": { ` + strings.Join(props, ", ") + ` } }`),
		})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "opensearchInit: the %s index didn't take its searchable copies, so search won't filter there by them: %+v", index, err)
			continue
		}
		copiesMu.Lock()
		for field := range fields {
			copied[index+"/"+field+QueryCopy] = true
		}
		copiesMu.Unlock()
	}
}

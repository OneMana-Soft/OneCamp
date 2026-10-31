package whoKnowsDomain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

// Seed is one item the words matched: its kind (doc, task, message) and uuid.
type Seed struct {
	Kind string
	ID   string
}

// Reach is what the index may grant a reader: the same sources the unified
// search's doc, task and post clauses use (domain/GlobalSearch). The index's
// copies can be stale either way, so every seed is decided again against the
// graph (business/WhoKnows); this only keeps the search from wading through
// what the reader plainly can't open.
type Reach struct {
	UserUUID string
	Channels []string
	Projects []string
	Teams    []string
}

// The indices seeds come from, and the field each names its item by.
var seedIndices = map[string]struct{ kind, idField string }{
	openSearchStruct.DOC_INDEX:  {KindDoc, "doc_uuid"},
	openSearchStruct.TASK_INDEX: {KindTask, "task_id"},
	openSearchStruct.POST_INDEX: {KindMessage, "post_id"},
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// SeedQuery is the OpenSearch body that finds up to size docs, tasks and
// channel posts matching text, within reach, best first, live only. Every
// value is JSON-marshalled into it, never formatted as text. DMs and group
// chats are never seeds: a private conversation is not evidence to show
// anyone, even a participant.
func SeedQuery(r Reach, text string, size int) string {
	user := jsonOf(r.UserUUID)
	words := jsonOf(text)
	channels, projects, teams := jsonOf(helpers.NonNil(r.Channels)), jsonOf(helpers.NonNil(r.Projects)), jsonOf(helpers.NonNil(r.Teams))
	spaces := jsonOf(append(append([]string{}, helpers.NonNil(r.Projects)...), helpers.NonNil(r.Teams)...))
	live := `"filter": { "bool": { "must_not": [ { "exists": { "field": "deleted_date" } } ] } }`
	return fmt.Sprintf(`{
	"size": %d,
	"_source": ["doc_uuid", "task_id", "post_id"],
	"query": { "bool": { "minimum_should_match": 1, "should": [
		{ "bool": { "must": [
			{ "term": { "_index": "docs" } },
			{ "bool": { "minimum_should_match": 1, "should": [
				{ "term": { "doc_created_by_user_id": %[2]s } },
				{ "term": { "doc_reading_users": %[2]s } },
				{ "term": { "doc_editing_users": %[2]s } },
				{ "term": { "doc_commenting_users": %[2]s } },
				{ "term": { "doc_owner_user_id": %[2]s } },
				{ "term": { "doc_full_access_users": %[2]s } },
				{ "terms": { "doc_team_uuids": %[5]s } },
				{ "terms": { "doc_space_uuid": %[6]s } },
				{ "term": { "doc_private": false } }
			] } },
			{ "multi_match": { "query": %[3]s, "fields": ["doc_title^3", "doc_body"] } }
		], %[7]s } },
		{ "bool": { "must": [
			{ "term": { "_index": "tasks" } },
			{ "bool": { "minimum_should_match": 1, "should": [
				{ "terms": { "task_project_id": %[4]s } },
				{ "term": { "task_assignee_user_id": %[2]s } },
				{ "bool": { "must": [ { "term": { "task_created_by_user_id": %[2]s } } ], "must_not": [ { "exists": { "field": "task_project_id" } } ] } }
			] } },
			{ "multi_match": { "query": %[3]s, "fields": ["task_name^3", "task_desc", "task_label^2"] } }
		], %[7]s } },
		{ "bool": { "must": [
			{ "term": { "_index": "posts" } },
			{ "terms": { "post_ch_id": %[8]s } },
			{ "multi_match": { "query": %[3]s, "fields": ["post_body"] } }
		], %[7]s } }
	] } },
	"sort": [ { "_score": { "order": "desc" } } ]
}`, size, user, words, projects, teams, spaces, live, channels)
}

// search runs a seed query (a seam for tests).
var search = opensearchModels.SearchIDs

// KeywordSeeds is up to size items matching text within reach, best first,
// each once.
func KeywordSeeds(ctx context.Context, r Reach, text string, size int) ([]Seed, error) {
	if strings.TrimSpace(text) == "" || size <= 0 || r.UserUUID == "" {
		return nil, nil
	}
	idFields := map[string]string{}
	indices := make([]string, 0, len(seedIndices))
	for idx, s := range seedIndices {
		idFields[idx] = s.idField
		indices = append(indices, idx)
	}
	hits, err := search(ctx, indices, SeedQuery(r, text, size), idFields)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WhoKnows KeywordSeeds: %+v", err)
		return nil, err
	}
	out := make([]Seed, 0, len(hits))
	seen := map[Seed]bool{}
	for _, h := range hits {
		s, ok := seedIndices[h.Index]
		if !ok || !helpers.IsCanonicalUUID(h.ID) {
			continue
		}
		seed := Seed{Kind: s.kind, ID: strings.ToLower(h.ID)}
		if !seen[seed] {
			seen[seed] = true
			out = append(out, seed)
		}
	}
	return out, nil
}

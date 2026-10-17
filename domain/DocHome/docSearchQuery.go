package domain

// The docs home's search (docs programme B5): words in a doc's body, not only
// its title, over the search index's docs (the index global search reads),
// with the passages they were found in.
//
// The query is built here and run by models/openSearch/DocSearch. It admits
// only docs the person reaches as the index records it (their own, shared
// with them, the workspace's; and, once A5 indexes them, as owner, with full
// access, through a team or a space). That is a first cut: every hit is then
// put to the Access Resolver (business/DocHome), which has the last word, and
// a hit it refuses is never named.

import (
	"encoding/json"
	"strings"
)

// SearchCandidates is how many hits a search reads, best first: enough for
// every result anyone reads, few enough that the access check stays quick.
const SearchCandidates = 200

// FragmentSize and Fragments shape the passages: about a line of text each.
const (
	FragmentSize = 150
	Fragments    = 2
)

// DocSearchQuery is one search, for one person.
type DocSearchQuery struct {
	Text string
	// Who asks, by uuid, and the projects and teams they're in (whose spaces
	// they reach once A5 indexes spaces).
	UserUUID     string
	ProjectUUIDs []string
	TeamUUIDs    []string
	// OwnerUUID keeps only this person's docs.
	OwnerUUID string
}

// term is a term clause.
func term(field string, value any) map[string]any {
	return map[string]any{"term": map[string]any{field: value}}
}

// terms is a terms clause, or nil for no values.
func terms(field string, values []string) map[string]any {
	if len(values) == 0 {
		return nil
	}
	return map[string]any{"terms": map[string]any{field: values}}
}

// reach is who reaches a doc as the index records it. A5's fields
// (doc_owner_user_id, doc_full_access_users, doc_team_uuids, doc_space_uuid)
// match nothing until it indexes them.
func reach(q DocSearchQuery) []map[string]any {
	out := []map[string]any{
		term("doc_private", false),
		term("doc_created_by_user_id", q.UserUUID),
		term("doc_reading_users", q.UserUUID),
		term("doc_editing_users", q.UserUUID),
		term("doc_commenting_users", q.UserUUID),
		term("doc_owner_user_id", q.UserUUID),
		term("doc_full_access_users", q.UserUUID),
	}
	if c := terms("doc_team_uuids", q.TeamUUIDs); c != nil {
		out = append(out, c)
	}
	spaces := append(append([]string{}, q.ProjectUUIDs...), q.TeamUUIDs...)
	if c := terms("doc_space_uuid", spaces); c != nil {
		out = append(out, c)
	}
	return out
}

// DocSearchRequest is the search's body for the index. Pure.
func DocSearchRequest(q DocSearchQuery) (string, error) {
	text := strings.TrimSpace(q.Text)
	filter := []map[string]any{
		{"bool": map[string]any{"should": reach(q), "minimum_should_match": 1}},
	}
	if q.OwnerUUID != "" {
		filter = append(filter, map[string]any{"bool": map[string]any{
			"should":               []map[string]any{term("doc_created_by_user_id", q.OwnerUUID), term("doc_owner_user_id", q.OwnerUUID)},
			"minimum_should_match": 1,
		}})
	}
	body := map[string]any{
		"size": SearchCandidates,
		// Counted far enough to say whether there were more than were read.
		"track_total_hits": SearchCandidates + 1,
		// The body is searched, never sent back: a hit shows its passages.
		"_source": []string{"doc_uuid", "doc_title", "doc_snippet"},
		"highlight": map[string]any{
			// The passages are text: the index escapes them, and adds the marks.
			"encoder":   "html",
			"pre_tags":  []string{"<mark>"},
			"post_tags": []string{"</mark>"},
			"fields": map[string]any{
				"doc_body":  map[string]any{"fragment_size": FragmentSize, "number_of_fragments": Fragments},
				"doc_title": map[string]any{"number_of_fragments": 0},
			},
		},
		"query": map[string]any{
			"bool": map[string]any{
				"filter": filter,
				"must": []map[string]any{{
					"bool": map[string]any{
						"should": []map[string]any{
							{"multi_match": map[string]any{"query": text, "fields": []string{"doc_title^5", "doc_body", "doc_created_by_user_full_name^2"}}},
							{"match_phrase_prefix": map[string]any{"doc_title": map[string]any{"query": text, "boost": 10.0}}},
						},
						"minimum_should_match": 1,
					},
				}},
				"must_not": []map[string]any{
					{"exists": map[string]any{"field": "deleted_date"}},
					// A table row's body is the table's, never a doc of its own.
					term("doc_space_kind", "table"),
				},
			},
		},
	}
	b, err := json.Marshal(body)
	return string(b), err
}

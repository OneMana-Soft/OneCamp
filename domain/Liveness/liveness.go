// Package liveness answers "does this content still exist?" for search
// results, whose index can outlive what it indexes.
//
// WHY. OpenSearch and the databases change apart. A restore puts Postgres and
// Dgraph back to a snapshot and leaves the indices as they were; a delete that
// fails half way removes the row and leaves the entry. Both AI answers and
// global search read the index, so both check their results here before
// anyone sees them. Content that is gone is dropped (and its entry can be
// forgotten); content that is soft-deleted is dropped but its entry kept,
// because an admin can restore it. A check that cannot run keeps its results:
// a stale link is better than no answer.
//
// DELETED MEANS A DELETION TIME AFTER 1970, in both stores. A live doc is
// written with a zero doc_deleted_at (0001-01-01), not without one, and a
// restored doc is given the zero time back; the rest of the code base filters
// with gt(doc_deleted_at, 1970) for that reason. Postgres rows are held to the
// same rule so either convention reads right.
package liveness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/lib/pq"
)

// Checker says, for each id it knows, whether the content is live (true) or
// soft-deleted (false). An id it does not return does not exist.
type Checker func(ctx context.Context, ids []string) (map[string]bool, error)

// Checkers covers every content type whose rows can disappear, keyed by the
// type names search results carry. A type not listed here is never filtered.
var Checkers = map[string]Checker{
	"post":    postgres("posts"),
	"chat":    postgres("chats"),
	"comment": postgres("comments"),
	"task":    postgres("tasks"),
	"doc":     dgraphDocs,
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Key names an item's content: its type and id.
type Key[T any] func(T) (contentType, id string)

// States checks every item of a checked type, one query per type. A type
// whose check fails is left out, so its items are kept.
func States[T any](ctx context.Context, checkers map[string]Checker, items []T, key Key[T]) map[string]map[string]bool {
	ids := make(map[string][]string)
	for _, it := range items {
		t, id := key(it)
		if _, checked := checkers[t]; checked && uuidPattern.MatchString(id) {
			ids[t] = append(ids[t], strings.ToLower(id))
		}
	}
	states := make(map[string]map[string]bool, len(ids))
	for t, list := range ids {
		state, err := checkers[t](ctx, list)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "liveness: could not check whether %d %s results still exist, keeping them: %v", len(list), t, err)
			continue
		}
		states[t] = state
	}
	return states
}

// Partition splits items by what states says about them: those to show, and
// those whose content does not exist at all. An item of a type with no state
// (unchecked, or its check failed) is kept; a soft-deleted one is in neither.
// Pure.
func Partition[T any](items []T, key Key[T], states map[string]map[string]bool) (kept, gone []T) {
	kept = make([]T, 0, len(items))
	for _, it := range items {
		t, id := key(it)
		state, checked := states[t]
		if !checked || !uuidPattern.MatchString(id) {
			kept = append(kept, it)
			continue
		}
		live, exists := state[strings.ToLower(id)]
		switch {
		case !exists:
			gone = append(gone, it)
		case live:
			kept = append(kept, it)
		}
	}
	return kept, gone
}

// postgres reads a table keyed by a uuid id with a deleted_at column. table
// only ever comes from Checkers, never from input.
func postgres(table string) Checker {
	query := fmt.Sprintf(`SELECT id::text, COALESCE(deleted_at <= TIMESTAMPTZ 'epoch', TRUE) FROM %s WHERE id = ANY($1::uuid[])`, table)
	return func(ctx context.Context, ids []string) (map[string]bool, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return nil, fmt.Errorf("no database connection")
		}
		rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, pq.Array(ids))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := make(map[string]bool, len(ids))
		for rows.Next() {
			var id string
			var live sql.NullBool
			if err := rows.Scan(&id, &live); err != nil {
				return nil, err
			}
			out[strings.ToLower(id)] = live.Valid && live.Bool
		}
		return out, rows.Err()
	}
}

// DocQuery asks Dgraph which of ids are docs, and which of those are deleted,
// with the same filter the rest of the code base uses. ids must already be
// uuids; anything else is left out. Pure.
func DocQuery(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if uuidPattern.MatchString(id) {
			quoted = append(quoted, `"`+id+`"`)
		}
	}
	list := `[` + strings.Join(quoted, ", ") + `]`
	return `{ docs(func: eq(doc_uuid, ` + list + `)) { doc_uuid }` +
		` deleted(func: eq(doc_uuid, ` + list + `)) @filter(gt(doc_deleted_at, "1970-01-01T00:00:00Z")) { doc_uuid } }`
}

// DocStates turns the two lists into liveness: every doc found, live unless it
// is among the deleted. Pure.
func DocStates(found, deleted []string) map[string]bool {
	out := make(map[string]bool, len(found))
	for _, id := range found {
		out[strings.ToLower(id)] = true
	}
	for _, id := range deleted {
		out[strings.ToLower(id)] = false
	}
	return out
}

func dgraphDocs(ctx context.Context, ids []string) (map[string]bool, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, fmt.Errorf("no graph connection")
	}
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().Query(ctx, DocQuery(ids))
	if err != nil {
		return nil, err
	}
	type row struct {
		UUID string `json:"doc_uuid"`
	}
	var parsed struct {
		Docs    []row `json:"docs"`
		Deleted []row `json:"deleted"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return nil, err
	}
	uuidsOf := func(rows []row) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.UUID
		}
		return out
	}
	return DocStates(uuidsOf(parsed.Docs), uuidsOf(parsed.Deleted)), nil
}

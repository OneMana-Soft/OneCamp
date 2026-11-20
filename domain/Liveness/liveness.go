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
	"sort"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/lib/pq"
)

// Checker says, for each id it knows, whether the content is live (true) or
// soft-deleted (false). An id it does not return does not exist.
type Checker func(ctx context.Context, ids []string) (map[string]bool, error)

// Checkers and Tables cover every content type whose rows can disappear,
// keyed by the type names search results carry: Checkers what lives outside
// Postgres, Tables what lives in it. A type in neither is never filtered.
var Checkers = map[string]Checker{
	"doc": dgraphDocs,
}

// Tables are the kinds kept in Postgres, by table. States checks every one
// of them a request names in one statement (UnionQuery), each table with its
// own deletion predicate; a kind that also has a Checker (a test's) is
// checked by that instead.
var Tables = map[string]string{
	"post":    "posts",
	"chat":    "chats",
	"comment": "comments",
	"task":    "tasks",
	"project": "projects",
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
		if _, checked := checkers[t]; (checked || Tables[t] != "") && uuidPattern.MatchString(id) {
			ids[t] = append(ids[t], strings.ToLower(id))
		}
	}
	states := make(map[string]map[string]bool, len(ids))
	inTables := map[string][]string{}
	for t, list := range ids {
		if check, own := checkers[t]; own {
			state, err := check(ctx, list)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "liveness: could not check whether %d %s results still exist, keeping them: %v", len(list), t, err)
				continue
			}
			states[t] = state
		} else {
			inTables[t] = list
		}
	}
	if len(inTables) == 0 {
		return states
	}
	found, err := checkTables(ctx, inTables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "liveness: could not check whether results in %d tables still exist, keeping them: %v", len(inTables), err)
		return states
	}
	for t := range inTables {
		states[t] = found[t]
		if states[t] == nil {
			states[t] = map[string]bool{}
		}
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

// UnionQuery is one statement that says, for every table named, which of
// its ids still have a row and whether each is live (not soft-deleted): a
// row of (kind, id, live) per id found, each table's arm with its own = ANY
// list (the arguments, in the order of kinds) and deletion predicate.
func UnionQuery(kinds []string) string {
	arms := make([]string, len(kinds))
	for i, k := range kinds {
		arms[i] = fmt.Sprintf(`SELECT %s::text, id::text, COALESCE(deleted_at <= TIMESTAMPTZ 'epoch', TRUE) FROM %s WHERE id = ANY($%d::uuid[])`,
			pq.QuoteLiteral(k), pq.QuoteIdentifier(Tables[k]), i+1)
	}
	return strings.Join(arms, " UNION ALL ")
}

// checkTables is UnionQuery run for the ids of each kind kept in a table:
// by kind, each found id and whether it is live.
func checkTables(ctx context.Context, ids map[string][]string) (map[string]map[string]bool, error) {
	kinds := make([]string, 0, len(ids))
	for k := range ids {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = pq.Array(ids[k])
	}
	type found struct {
		kind, id string
		live     sql.NullBool
	}
	rows, err := pgexec.Query(ctx, UnionQuery(kinds), func(r pgexec.Row) (found, error) {
		var f found
		err := r.Scan(&f.kind, &f.id, &f.live)
		return f, err
	}, args...)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(kinds))
	for _, f := range rows {
		if out[f.kind] == nil {
			out[f.kind] = map[string]bool{}
		}
		out[f.kind][strings.ToLower(f.id)] = f.live.Valid && f.live.Bool
	}
	return out, nil
}

// DocQuery asks Dgraph which of ids are docs, and which of those are deleted,
// with the same filter the rest of the code base uses. ids must already be
// uuids; anything else is left out. Pure.
func DocQuery(ids []string) string {
	list := `[` + dgraphquery.UUIDList(ids) + `]`
	return `{ docs(func: eq(doc_uuid, ` + list + `)) { doc_uuid }` +
		` deleted(func: eq(doc_uuid, ` + list + `)) @filter(` + dgraphquery.Deleted("doc_deleted_at") + `) { doc_uuid } }`
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

// Package dgexec is the one way a model runs DQL: the domain writes the query
// (and the mutations), and these generics run it.
//
// WHY. models/dgraph held 86 hand-written executors and domain 33 more, each
// opening its own transaction. 77 opened a read-write NewTxn against 4
// NewReadOnlyTxn, 32 of the reads never discarded theirs, and 36 committed
// with CommitNow straight on a txn, so a write that lost a race to another
// (Dgraph's "please retry") simply failed. Here:
//
//   - a read runs in a read-only transaction that is always discarded;
//   - a write runs through dgraphInit.DoCommitNow, which tries an aborted
//     write again in a fresh transaction;
//   - an answer decodes straight into the caller's type.
//
// An upsert names its query's variables; Var writes one as a mutation's
// uid ("uid(task)"), so a domain can keep its variable names as constants
// rather than spelling the string in every business function.
package dgexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// ErrNotFound is a First whose block came back empty.
var ErrNotFound = errors.New("dgexec: not found")

// Read runs a read-only query and decodes its answer into a T: a struct with
// a field for each of the query's blocks, by its JSON name.
func Read[T any](ctx context.Context, q string, vars map[string]string) (T, error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	return readTxn[T](ctx, txn, q, vars)
}

// readTxn is Read in txn.
func readTxn[T any](ctx context.Context, txn *dgo.Txn, q string, vars map[string]string) (T, error) {
	var out T
	resp, err := txn.QueryWithVars(ctx, q, vars)
	if err != nil {
		return out, err
	}
	if len(resp.GetJson()) == 0 {
		return out, nil
	}
	err = json.Unmarshal(resp.GetJson(), &out)
	return out, err
}

// List reads the nodes of one block of a read-only query.
func List[T any](ctx context.Context, q string, vars map[string]string, block string) ([]T, error) {
	raw, err := Read[map[string]json.RawMessage](ctx, q, vars)
	if err != nil {
		return nil, err
	}
	b, ok := raw[block]
	if !ok || len(b) == 0 {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("dgexec: block %q: %w", block, err)
	}
	return out, nil
}

// UUIDPage reads one page of the values of pred, the uuid every node of a
// kind holds (doc_uuid, board_uuid), over every node that has it, in the
// graph's order: first of them after offset, of those filter keeps (a
// @filter's inside; "" keeps every one). For a reader that walks every node
// of a kind a page at a time, as the workspace export and the docs' search
// repair do.
func UUIDPage(ctx context.Context, pred, filter string, offset, first int) ([]string, error) {
	at := ""
	if filter != "" {
		at = " @filter(" + filter + ")"
	}
	rows, err := List[map[string]string](ctx, fmt.Sprintf(`{
		page(func: has(%s), first: %d, offset: %d)%s {
			%s
		}
	}`, pred, first, offset, at, pred), map[string]string{}, "page")
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r[pred]
	}
	return ids, nil
}

// First reads the first node of one block of a read-only query: ErrNotFound
// when the block is empty.
func First[T any](ctx context.Context, q string, vars map[string]string, block string) (*T, error) {
	all, err := List[T](ctx, q, vars, block)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, ErrNotFound
	}
	return &all[0], nil
}

// Mutation is one mutation of a request: Set and Delete are values to marshal
// as JSON (a struct, a map, a slice of them), or the N-Quads; Cond is its
// @if(...) condition over the query's variables.
type Mutation struct {
	Set       any
	Delete    any
	SetNQuads string
	DelNQuads string
	Cond      string
}

// build is m as Dgraph takes it.
func (m Mutation) build() (*api.Mutation, error) {
	out := &api.Mutation{SetNquads: []byte(m.SetNQuads), DelNquads: []byte(m.DelNQuads), Cond: m.Cond}
	if m.Set != nil {
		b, err := json.Marshal(m.Set)
		if err != nil {
			return nil, fmt.Errorf("dgexec: marshal set: %w", err)
		}
		out.SetJson = b
	}
	if m.Delete != nil {
		b, err := json.Marshal(m.Delete)
		if err != nil {
			return nil, fmt.Errorf("dgexec: marshal delete: %w", err)
		}
		out.DeleteJson = b
	}
	return out, nil
}

// request is the request Mutate sends: q (an upsert's query block, or "")
// with vars, and the mutations.
func request(q string, vars map[string]string, muts ...Mutation) (*api.Request, error) {
	req := &api.Request{Query: q, Vars: vars, CommitNow: true}
	for _, m := range muts {
		am, err := m.build()
		if err != nil {
			return nil, err
		}
		req.Mutations = append(req.Mutations, am)
	}
	return req, nil
}

// Mutate commits the mutations (after q, for an upsert) in one transaction,
// trying again in a fresh one when Dgraph aborts it for a concurrent write.
func Mutate(ctx context.Context, q string, vars map[string]string, muts ...Mutation) (*api.Response, error) {
	req, err := request(q, vars, muts...)
	if err != nil {
		return nil, err
	}
	return dgraphInit.DoCommitNow(ctx, req)
}

// Set commits one set mutation of v (a node, or a slice of them) and returns
// the uids Dgraph gave its blank nodes, by name.
func Set(ctx context.Context, v any) (map[string]string, error) {
	res, err := Mutate(ctx, "", nil, Mutation{Set: v})
	if err != nil {
		return nil, err
	}
	return res.GetUids(), nil
}

// Upsert runs q, then sets v (whose uids name q's variables, as Var writes
// them), in one transaction, and returns the uids of any new blank nodes.
func Upsert(ctx context.Context, q string, vars map[string]string, v any) (map[string]string, error) {
	res, err := Mutate(ctx, q, vars, Mutation{Set: v})
	if err != nil {
		return nil, err
	}
	return res.GetUids(), nil
}

// Var is an upsert variable as a mutation's uid: Var("task") is "uid(task)".
func Var(name string) string { return "uid(" + name + ")" }

// Txn is one read-write transaction InTxn hands its function: what it reads
// (ReadIn) and what it writes (Mutate) commit together or not at all.
type Txn struct {
	ctx context.Context
	txn *dgo.Txn
}

// InTxn runs fn in one read-write transaction and commits what it wrote, so
// what fn read and decided on is still true when its write lands. When Dgraph
// aborts the commit because a concurrent transaction wrote the same thing, fn
// runs again in a fresh transaction and sees that write
// (dgraphInit.InTxn). An error from fn is returned as it is, with nothing
// written.
func InTxn(ctx context.Context, fn func(tx *Txn) error) error {
	return dgraphInit.InTxn(ctx, func(txn *dgo.Txn) error {
		return fn(&Txn{ctx: ctx, txn: txn})
	})
}

// ReadIn is Read in tx: it sees tx's snapshot and the writes tx has made.
func ReadIn[T any](tx *Txn, q string, vars map[string]string) (T, error) {
	return readTxn[T](tx.ctx, tx.txn, q, vars)
}

// Mutate runs the mutations (after q, for an upsert) in tx, to commit when
// InTxn's function returns nil.
func (tx *Txn) Mutate(q string, vars map[string]string, muts ...Mutation) (*api.Response, error) {
	req, err := request(q, vars, muts...)
	if err != nil {
		return nil, err
	}
	req.CommitNow = false
	return tx.txn.Do(tx.ctx, req)
}

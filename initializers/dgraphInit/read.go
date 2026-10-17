package dgraphInit

import (
	"context"
	"encoding/json"

	"github.com/dgraph-io/dgo/v230"
)

// ReadInto runs a read-only query and decodes its answer into out: a pointer
// to a struct with a field for each of the query's blocks, by its name.
func ReadInto(ctx context.Context, query string, vars map[string]string, out any) error {
	return ReadIntoTxn(ctx, DgraphClient.NewReadOnlyTxn(), query, vars, out)
}

// ReadIntoTxn is ReadInto in txn, for a read made of several queries that must
// see one snapshot (InSnapshot).
func ReadIntoTxn(ctx context.Context, txn *dgo.Txn, query string, vars map[string]string, out any) error {
	resp, err := txn.QueryWithVars(ctx, query, vars)
	if err != nil {
		return err
	}
	return json.Unmarshal(resp.GetJson(), out)
}

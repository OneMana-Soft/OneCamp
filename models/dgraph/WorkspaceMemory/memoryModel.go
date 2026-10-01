package models

// Dgraph projection of workspace-memory items (GraphRAG).
//
// Postgres is the system of record; this package mirrors each memory item
// into Dgraph as a linked node so the structured knowledge layer is
// queryable as a graph: memory ↔ owner (User), ↔ channel (Channel), ↔
// project (Project). That unlocks relationship questions the vector/SQL
// stores can't answer cheaply — "open commitments owned by @x", "every
// decision touching this project", "who owns the most overdue items".
//
// All writes are upserts keyed by mem_uuid (the Postgres row id), so
// re-projection is idempotent. Links are resolved in the SAME upsert via
// query vars matching owner/channel/project by their uuids — if a linked
// node doesn't exist yet the edge is simply omitted (best-effort; the graph
// is an index, not the source of truth).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

// UpsertMemoryItem creates/updates a MemoryItem node and its owner/channel/
// project edges in a single transaction. ownerUUID/channelUUID/projectUUID
// may be empty; empty ones are not linked. The node is matched by mem_uuid
// so repeated calls update in place.
func UpsertMemoryItem(ctx context.Context, item *dgraphStruct.DgraphMemoryItem,
	ownerUUID, channelUUID, projectUUID string) error {
	if item == nil || item.Uuid == "" {
		return fmt.Errorf("memory item uuid required")
	}

	// Build the mutation JSON as a map so we have absolute control over
	// which edges are emitted. Struct tags / omitempty behaviour caused
	// subtle mismatches where a query variable was defined but the edge
	// did not appear in the marshaled JSON, triggering Dgraph's
	// "Some variables are defined but not used" error.
	mut := map[string]interface{}{
		"uid":            "uid(m)",
		"dgraph.type":    []string{"MemoryItem"},
		"mem_uuid":       item.Uuid,
		"mem_kind":       item.Kind,
		"mem_content":    item.Content,
		"mem_status":     item.Status,
		"mem_confidence": item.Confidence,
		"mem_grp_id":     item.GrpID,
	}
	if item.DueAt != nil {
		mut["mem_due_at"] = item.DueAt
	}
	if item.CreatedAt != nil {
		mut["mem_created_at"] = item.CreatedAt
	}
	if item.UpdatedAt != nil {
		mut["mem_updated_at"] = item.UpdatedAt
	}

	// Build the query dynamically: always include `m` (the memory node),
	// but only add o/c/p when their UUID is non-empty.  Keep the mutation
	// map and the query in perfect sync so Dgraph never sees an unused
	// variable.
	var qb strings.Builder
	qb.WriteString("query {\n")
	fmt.Fprintf(&qb, "\t\tm as var(func: eq(mem_uuid, \"%s\"))\n", item.Uuid)
	if ownerUUID != "" {
		fmt.Fprintf(&qb, "\t\to as var(func: eq(user_uuid, \"%s\"))\n", ownerUUID)
		mut["mem_owner"] = map[string]string{"uid": "uid(o)"}
	}
	if channelUUID != "" {
		fmt.Fprintf(&qb, "\t\tc as var(func: eq(ch_uuid, \"%s\"))\n", channelUUID)
		mut["mem_channel"] = map[string]string{"uid": "uid(c)"}
	}
	if projectUUID != "" {
		fmt.Fprintf(&qb, "\t\tp as var(func: eq(project_uuid, \"%s\"))\n", projectUUID)
		mut["mem_project"] = map[string]string{"uid": "uid(p)"}
	}
	qb.WriteString("\t}")
	query := qb.String()

	pb, err := json.Marshal(mut)
	if err != nil {
		return fmt.Errorf("marshal memory mutation: %w", err)
	}

	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() {
		if derr := txn.Discard(ctx); derr != nil {
			helpers.LogErrorWithContext(ctx, "models/dgraph/UpsertMemoryItem discard err: %+v", derr)
		}
	}()

	_, err = txn.Do(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: pb}},
		CommitNow: true,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/dgraph/UpsertMemoryItem failed: %+v", err)
		return err
	}
	return nil
}

// SetMemoryItemStatus updates just the status (and updated_at) of a
// MemoryItem node by mem_uuid. Used when a user resolves/dismisses an item
// so graph "open items" reads stay accurate. No-op if the node is absent.
func SetMemoryItemStatus(ctx context.Context, memUUID, status string) error {
	if memUUID == "" || status == "" {
		return nil
	}
	query := fmt.Sprintf(`query { m as var(func: eq(mem_uuid, "%s")) }`, memUUID)
	node := &dgraphStruct.DgraphMemoryItem{Uid: "uid(m)", Status: status}
	pb, err := json.Marshal(node)
	if err != nil {
		return err
	}

	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()

	_, err = txn.Do(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{SetJson: pb}},
		CommitNow: true,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/dgraph/SetMemoryItemStatus failed: %+v", err)
		return err
	}
	return nil
}

// DeleteMemoryItem removes a MemoryItem node (and its outbound edges) by
// mem_uuid. Used by the deletion cascade so deleted source content leaves
// no trace in the graph. No-op if the node doesn't exist.
func DeleteMemoryItem(ctx context.Context, memUUID string) error {
	if memUUID == "" {
		return nil
	}
	query := fmt.Sprintf(`query { m as var(func: eq(mem_uuid, "%s")) }`, memUUID)
	// Delete the whole node: S * * removes all predicates + the node.
	delJSON := `{"uid": "uid(m)"}`

	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() {
		if derr := txn.Discard(ctx); derr != nil {
			helpers.LogErrorWithContext(ctx, "models/dgraph/DeleteMemoryItem discard err: %+v", derr)
		}
	}()

	_, err := txn.Do(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{{DeleteJson: []byte(delJSON)}},
		CommitNow: true,
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/dgraph/DeleteMemoryItem failed: %+v", err)
		return err
	}
	return nil
}

// ExecScopeOpenItems executes a caller-built scope-anchored open-items query
// (see domain/WorkspaceMemory.QueryScopeOpenItems) and flattens the results.
// reverseEdge is the reverse predicate the items nest under in the JSON
// ("~mem_channel" / "~mem_project"), passed alongside the query so this model
// only executes + parses.
func ExecScopeOpenItems(ctx context.Context, query string, variables map[string]string, reverseEdge string) ([]*dgraphStruct.DgraphMemoryItem, error) {
	if query == "" {
		return nil, nil
	}
	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/dgraph/ExecScopeOpenItems failed: %+v", err)
		return nil, err
	}

	// The reverse edge key in JSON is the literal predicate name.
	var raw struct {
		Anchor []map[string]json.RawMessage `json:"anchor"`
	}
	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		return nil, err
	}
	var out []*dgraphStruct.DgraphMemoryItem
	for _, node := range raw.Anchor {
		rawItems, ok := node[reverseEdge]
		if !ok {
			continue
		}
		var items []*dgraphStruct.DgraphMemoryItem
		if err := json.Unmarshal(rawItems, &items); err != nil {
			continue
		}
		out = append(out, items...)
	}
	return out, nil
}

// ExecOwnedOpenItems executes a caller-built owner-anchored open-items query
// (see domain/WorkspaceMemory.QueryOwnedOpenItems) and flattens the results
// from under the reverse mem_owner edge. Execute + parse only.
func ExecOwnedOpenItems(ctx context.Context, query string, variables map[string]string) ([]*dgraphStruct.DgraphMemoryItem, error) {
	if query == "" {
		return nil, nil
	}
	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()

	resp, err := txn.QueryWithVars(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/dgraph/ExecOwnedOpenItems failed: %+v", err)
		return nil, err
	}

	// The reverse edge nests items under a wrapper; decode then flatten.
	var raw struct {
		Items []struct {
			Owned []*dgraphStruct.DgraphMemoryItem `json:"~mem_owner"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Json, &raw); err != nil {
		return nil, err
	}
	var out []*dgraphStruct.DgraphMemoryItem
	for _, w := range raw.Items {
		out = append(out, w.Owned...)
	}
	return out, nil
}

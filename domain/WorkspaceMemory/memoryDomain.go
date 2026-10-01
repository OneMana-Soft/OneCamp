package domain

// Workspace-memory GraphRAG reads — domain layer.
//
// Follows the repo's Dgraph convention: the QUERY is written here (domain) and
// EXECUTED by the model (models/dgraph/WorkspaceMemory). These are the
// scope-anchored and owner-anchored "open items" reads powering the memory
// panel, the team-report agent, and the assistant's memory grounding. Memory
// mutations (upserts / status) stay in the model, as every write does.

import (
	"context"
	"fmt"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	memDgraph "github.com/akashc777/OneCamp/models/dgraph/WorkspaceMemory"
)

// QueryOwnedOpenItems returns OPEN memory items owned by a user (by user uuid),
// optionally filtered to a single kind (empty = any), bounded by limit. It
// traverses the reverse mem_owner edge (GraphRAG) and returns each item's
// linked channel/project so callers can render "where" without extra lookups.
func QueryOwnedOpenItems(ctx context.Context, ownerUUID, kind string, limit int) ([]*dgraphStruct.DgraphMemoryItem, error) {
	if ownerUUID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return memDgraph.ExecOwnedOpenItems(ctx, buildOwnedOpenItemsQuery(kind, limit), map[string]string{"$owner": ownerUUID})
}

// buildOwnedOpenItemsQuery builds the owner-anchored open-items DQL. Pure +
// unit-tested. A SINGLE @filter is used (status always "open", plus kind when
// requested): two @filter directives on one block is a DQL syntax error, so
// they MUST be combined with AND. Order by mem_created_at (set on every node)
// rather than mem_due_at — Dgraph's sort DROPS nodes lacking the sort
// predicate, and questions have no due date.
func buildOwnedOpenItemsQuery(kind string, limit int) string {
	filter := `eq(mem_status, "open")`
	if kind != "" {
		filter = fmt.Sprintf(`eq(mem_status, "open") AND eq(mem_kind, "%s")`, kind)
	}
	return fmt.Sprintf(`query q($owner: string) {
		items(func: eq(user_uuid, $owner)) {
			~mem_owner @filter(%s) (first: %d, orderdesc: mem_created_at) {
				uid
				mem_uuid
				mem_kind
				mem_content
				mem_status
				mem_confidence
				mem_due_at
				mem_grp_id
				mem_created_at
				mem_channel { ch_uuid ch_name }
				mem_project { project_uuid project_name }
			}
		}
	}`, filter, limit)
}

// QueryScopeOpenItems returns OPEN memory items linked to a channel or project
// (by uuid), traversing the reverse mem_channel / mem_project edge. Each item
// carries its owner (via mem_owner) for the accountability rollup. scopeType is
// "channel" or "project". Bounded by limit.
func QueryScopeOpenItems(ctx context.Context, scopeType, scopeUUID string, limit int) ([]*dgraphStruct.DgraphMemoryItem, error) {
	if scopeUUID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 300 {
		limit = 100
	}

	var anchorPred, reverseEdge string
	switch scopeType {
	case "channel":
		anchorPred, reverseEdge = "ch_uuid", "~mem_channel"
	case "project":
		anchorPred, reverseEdge = "project_uuid", "~mem_project"
	default:
		return nil, fmt.Errorf("unsupported scope type: %s", scopeType)
	}

	// reverseEdge is also the JSON key the model reads items back from.
	query := buildScopeOpenItemsQuery(anchorPred, reverseEdge, limit)
	return memDgraph.ExecScopeOpenItems(ctx, query, map[string]string{"$scope": scopeUUID}, reverseEdge)
}

// buildScopeOpenItemsQuery builds the scope-anchored open-items DQL: anchor on
// the channel/project node, walk the reverse memory edge to its open items, and
// pull each item's owner for the accountability rollup. orderdesc by
// mem_created_at keeps every item (created_at is always set; due_at is not).
// Pure + unit-tested.
func buildScopeOpenItemsQuery(anchorPred, reverseEdge string, limit int) string {
	return fmt.Sprintf(`query q($scope: string) {
		anchor(func: eq(%s, $scope)) {
			%s @filter(eq(mem_status, "open")) (first: %d, orderdesc: mem_created_at) {
				uid
				mem_uuid
				mem_kind
				mem_content
				mem_status
				mem_confidence
				mem_due_at
				mem_grp_id
				mem_created_at
				mem_owner { user_uuid user_name user_full_name }
				mem_channel { ch_uuid ch_name }
				mem_project { project_uuid project_name }
			}
		}
	}`, anchorPred, reverseEdge, limit)
}

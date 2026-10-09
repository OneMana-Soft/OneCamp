package domain

import (
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The upsert's query defines exactly the variables its mutation names: a new
// post (po_N), a 1:1 conversation and its chat (dm_N, ch_N), and a chat added
// to an existing group conversation (grpch_N, the conversation named by its
// uid). The query used to say post_N and never grpch_N, so the graph refused
// every forward into a channel or a group chat.
func TestAForwardsQueryDefinesWhatItsMutationNames(t *testing.T) {
	posts := []*dgraphStruct.DgraphPost{{Uid: "uid(po_0)", Uuid: "p-0"}}
	dms := []*dgraphStruct.DgraphDm{
		{Uid: "uid(dm_0)", GroupingId: "g-1to1", Chats: []*dgraphStruct.DgraphChat{{Uid: "uid(ch_0)", Uuid: "c-0"}}},
		{Uid: "0x2a", GroupingId: "g-group", Chats: []*dgraphStruct.DgraphChat{{Uid: "uid(grpch_1)", Uuid: "c-1"}}},
	}
	q := forwardUpsertQuery(posts, dms)
	for _, want := range []string{
		`dm_0 as var(func: eq(dm_grouping_id, "g-1to1"))`,
		`ch_0 as var(func: eq(chat_uuid, "c-0"))`,
		`grpch_1 as var(func: eq(chat_uuid, "c-1"))`,
		`po_0 as var(func: eq(post_uuid, "p-0"))`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s:\n%s", want, q)
		}
	}
	// The group conversation is named by its uid: nothing to define, and an
	// unused variable is an error to the graph.
	if strings.Contains(q, "g-group") || strings.Count(q, " as var(") != 4 {
		t.Errorf("query defines more than the mutation names:\n%s", q)
	}
}

func TestAForwardsQueryQuotesItsValues(t *testing.T) {
	q := forwardUpsertQuery([]*dgraphStruct.DgraphPost{{Uid: "uid(po_0)", Uuid: `x") { evil(func: has(user_uuid)) { user_uuid } } #`}}, nil)
	if strings.Contains(q, "evil(func") && !strings.Contains(q, `\"`) {
		t.Fatalf("a value went into the query bare:\n%s", q)
	}
}
